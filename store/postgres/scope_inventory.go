package postgres

import (
	"context"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/scopeaudit"
)

// InspectScopes streams invalid stored identities to an operator callback.
// This administrative scan crosses all scopes and never changes data.
func (s *Store) InspectScopes(ctx context.Context, visit func(store.ScopeIssue) error) error {
	return scopeaudit.SQL(ctx, s.pgdb, `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name LIKE 'cortex\_%' ESCAPE '\'`, visit)
}
