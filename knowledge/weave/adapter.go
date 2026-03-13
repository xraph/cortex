// Package weave adapts the Weave RAG engine to the cortex knowledge.Provider interface.
package weave

import (
	"context"
	"fmt"

	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/knowledge"

	weaveengine "github.com/xraph/weave/engine"
	"github.com/xraph/weave/id"
)

// Compile-time assertion that Adapter implements knowledge.Provider.
var _ knowledge.Provider = (*Adapter)(nil)

// Adapter implements knowledge.Provider by delegating to a weave Engine.
type Adapter struct {
	engine *weaveengine.Engine
}

// New creates a new Weave adapter from an engine instance.
func New(eng *weaveengine.Engine) *Adapter {
	return &Adapter{engine: eng}
}

// EngineOption returns an engine.Option that auto-discovers a Weave engine
// from the DI container and configures the Cortex engine's knowledge provider.
// If no Weave engine is found, returns a no-op option (safe to always include).
func EngineOption(c vessel.Vessel) engine.Option {
	eng, err := vessel.Inject[*weaveengine.Engine](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithKnowledge(New(eng))
}

// Retrieve performs a semantic search and returns scored chunks.
func (a *Adapter) Retrieve(ctx context.Context, query string, params *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	var opts []weaveengine.RetrieveOption

	if params != nil {
		if params.TopK > 0 {
			opts = append(opts, weaveengine.WithTopK(params.TopK))
		}
		if params.MinScore > 0 {
			opts = append(opts, weaveengine.WithMinScore(params.MinScore))
		}
		if params.Collection != "" {
			colID, err := a.resolveCollection(ctx, params.Collection)
			if err != nil {
				return nil, fmt.Errorf("weave: resolve collection %q: %w", params.Collection, err)
			}
			opts = append(opts, weaveengine.WithCollection(colID))
		}
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

// ListCollections returns available knowledge collections with statistics.
func (a *Adapter) ListCollections(ctx context.Context) ([]knowledge.CollectionInfo, error) {
	cols, err := a.engine.ListCollections(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("weave: list collections: %w", err)
	}

	out := make([]knowledge.CollectionInfo, 0, len(cols))
	for _, col := range cols {
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

		out = append(out, info)
	}

	return out, nil
}

// resolveCollection resolves a collection name-or-ID string to a CollectionID.
// It first attempts to parse as a typed ID; on failure it looks up by name.
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
