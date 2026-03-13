package dashboard

import "github.com/xraph/cortex/dashboard/shared"

// Re-export types from shared to maintain backward compatibility for
// external consumers (e.g. extension package).

// ModelSource provides an abstraction over LLM model discovery.
type ModelSource = shared.ModelSource

// ModelInfo is a dashboard-level representation of an LLM model.
type ModelInfo = shared.ModelInfo

// ModelCapabilities describes what an LLM model supports.
type ModelCapabilities = shared.ModelCapabilities

// ProviderInfo summarises an LLM provider for the dashboard.
type ProviderInfo = shared.ProviderInfo
