package dashboard

import "github.com/xraph/cortex/dashboard/shared"

// Re-export safety types from shared to maintain backward compatibility for
// external consumers (e.g. extension package).

// SafetySource provides an abstraction over safety engine data.
type SafetySource = shared.SafetySource

// SafetyProfileInfo summarises a shield safety profile for the dashboard.
type SafetyProfileInfo = shared.SafetyProfileInfo

// ScanSummary provides aggregated scan statistics.
type ScanSummary = shared.ScanSummary

// ScanInfo represents a single scan result for dashboard display.
type ScanInfo = shared.ScanInfo
