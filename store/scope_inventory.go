package store

import "context"

// ScopeIssue identifies a stored identity requiring operator reconciliation.
// It includes scope metadata only, never prompt, tool or conversation content.
type ScopeIssue struct {
	Resource  string    `json:"resource"`
	ID        string    `json:"id"`
	Canonical string    `json:"canonical"`
	Indexed   [3]string `json:"indexed"`
	Extra     string    `json:"extra"`
	Reason    string    `json:"reason"`
}

// ScopeInspector is an optional read-only administrative capability. It visits
// invalid rows across the entire database. Hosts must restrict access to their
// database operators; request scope does not limit this maintenance operation.
// A callback error or cancellation stops inspection. No identity is rewritten.
type ScopeInspector interface {
	InspectScopes(ctx context.Context, visit func(ScopeIssue) error) error
}
