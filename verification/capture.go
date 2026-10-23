package verification

import "context"

// CaptureStore atomically copies a completed run's saved output into candidate
// evidence, enqueues its verification job and marks the run pending. It never
// accepts candidate bytes from the caller. The requested reference must match
// the exact stored output. Supporting evidence and the contract must exist.
//
// This is trusted storage, not authorization. The host must authorize the run
// read, evidence write (including sensitivity and retention) and job binding.
// Same-request retries return the historical job without recreating evidence
// removed since capture. They do not grant access or permission to publish.
// Implementations require a real transaction and must preserve tombstones.
type CaptureStore interface {
	CaptureVerificationJob(context.Context, JobRequest) (Job, error)
}
