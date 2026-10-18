package sqlite

import (
	"context"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/scopeaudit"
)

// InspectScopes streams invalid stored identities to an operator callback.
// This administrative scan crosses all scopes and never changes data.
func (s *Store) InspectScopes(ctx context.Context, visit func(store.ScopeIssue) error) error {
	return scopeaudit.SQL(ctx, s.sdb, `SELECT m.name, p.name FROM sqlite_master AS m JOIN pragma_table_info(m.name) AS p WHERE m.type = 'table' AND m.name LIKE 'cortex\_%' ESCAPE '\'`, visit)
}
