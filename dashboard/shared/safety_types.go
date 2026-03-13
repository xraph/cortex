package shared

import "context"

// SafetySource provides an abstraction over safety engine data for the dashboard.
// When shield is available, it adapts the shield engine; otherwise the
// dashboard works without it.
type SafetySource interface {
	ListProfiles(ctx context.Context) ([]SafetyProfileInfo, error)
	GetScanSummary(ctx context.Context, agentID string) (*ScanSummary, error)
	GetRunScans(ctx context.Context, runID string) ([]ScanInfo, error)
}

// SafetyProfileInfo summarises a shield safety profile for the dashboard.
type SafetyProfileInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// ScanSummary provides aggregated scan statistics.
type ScanSummary struct {
	TotalScans    int64   `json:"total_scans"`
	BlockedScans  int64   `json:"blocked_scans"`
	FlaggedScans  int64   `json:"flagged_scans"`
	AllowedScans  int64   `json:"allowed_scans"`
	BlockRate     float64 `json:"block_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// ScanInfo represents a single scan result for dashboard display.
type ScanInfo struct {
	ID           string `json:"id"`
	Direction    string `json:"direction"`
	Decision     string `json:"decision"`
	Blocked      bool   `json:"blocked"`
	FindingCount int    `json:"finding_count"`
	PIICount     int    `json:"pii_count"`
	ProfileUsed  string `json:"profile_used"`
	DurationMs   int64  `json:"duration_ms"`
	CreatedAt    string `json:"created_at"`
}
