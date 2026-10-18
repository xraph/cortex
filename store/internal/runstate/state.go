// Package runstate encodes storage states that fence unsupported workers.
package runstate

import "github.com/xraph/cortex/run"

// PausedPrepared is deliberately unknown to readers predating preparation.
// Their atomic resume and expiry claims match only the literal paused state.
const PausedPrepared = "paused_prepared"

// Encode protects prepared pauses while retaining the public lifecycle.
func Encode(r *run.Run) string {
	if r.State == run.StatePaused && r.Provenance.PreparedDigest != "" {
		return PausedPrepared
	}
	return string(r.State)
}

// Decode restores the public state understood by preparation-aware workers.
func Decode(stored string) run.State {
	if stored == PausedPrepared {
		return run.StatePaused
	}
	return run.State(stored)
}
