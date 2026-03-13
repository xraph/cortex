package dashboard

import "github.com/xraph/cortex/dashboard/shared"

// Re-export knowledge types from shared to maintain backward compatibility for
// external consumers (e.g. extension package).

// KnowledgeSource provides an abstraction over knowledge engine data.
type KnowledgeSource = shared.KnowledgeSource

// KnowledgeCollectionInfo summarises a knowledge collection for the dashboard.
type KnowledgeCollectionInfo = shared.KnowledgeCollectionInfo

// KnowledgeCollectionStats provides detailed statistics for a single collection.
type KnowledgeCollectionStats = shared.KnowledgeCollectionStats
