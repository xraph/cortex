// Package scoped shares exact-scope index construction across storage adapters.
package scoped

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
)

// Index keeps an exact scope alongside its bounded database lookup key.
type Index struct {
	Hash, Canon string
	Levels      [3]string
}

// New validates and indexes the exact context scope.
func New(ctx context.Context, key string) (Index, error) {
	var result Index
	scope := cortex.ScopeFromContext(ctx)
	canonical, err := optimization.ScopeKey(scope)
	if err != nil {
		return result, err
	}
	if canonical != key {
		return result, optimization.ErrPolicyDenied
	}
	digest := sha256.Sum256([]byte(key))
	result.Hash, result.Canon = hex.EncodeToString(digest[:]), key
	for i, level := range scope.Levels {
		result.Levels[i] = level.Key + "=" + level.Value
	}
	return result, nil
}
