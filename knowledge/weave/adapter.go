// Package weave adapts the Weave RAG engine to the cortex knowledge.Provider interface.
//
// Every call runs under a weave tenant, taken from the run's scope by a
// cortex.TenantFunc, so a run only ever reads its own tenant's collections
// plus the shared ones the host lists. The adapter never writes, and a call
// whose scope names no tenant fails before weave is asked anything.
package weave

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/xraph/vessel"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/knowledge"

	"github.com/xraph/weave"
	"github.com/xraph/weave/collection"
	weaveengine "github.com/xraph/weave/engine"
	"github.com/xraph/weave/id"
)

// Compile-time assertion that Adapter implements knowledge.Provider.
var _ knowledge.Provider = (*Adapter)(nil)

// defaultTopK is how many chunks a retrieval returns when the caller names
// no limit and results from more than one tenant are merged.
const defaultTopK = 5

// SharedCollection names a read-only collection that every run may search
// besides its own tenant's, such as a product documentation tenant.
type SharedCollection struct {
	// Tenant is the weave tenant that owns the collection.
	Tenant string
	// Collection is the collection's name or ID inside that tenant.
	Collection string
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithTenant sets how the adapter names the weave tenant a run reads under,
// from the run's scope. The default is cortex.RootTenant, the value of the
// scope's first level, which only works when that value is the weave
// tenant. An empty result is refused like a missing scope.
func WithTenant(f cortex.TenantFunc) Option {
	return func(a *Adapter) {
		if f != nil {
			a.tenant = f
		}
	}
}

// WithShared lists collections, each under its own tenant, that every run
// may also search. They are read only. A retrieval that names no collection
// searches them along with the run's tenant, and one that names a listed
// collection reads it under its tenant.
func WithShared(collections ...SharedCollection) Option {
	return func(a *Adapter) { a.shared = append(a.shared, collections...) }
}

// Adapter implements knowledge.Provider by delegating to a weave Engine.
type Adapter struct {
	engine *weaveengine.Engine
	tenant cortex.TenantFunc
	shared []SharedCollection
}

// New creates a new Weave adapter from an engine instance.
func New(eng *weaveengine.Engine, opts ...Option) *Adapter {
	a := &Adapter{engine: eng, tenant: cortex.RootTenant}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// EngineOption returns an engine.Option that auto-discovers a Weave engine
// from the DI container and configures the Cortex engine's knowledge provider.
// If no Weave engine is found, returns a no-op option (safe to always include).
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	eng, err := vessel.Inject[*weaveengine.Engine](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithKnowledge(New(eng, opts...))
}

// runTenant names the weave tenant for the run in ctx. A missing scope or an
// empty tenant returns an error that is both cortex.ErrNoScope and
// weave.ErrTenantRequired, so callers can match either.
func (a *Adapter) runTenant(ctx context.Context) (string, error) {
	tenant, err := a.tenant(ctx, cortex.ScopeFromContext(ctx))
	if err != nil {
		if errors.Is(err, cortex.ErrNoScope) {
			return "", fmt.Errorf("weave: tenant: %w", errors.Join(err, weave.ErrTenantRequired))
		}
		return "", fmt.Errorf("weave: tenant: %w", err)
	}
	if tenant == "" {
		return "", fmt.Errorf("weave: tenant: %w", errors.Join(cortex.ErrNoScope, weave.ErrTenantRequired))
	}
	return tenant, nil
}

// sharedNamed returns the configured shared collection that nameOrID names.
func (a *Adapter) sharedNamed(nameOrID string) (SharedCollection, bool) {
	for _, s := range a.shared {
		if s.Collection == nameOrID {
			return s, true
		}
	}
	return SharedCollection{}, false
}

// Retrieve performs a semantic search and returns scored chunks.
func (a *Adapter) Retrieve(ctx context.Context, query string, params *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	tenant, err := a.runTenant(ctx)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = &knowledge.RetrieveParams{}
	}

	if params.Collection != "" {
		owner := tenant
		if s, ok := a.sharedNamed(params.Collection); ok {
			owner = s.Tenant
		}
		return a.retrieveIn(ctx, owner, query, params.Collection, params, params.TopK)
	}

	if len(a.shared) == 0 {
		return a.retrieveIn(ctx, tenant, query, "", params, params.TopK)
	}

	topK := params.TopK
	if topK <= 0 {
		topK = defaultTopK
	}
	merged, err := a.retrieveIn(ctx, tenant, query, "", params, topK)
	if err != nil {
		return nil, err
	}
	for _, s := range a.shared {
		part, partErr := a.retrieveIn(ctx, s.Tenant, query, s.Collection, params, topK)
		if partErr != nil {
			return nil, partErr
		}
		merged = append(merged, part...)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged, nil
}

// retrieveIn runs one weave retrieval under tenant, restricted to collection
// (a name or ID in that tenant) when it is not empty.
func (a *Adapter) retrieveIn(ctx context.Context, tenant, query, collectionRef string, params *knowledge.RetrieveParams, topK int) ([]knowledge.ScoredChunk, error) {
	ctx = weave.WithTenant(ctx, tenant)

	var opts []weaveengine.RetrieveOption
	if topK > 0 {
		opts = append(opts, weaveengine.WithTopK(topK))
	}
	if params.MinScore > 0 {
		opts = append(opts, weaveengine.WithMinScore(params.MinScore))
	}
	if collectionRef != "" {
		colID, err := a.resolveCollection(ctx, collectionRef)
		if err != nil {
			return nil, fmt.Errorf("weave: resolve collection %q: %w", collectionRef, err)
		}
		opts = append(opts, weaveengine.WithCollection(colID))
	}

	results, err := a.engine.Retrieve(ctx, query, opts...)
	if err != nil {
		return nil, fmt.Errorf("weave: retrieve: %w", err)
	}

	out := make([]knowledge.ScoredChunk, len(results))
	for i, r := range results {
		sc := knowledge.ScoredChunk{
			Score: r.Score,
		}
		if r.Chunk != nil {
			sc.Content = r.Chunk.Content
			sc.DocumentID = r.Chunk.DocumentID.String()
			sc.CollectionID = r.Chunk.CollectionID.String()
			sc.Metadata = r.Chunk.Metadata
		}
		out[i] = sc
	}

	return out, nil
}

// ListCollections returns the run tenant's collections and the configured
// shared ones, with statistics, each read under its own tenant.
func (a *Adapter) ListCollections(ctx context.Context) ([]knowledge.CollectionInfo, error) {
	tenant, err := a.runTenant(ctx)
	if err != nil {
		return nil, err
	}

	tctx := weave.WithTenant(ctx, tenant)
	cols, err := a.engine.ListCollections(tctx, nil)
	if err != nil {
		return nil, fmt.Errorf("weave: list collections: %w", err)
	}

	out := make([]knowledge.CollectionInfo, 0, len(cols)+len(a.shared))
	seen := make(map[string]bool, len(cols)+len(a.shared))
	for _, col := range cols {
		seen[col.ID.String()] = true
		out = append(out, a.info(tctx, col))
	}

	for _, s := range a.shared {
		sctx := weave.WithTenant(ctx, s.Tenant)
		colID, resolveErr := a.resolveCollection(sctx, s.Collection)
		if resolveErr != nil {
			return nil, fmt.Errorf("weave: resolve shared collection %q: %w", s.Collection, resolveErr)
		}
		if seen[colID.String()] {
			continue
		}
		col, getErr := a.engine.GetCollection(sctx, colID)
		if getErr != nil {
			return nil, fmt.Errorf("weave: get shared collection %q: %w", s.Collection, getErr)
		}
		seen[colID.String()] = true
		out = append(out, a.info(sctx, col))
	}

	return out, nil
}

// info describes col, reading its statistics under ctx's tenant. A failed
// statistics read leaves the counts at zero.
func (a *Adapter) info(ctx context.Context, col *collection.Collection) knowledge.CollectionInfo {
	info := knowledge.CollectionInfo{
		ID:   col.ID.String(),
		Name: col.Name,
	}
	stats, err := a.engine.CollectionStats(ctx, col.ID)
	if err == nil {
		info.DocumentCount = stats.DocumentCount
		info.ChunkCount = stats.ChunkCount
		info.EmbeddingModel = stats.EmbeddingModel
	}
	return info
}

// resolveCollection resolves a collection name-or-ID string to a CollectionID
// under the tenant in ctx. It first attempts to parse as a typed ID; on
// failure it looks up by name. Weave checks that an ID belongs to the tenant
// when the retrieval runs.
func (a *Adapter) resolveCollection(ctx context.Context, nameOrID string) (id.CollectionID, error) {
	if colID, err := id.ParseCollectionID(nameOrID); err == nil {
		return colID, nil
	}

	col, err := a.engine.GetCollectionByName(ctx, nameOrID)
	if err != nil {
		return id.Nil, fmt.Errorf("collection not found: %w", err)
	}
	return col.ID, nil
}
