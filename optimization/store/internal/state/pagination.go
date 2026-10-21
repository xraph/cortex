package state

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

type resourceCursor struct {
	Version    int       `json:"version"`
	Scope      string    `json:"scope"`
	Collection string    `json:"collection"`
	Filter     string    `json:"filter"`
	AfterTime  time.Time `json:"after_time"`
	AfterID    string    `json:"after_id"`
}

type pageRow[T any] struct {
	id      string
	created time.Time
	value   T
}

func selectRows[T any](values map[string]T, include func(T) bool, created func(T) time.Time) []pageRow[T] {
	rows := make([]pageRow[T], 0, len(values))
	for key, value := range values {
		if include(value) {
			rows = append(rows, pageRow[T]{key, created(value), value})
		}
	}
	return rows
}

func readPage[T any](ctx context.Context, s *Store, collection string, parent id.ID, filter any, query optimization.PageQuery, collect func(*scopeData) ([]pageRow[T], error)) (optimization.Page[T], error) {
	page := optimization.Page[T]{Items: []T{}}
	scope, err := contextScope(ctx)
	if err != nil {
		return page, err
	}
	limit, err := pageLimit(query.Limit)
	if err != nil {
		return page, err
	}
	if !parent.IsNil() {
		if _, err = optimization.ParseID(parent.String(), optimization.PrefixExperiment); err != nil {
			return page, err
		}
	}
	encoded, err := canonical.Marshal(struct {
		Parent string
		Filter any
	}{parent.String(), filter})
	if err != nil {
		return page, err
	}
	hash := sha256.Sum256(encoded)
	scopeHash := sha256.Sum256([]byte(scope))
	cursor := resourceCursor{Version: 1, Scope: hex.EncodeToString(scopeHash[:]), Collection: collection, Filter: hex.EncodeToString(hash[:])}
	if query.Cursor != "" {
		// A 512-byte opaque key may expand sixfold under JSON escaping before base64.
		if len(query.Cursor) > 8192 {
			return page, optimization.ErrInvalidSpec
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(query.Cursor)
		if decodeErr != nil {
			return page, optimization.ErrInvalidSpec
		}
		var previous resourceCursor
		if decodeErr = canonical.Decode(data, &previous); decodeErr != nil {
			return page, optimization.ErrInvalidSpec
		}
		if previous.Version != cursor.Version || previous.Scope != cursor.Scope || previous.Collection != collection || previous.Filter != cursor.Filter || previous.AfterID == "" {
			return page, optimization.ErrInvalidSpec
		}
		cursor = previous
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return page, optimization.ErrUnavailable
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return page, contextErr
	}
	data, loadErr := s.loadScope(ctx, scope)
	if loadErr != nil {
		return page, loadErr
	}
	if data == nil {
		data = newScopeData()
	}
	if !parent.IsNil() {
		if _, exists := data.Experiments[parent.String()]; !exists {
			return page, optimization.ErrNotFound
		}
	}
	rows, err := collect(data)
	if err != nil {
		return page, err
	}
	slices.SortFunc(rows, func(a, b pageRow[T]) int {
		if order := a.created.Compare(b.created); order != 0 {
			return order
		}
		return strings.Compare(a.id, b.id)
	})
	for _, row := range rows {
		if query.Cursor != "" && (row.created.Before(cursor.AfterTime) || row.created.Equal(cursor.AfterTime) && row.id <= cursor.AfterID) {
			continue
		}
		if len(page.Items) == limit {
			encodedCursor, encodeErr := canonical.Marshal(cursor)
			if encodeErr != nil {
				return page, encodeErr
			}
			page.NextCursor = base64.RawURLEncoding.EncodeToString(encodedCursor)
			break
		}
		page.Items = append(page.Items, row.value)
		cursor.AfterTime, cursor.AfterID = row.created, row.id
	}
	return clone(page)
}

func readExperimentPage[T any](ctx context.Context, s *Store, collection string, parent id.ID, filter any, query optimization.PageQuery, collect func(*scopeData) ([]pageRow[T], error)) (optimization.Page[T], error) {
	if parent.IsNil() {
		return optimization.Page[T]{Items: []T{}}, optimization.ErrInvalidSpec
	}
	return readPage(ctx, s, collection, parent, filter, query, collect)
}
