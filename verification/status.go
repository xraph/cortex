// Package verification defines result assurance independently of execution state.
package verification

// Status records the outcome of explicit result checks, never model confidence.
type Status string

const (
	StatusUnchecked    Status = "unchecked"
	StatusPending      Status = "pending"
	StatusPassed       Status = "passed"
	StatusFailed       Status = "failed"
	StatusInconclusive Status = "inconclusive"
	StatusStale        Status = "stale"
)

// Normalized treats historical records with no checks as unchecked. Unknown
// values survive round trips but do not become passed.
func (s Status) Normalized() Status {
	if s == "" {
		return StatusUnchecked
	}
	return s
}
