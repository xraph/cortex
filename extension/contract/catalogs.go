package contract

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/nexus"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/shield/profile"
	"github.com/xraph/shield/scan"
	"github.com/xraph/weave/collection"
	weaveengine "github.com/xraph/weave/engine"
	weaveid "github.com/xraph/weave/id"
)

// CatalogScope is resolved by the host from the Cortex principal and scope.
// External identifiers need not equal Cortex hierarchy values. Empty identifiers
// are refused, because those services interpret empty filters as every tenant.
type CatalogScope struct {
	AppID    string
	TenantID string
}
type CatalogAccess func(context.Context, string) (CatalogScope, error)
type CatalogInput struct {
	ListInput
	ID       string `json:"id"`
	Provider string `json:"provider"`
	RunID    string `json:"run_id"`
}

func catalogInput(raw map[string]any) (CatalogInput, error) {
	var in CatalogInput
	b, err := json.Marshal(raw)
	if err != nil {
		return in, bad("Invalid catalog parameters")
	}
	if err = json.Unmarshal(b, &in); err != nil {
		return in, bad("Invalid catalog parameters")
	}
	return in, in.normalize()
}
func catalogScope(ctx context.Context, system string, access CatalogAccess) (CatalogScope, error) {
	if access == nil {
		return CatalogScope{}, denied("An external catalog scope resolver is required")
	}
	scope, err := access(ctx, system)
	if err != nil {
		return scope, err
	}
	if strings.TrimSpace(scope.AppID) == "" || strings.TrimSpace(scope.TenantID) == "" {
		return scope, denied("External catalog scope could not be resolved")
	}
	return scope, nil
}
func catalogPage(rows []map[string]any, in CatalogInput) map[string]any {
	total := len(rows)
	start := min(in.Offset, total)
	end := min(start+in.Limit, total)
	items := rows[start:end]
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{"available": true, "items": items, "total": total, "limit": in.Limit, "offset": in.Offset, "complete": true}
}

// NexusCatalogs retains provider failures instead of silently omitting models.
// Catalog metadata is distinct from the ability to make a remote completion.
func NexusCatalogs(gw *nexus.Gateway, access CatalogAccess) map[string]CatalogQuery {
	query := func(ctx context.Context, raw map[string]any) (any, error) {
		if _, err := catalogScope(ctx, "nexus", access); err != nil {
			return nil, err
		}
		if gw == nil {
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Nexus gateway is unavailable"}
		}
		in, err := catalogInput(raw)
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		for _, p := range gw.Providers().All() {
			models, err := p.Models(ctx)
			if err != nil {
				return nil, err
			}
			for _, m := range models {
				if in.ID != "" && m.ID != in.ID {
					continue
				}
				if in.Provider != "" && m.Provider != in.Provider {
					continue
				}
				if in.Search != "" && !strings.Contains(strings.ToLower(m.ID+" "+m.Name), strings.ToLower(in.Search)) {
					continue
				}
				b, err := json.Marshal(m)
				if err != nil {
					return nil, err
				}
				var row map[string]any
				dec := json.NewDecoder(strings.NewReader(string(b)))
				dec.UseNumber()
				if err = dec.Decode(&row); err != nil {
					return nil, err
				}
				if pricing, ok := row["pricing"].(map[string]any); ok {
					for k, v := range pricing {
						if n, ok := v.(json.Number); ok {
							pricing[k] = n.String()
						}
					}
				}
				rows = append(rows, row)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
		if in.ID != "" && len(rows) == 0 {
			return nil, &dc.Error{Code: dc.CodeNotFound, Message: "Model was not found"}
		}
		return catalogPage(rows, in), nil
	}
	return map[string]CatalogQuery{"models.list": query, "models.detail": query}
}

// WeaveCatalogs checks the row's app and tenant before reading its statistics.
// Released Weave filters are not scoped, so listing walks all pages and exposes
// only exact matches. A later scoped store can replace this adapter directly.
func WeaveCatalogs(eng *weaveengine.Engine, access CatalogAccess) map[string]CatalogQuery {
	query := func(ctx context.Context, raw map[string]any) (any, error) {
		scope, err := catalogScope(ctx, "weave", access)
		if err != nil {
			return nil, err
		}
		if eng == nil {
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Weave engine is unavailable"}
		}
		in, err := catalogInput(raw)
		if err != nil {
			return nil, err
		}
		cols := []*collection.Collection{}
		if in.ID != "" {
			v, err := weaveid.ParseCollectionID(in.ID)
			if err != nil {
				return nil, bad("Invalid collection ID")
			}
			row, err := eng.GetCollection(ctx, v)
			if err != nil {
				return nil, err
			}
			if row.AppID != scope.AppID || row.TenantID != scope.TenantID {
				return nil, &dc.Error{Code: dc.CodeNotFound, Message: "Collection was not found in your scope"}
			}
			cols = append(cols, row)
		} else {
			for offset := 0; ; offset += 100 {
				batch, err := eng.ListCollections(ctx, &collection.ListFilter{Search: in.Search, Limit: 100, Offset: offset})
				if err != nil {
					return nil, err
				}
				for _, row := range batch {
					if row.AppID == scope.AppID && row.TenantID == scope.TenantID {
						cols = append(cols, row)
					}
				}
				if len(batch) < 100 {
					break
				}
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
		}
		rows := []map[string]any{}
		summary := map[string]int64{"collections": int64(len(cols)), "documents": 0, "chunks": 0}
		for _, col := range cols {
			stats, err := eng.CollectionStats(ctx, col.ID)
			if err != nil {
				return nil, err
			}
			rows = append(rows, map[string]any{"id": col.ID.String(), "name": col.Name, "description": col.Description, "document_count": stats.DocumentCount, "chunk_count": stats.ChunkCount, "embedding_model": stats.EmbeddingModel, "chunk_strategy": stats.ChunkStrategy})
			summary["documents"] += stats.DocumentCount
			summary["chunks"] += stats.ChunkCount
		}
		out := catalogPage(rows, in)
		out["summary"] = summary
		return out, nil
	}
	return map[string]CatalogQuery{"knowledge.list": query, "knowledge.detail": query}
}

// ShieldCatalogs uses exact app/tenant filters and scans the complete scoped
// result before optional run filtering. Every storage error reaches the caller.
func ShieldCatalogs(eng *shieldengine.Engine, access CatalogAccess) map[string]CatalogQuery {
	profiles := func(ctx context.Context, raw map[string]any) (any, error) {
		scope, err := catalogScope(ctx, "shield", access)
		if err != nil {
			return nil, err
		}
		if eng == nil || eng.Store() == nil {
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Shield store is unavailable"}
		}
		in, err := catalogInput(raw)
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		for offset := 0; ; offset += 100 {
			batch, err := eng.Store().ListProfiles(ctx, &profile.ListFilter{AppID: scope.AppID, TenantID: scope.TenantID, Limit: 100, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				if row.AppID != scope.AppID || row.TenantID != scope.TenantID {
					continue
				}
				if in.Search != "" && !strings.Contains(strings.ToLower(row.Name), strings.ToLower(in.Search)) {
					continue
				}
				rows = append(rows, map[string]any{"id": row.ID.String(), "name": row.Name, "description": row.Description, "enabled": row.Enabled})
			}
			if len(batch) < 100 {
				break
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		return catalogPage(rows, in), nil
	}
	scans := func(ctx context.Context, raw map[string]any) (any, error) {
		scope, err := catalogScope(ctx, "shield", access)
		if err != nil {
			return nil, err
		}
		if eng == nil || eng.Store() == nil {
			return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Shield store is unavailable"}
		}
		in, err := catalogInput(raw)
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		summary := map[string]int64{"total": 0, "blocked": 0, "flagged": 0, "allowed": 0}
		for offset := 0; ; offset += 100 {
			batch, err := eng.Store().ListScans(ctx, &scan.ListFilter{AppID: scope.AppID, TenantID: scope.TenantID, Limit: 100, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				if row.AppID != scope.AppID || row.TenantID != scope.TenantID {
					continue
				}
				rid, _ := row.Metadata["run_id"].(string)
				if in.RunID != "" && rid != in.RunID {
					continue
				}
				summary["total"]++
				if row.Blocked {
					summary["blocked"]++
				}
				if row.Decision == scan.DecisionFlag {
					summary["flagged"]++
				}
				if row.Decision == scan.DecisionAllow {
					summary["allowed"]++
				}
				rows = append(rows, map[string]any{"id": row.ID.String(), "direction": row.Direction, "decision": row.Decision, "findings": row.Findings, "pii_count": row.PIICount, "profile_used": row.ProfileUsed, "duration_ms": row.Duration.Milliseconds(), "created_at": row.CreatedAt, "run_id": rid})
			}
			if len(batch) < 100 {
				break
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		out := catalogPage(rows, in)
		out["summary"] = summary
		return out, nil
	}
	return map[string]CatalogQuery{"safety.profiles": profiles, "safety.scans": scans}
}
