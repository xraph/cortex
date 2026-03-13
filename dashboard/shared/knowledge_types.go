package shared

import "context"

// KnowledgeSource provides an abstraction over knowledge engine data for the dashboard.
// When weave is available, it adapts the weave engine; otherwise the
// dashboard works without it.
type KnowledgeSource interface {
	ListCollections(ctx context.Context) ([]KnowledgeCollectionInfo, error)
	CollectionStats(ctx context.Context, colID string) (*KnowledgeCollectionStats, error)
}

// KnowledgeCollectionInfo summarises a knowledge collection for the dashboard.
type KnowledgeCollectionInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DocumentCount  int64  `json:"document_count"`
	ChunkCount     int64  `json:"chunk_count"`
	EmbeddingModel string `json:"embedding_model"`
	ChunkStrategy  string `json:"chunk_strategy"`
}

// KnowledgeCollectionStats provides detailed statistics for a single collection.
type KnowledgeCollectionStats struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DocumentCount  int64  `json:"document_count"`
	ChunkCount     int64  `json:"chunk_count"`
	EmbeddingModel string `json:"embedding_model"`
	ChunkStrategy  string `json:"chunk_strategy"`
}
