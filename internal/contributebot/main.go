// Copyright 2018 The Go Cloud Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// contributebot is a service for keeping the Go Cloud project tidy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/google/go-github/v18/github"
)

const (
	userAgent = "google/go-cloud Contribute Bot"
	cloneURL  = "https://github.com/google/go-cloud.git"
)

type flagConfig struct {
	project      string
	subscription string
	gitHubAppID  int64
	keyPath      string
}

func main() {
	addr := flag.String("listen", ":8080", "address to listen for health checks")
	var cfg flagConfig
	flag.StringVar(&cfg.project, "project", "", "GCP project for topic")
	flag.StringVar(&cfg.subscription, "subscription", "contributebot-github-events", "subscription name inside project")
	flag.Int64Var(&cfg.gitHubAppID, "github_app", 0, "GitHub application ID")
	flag.StringVar(&cfg.keyPath, "github_key", "", "path to GitHub application private key")
	flag.Parse()
	if cfg.project == "" || cfg.gitHubAppID == 0 || cfg.keyPath == "" {
		fmt.Fprintln(os.Stderr, "contributebot: must specify -project, -github_app, and -github_key")
		os.Exit(2)
	}

	ctx := context.Background()
	w, server, cleanup, err := setup(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()
	log.Printf("Serving health checks at %s", *addr)
	go server.ListenAndServe(*addr, w)
	log.Fatal(w.receive(ctx))
}

// worker contains the connections used by this server.
type worker struct {
	sub  *pubsub.Subscription
	auth *gitHubAppAuth
}

// receive listens for events on its subscription and handles them.
func (w *worker) receive(ctx context.Context) error {
	return w.sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		id := msg.Attributes["X-GitHub-Delivery"]
		eventType := msg.Attributes["X-GitHub-Event"]
		if eventType == "integration_installation" || eventType == "integration_installation_repositories" {
			// Deprecated event types. Ignore them in favor of supported ones.
			log.Printf("Skipped event %s of deprecated type %s", id, eventType)
			msg.Ack()
			return
		}
		event, err := github.ParseWebHook(eventType, msg.Data)
		if err != nil {
			log.Printf("Parsing %s event %s: %v", eventType, id, err)
			msg.Nack()
			return
		}
		var handleErr error
		switch event := event.(type) {
		case *github.IssuesEvent:
			handleErr = w.receiveIssueEvent(ctx, event)
		case *github.PullRequestEvent:
			handleErr = w.receivePullRequestEvent(ctx, event)
		case *github.CheckRunEvent:
			handleErr = w.receiveCheckRunEvent(ctx, event)
		case *github.PingEvent, *github.InstallationEvent, *github.CheckSuiteEvent:
			// No-op.
		default:
			log.Printf("Unhandled webhook event type %s (%T) for %s", eventType, event, id)
			msg.Nack()
			return
		}
		if handleErr != nil {
			log.Printf("Failed processing %s event %s: %v", eventType, id, handleErr)
			msg.Nack()
			return
		}
		msg.Ack()
		log.Printf("Processed %s event %s", eventType, id)
	})
}

func (w *worker) receiveIssueEvent(ctx context.Context, e *github.IssuesEvent) error {

	// Pull out the interesting data from the event.
	data := &issueData{
		Action: e.GetAction(),
		Owner:  e.GetRepo().GetOwner().GetLogin(),
		Repo:   e.GetRepo().GetName(),
		Issue:  e.GetIssue(),
		Change: e.GetChanges(),
	}

	// Refetch the issue in case the event data is stale.
	client := w.ghClient(e.GetInstallation().GetID())
	iss, _, err := client.Issues.Get(ctx, data.Owner, data.Repo, data.Issue.GetNumber())
	if err != nil {
		return err
	}
	data.Issue = iss

	// Process the issue, deciding what actions to take (if any).
	edits := processIssueEvent(data)
	// Execute the actions (if any).
	return edits.Execute(ctx, client, data)
}

func (w *worker) receivePullRequestEvent(ctx context.Context, e *github.PullRequestEvent) error {
	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()

	// Pull out the interesting data from the event.
	data := &pullRequestData{
		Action:      e.GetAction(),
		OwnerLogin:  owner,
		Repo:        repo,
		PullRequest: e.GetPullRequest(),
		Change:      e.GetChanges(),
	}

	// Refetch the pull request in case the event data is stale.
	client := w.ghClient(e.GetInstallation().GetID())
	pr, _, err := client.PullRequests.Get(ctx, data.OwnerLogin, data.Repo, data.PullRequest.GetNumber())
	if err != nil {
		return err
	}
	data.PullRequest = pr

	// Fetch the latest commit of the pull request.
	commits, _, err := client.PullRequests.ListCommits(ctx, data.OwnerLogin, data.Repo, e.GetNumber(), nil)
	if err != nil {
		return err
	}
	latest, _, err := client.Repositories.GetCommit(ctx, owner, repo, commits[len(commits)-1].GetSHA())
	if err != nil {
		return err
	}

	// Fetch the check runs for the commit.
	runs, _, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, latest.GetSHA(), nil)
	if err != nil {
		return err
	}
	createCheck := true
	for _, r := range runs.CheckRuns {
		if r.GetHeadSHA() == latest.GetSHA() {
			createCheck = false
		}
	}
	if createCheck {
		if _, _, err := client.Checks.CreateCheckRun(ctx, owner, repo, github.CreateCheckRunOptions{
			Name:      licenseHeaderCheck,
			HeadSHA:   latest.GetSHA(),
			Status:    github.String("in_progress"),
			StartedAt: &github.Timestamp{Time: time.Now()},
		}); err != nil {
			return err
		}
	}

	// Process the pull request, deciding what actions to take (if any).
	edits := processPullRequestEvent(data)
	// Execute the actions (if any).
	return edits.Execute(ctx, client, data)
}

const licenseHeaderCheck = "license-header-check"

func (w *worker) receiveCheckRunEvent(ctx context.Context, e *github.CheckRunEvent) error {
	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()
	cr := e.GetCheckRun()
	if cr.GetStatus() != "in_progress" || cr.GetName() != licenseHeaderCheck {
		return nil
	}
	local, err := fetchCode(ctx, cr.GetHeadSHA())
	if err != nil {
		return err
	}
	fmt.Println("Temp code directory:", local)
	defer os.RemoveAll(local)

	// When receiving an "in_progress" check run, get the commit from the SHA and the files.
	data := &checkRunData{
		CheckRun:   e.GetCheckRun(),
		OwnerLogin: owner,
		Repo:       repo,
		CodeDir:    local,
	}
	client := w.ghClient(e.GetInstallation().GetID())
	if data.Commit, _, err = client.Repositories.GetCommit(ctx, owner, repo, cr.GetHeadSHA()); err != nil {
		return err
	}

	updates, err := processCheckRunEvent(data)
	if err != nil {
		return err
	}
	return updates.Execute(ctx, client, data)
}

// fetchCode fetches the repo for a specific SHA.
func fetchCode(ctx context.Context, sha string) (string, error) {
	dir, err := ioutil.TempDir(os.TempDir(), "repo")
	if err != nil {
		return "", err
	}
	cmds := []*exec.Cmd{
		exec.CommandContext(ctx, "git", "init"),
		exec.CommandContext(ctx, "git", "remote", "add", "origin", cloneURL),
		exec.CommandContext(ctx, "git", "fetch", "origin", sha),
		exec.CommandContext(ctx, "git", "reset", "--hard", "FETCH_HEAD"),
	}
	for _, cmd := range cmds {
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%s: %v", cmd.Args, err)
		}
	}
	return dir, nil
}

// ghClient creates a GitHub client authenticated for the given installation.
func (w *worker) ghClient(installID int64) *github.Client {
	c := github.NewClient(&http.Client{Transport: w.auth.forInstall(installID)})
	c.UserAgent = userAgent
	return c
}

// ServeHTTP serves a page explaining that this port is only open for health checks.
func (w *worker) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(resp, req)
		return
	}
	const responseData = `<!DOCTYPE html>
<title>Go Cloud Contribute Bot Worker</title>
<h1>Go Cloud Contribute Bot Worker</h1>
<p>This HTTP port is only open to serve health checks.</p>`
	resp.Header().Set("Content-Length", fmt.Sprint(len(responseData)))
	resp.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(resp, responseData)
}

func (w *worker) CheckHealth() error {
	return w.auth.CheckHealth()
}
