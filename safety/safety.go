// Package safety defines the interface for content safety scanning
// in the Cortex agent engine. Implementations bridge external safety
// engines (e.g. Shield) to the Cortex runtime without introducing
// a direct dependency on any specific safety library.
package safety

import (
	"context"
	"time"
)

// Direction indicates the scan direction.
type Direction string

const (
	DirectionInput  Direction = "input"
	DirectionOutput Direction = "output"
)

// Decision is the safety verdict.
type Decision string

const (
	DecisionAllow  Decision = "allow"
	DecisionBlock  Decision = "block"
	DecisionFlag   Decision = "flag"
	DecisionRedact Decision = "redact"
)

// ScanRequest is the input to a safety scan.
type ScanRequest struct {
	Content     string         `json:"content"`
	Direction   Direction      `json:"direction"`
	AgentID     string         `json:"agent_id"`
	RunID       string         `json:"run_id"`
	ProfileName string         `json:"profile_name,omitempty"`
	AppID       string         `json:"app_id,omitempty"`
	TenantID    string         `json:"tenant_id,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Finding is a single detection from a safety layer.
type Finding struct {
	Layer    string  `json:"layer"`
	Source   string  `json:"source"`
	Severity string  `json:"severity"`
	Message  string  `json:"message"`
	Score    float64 `json:"score,omitempty"`
}

// ScanResult is the output of a safety scan.
type ScanResult struct {
	Decision    Decision      `json:"decision"`
	Blocked     bool          `json:"blocked"`
	Findings    []Finding     `json:"findings,omitempty"`
	Redacted    string        `json:"redacted,omitempty"`
	PIICount    int           `json:"pii_count,omitempty"`
	ProfileUsed string        `json:"profile_used,omitempty"`
	Duration    time.Duration `json:"duration"`
}

// Scanner is the interface the cortex engine uses for safety scanning.
// Implementations must be safe for concurrent use.
type Scanner interface {
	// ScanInput scans content before it is sent to the LLM.
	ScanInput(ctx context.Context, req *ScanRequest) (*ScanResult, error)
	// ScanOutput scans content after it is received from the LLM.
	ScanOutput(ctx context.Context, req *ScanRequest) (*ScanResult, error)
}
