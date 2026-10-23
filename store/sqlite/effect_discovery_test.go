package sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/store/internal/assurance"
)

type corruptEffectDiscoveryDriver struct {
	assuranceDriver
	kind    string
	corrupt func(*assurance.Record)
}

func (d corruptEffectDiscoveryDriver) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	rows, err := d.assuranceDriver.Select(ctx, query)
	if err == nil && query.Kind == d.kind && len(rows) > 0 {
		d.corrupt(&rows[0])
	}
	return rows, err
}
func TestEffectDiscoveryRejectsCorruptStoredBindings(t *testing.T) {
	for _, kind := range []string{"effect_request", "effect_receipt"} {
		for _, field := range []string{"id", "digest", "run", "scope", "body", "deleted"} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				s := newTestStore(t)
				r := sqliteEffectRequest()
				ctx := cortex.WithScope(context.Background(), r.Scope)
				_, err := s.BeginEffect(ctx, r)
				require.NoError(t, err)
				require.NoError(t, s.CompleteEffect(ctx, r, fileEffectReceipt(r)))
				s.assuranceStore = assurance.New(corruptEffectDiscoveryDriver{assuranceDriver: assuranceDriver{store: s}, kind: kind, corrupt: func(row *assurance.Record) {
					switch field {
					case "id":
						row.ID = "different"
					case "digest":
						row.Digest = "different"
					case "run":
						row.RunID = "different"
					case "scope":
						row.ScopeL0 = "org=other"
					case "body":
						row.Body = "{}"
					case "deleted":
						row.Deleted = true
					}
				}})
				page, err := s.ListEffects(ctx, execution.EffectListRequest{Limit: 10})
				require.Error(t, err)
				require.Empty(t, page)
			})
		}
	}
}
