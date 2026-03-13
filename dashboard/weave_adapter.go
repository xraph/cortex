package dashboard

import (
	"context"

	"github.com/xraph/weave/collection"
	weaveengine "github.com/xraph/weave/engine"
	"github.com/xraph/weave/id"
)

// weaveAdapter wraps a *weave/engine.Engine to satisfy the KnowledgeSource interface.
type weaveAdapter struct {
	eng *weaveengine.Engine
}

// NewWeaveKnowledgeSource creates a KnowledgeSource backed by a weave engine.
func NewWeaveKnowledgeSource(eng *weaveengine.Engine) KnowledgeSource {
	return &weaveAdapter{eng: eng}
}

func (a *weaveAdapter) ListCollections(ctx context.Context) ([]KnowledgeCollectionInfo, error) {
	cols, err := a.eng.ListCollections(ctx, &collection.ListFilter{})
	if err != nil {
		return nil, err
	}

	result := make([]KnowledgeCollectionInfo, 0, len(cols))
	for _, col := range cols {
		stats, err := a.eng.CollectionStats(ctx, col.ID)
		if err != nil {
			// Best-effort: include collection with zero counts.
			result = append(result, KnowledgeCollectionInfo{
				ID:             col.ID.String(),
				Name:           col.Name,
				EmbeddingModel: col.EmbeddingModel,
				ChunkStrategy:  col.ChunkStrategy,
			})
			continue
		}
		result = append(result, KnowledgeCollectionInfo{
			ID:             col.ID.String(),
			Name:           col.Name,
			DocumentCount:  stats.DocumentCount,
			ChunkCount:     stats.ChunkCount,
			EmbeddingModel: stats.EmbeddingModel,
			ChunkStrategy:  stats.ChunkStrategy,
		})
	}
	return result, nil
}

func (a *weaveAdapter) CollectionStats(ctx context.Context, colIDStr string) (*KnowledgeCollectionStats, error) {
	colID, err := id.ParseCollectionID(colIDStr)
	if err != nil {
		return nil, err
	}
	stats, err := a.eng.CollectionStats(ctx, colID)
	if err != nil {
		return nil, err
	}
	return &KnowledgeCollectionStats{
		ID:             stats.CollectionID.String(),
		Name:           stats.CollectionName,
		DocumentCount:  stats.DocumentCount,
		ChunkCount:     stats.ChunkCount,
		EmbeddingModel: stats.EmbeddingModel,
		ChunkStrategy:  stats.ChunkStrategy,
	}, nil
}
