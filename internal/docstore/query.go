// Copyright 2019 The Go Cloud Authors
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

package docstore

import (
	"context"
	"io"

	"gocloud.dev/internal/docstore/driver"
)

// Query represents a query over a collection.
type Query struct {
	coll *Collection
	dq   *driver.Query
}

// Query creates a new Query over the collection.
func (c *Collection) Query() *Query {
	return &Query{coll: c, dq: &driver.Query{}}
}

// Where expresses a condition on the query.
// Valid ops are: "=", ">", "<", ">=", "<=".
func (q *Query) Where(field, op string, value interface{}) *Query {
	q.dq.Filters = append(q.dq.Filters, driver.Filter{
		Field: field,
		Op:    op,
		Value: value,
	})
	return q
}

// Limit will limit the results to at most n documents.
func (q *Query) Limit(n int) *Query {
	q.dq.Limit = n
	return q
}

// Get returns an iterator for retrieving the documents specified by the query. If
// field paths are provided, only those paths are set in the resulting documents.
//
// Call Stop on the iterator when finished.
func (q *Query) Get(ctx context.Context, fieldpaths ...string) *DocumentIterator {
	_ = q.coll.driver.RunQuery(ctx, q.dq)
	return &DocumentIterator{iter: q.dq.Iter}
}

// DocumentIterator iterates over documents.
// Call Stop on the iterator when finished.
type DocumentIterator struct {
	iter driver.DocumentIterator
	err  error
}

// Next stores the next document in dst. It returns io.EOF if there are no more
// documents.
// Once Next returns an error, it will always return the same error.
func (it *DocumentIterator) Next(ctx context.Context, dst Document) error {
	if it.err != nil {
		return it.err
	}
	ddoc, err := driver.NewDocument(dst)
	if err != nil {
		it.err = err
		return err
	}
	it.err = it.iter.Next(ctx, ddoc)
	return it.err
}

// Stop stops the iterator, the state of the iterator will be lost and calling
// Next will return io.EOF.
func (it *DocumentIterator) Stop() {
	it.err = io.EOF
	it.iter.Stop()
}
