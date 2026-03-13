// Package knowledge defines the abstraction for knowledge retrieval providers.
//
// It decouples Cortex from specific RAG engines (e.g., Weave) the same way
// the llm.Client interface decouples Cortex from specific LLM providers.
package knowledge

import "context"

// ScoredChunk represents a retrieved knowledge chunk with relevance score.
type ScoredChunk struct {
	Content      string            `json:"content"`
	Score        float64           `json:"score"`
	Source       string            `json:"source,omitempty"`
	DocumentID   string            `json:"document_id,omitempty"`
	CollectionID string            `json:"collection_id,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// CollectionInfo describes an available knowledge collection.
type CollectionInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DocumentCount  int64  `json:"document_count"`
	ChunkCount     int64  `json:"chunk_count"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
}

// RetrieveParams configures a knowledge retrieval query.
type RetrieveParams struct {
	Collection string  // Collection name or ID to search in (empty = all).
	TopK       int     // Max results (default: 5).
	MinScore   float64 // Minimum relevance score (default: 0.0).
}

// Provider is the interface for knowledge retrieval backends.
// Implementations wrap specific engines (e.g., Weave) to provide
// knowledge to Cortex agents.
type Provider interface {
	// Retrieve performs a semantic search and returns scored chunks.
	Retrieve(ctx context.Context, query string, params *RetrieveParams) ([]ScoredChunk, error)

	// ListCollections returns available knowledge collections.
	ListCollections(ctx context.Context) ([]CollectionInfo, error)
}
