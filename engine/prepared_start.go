package engine

import "github.com/xraph/cortex/id"

// PreparedStartError retains an attempted run identity after startup fails.
// Creation may have committed despite an error. Inspect durable state before
// recovery; this error never authorizes retry or proves a terminal state.
// Error omits private failure details; errors.Is and errors.As retain the cause.
type PreparedStartError struct {
	runID id.AgentRunID
	cause error
}

// Error returns a public message without storage or captured-content details.
func (*PreparedStartError) Error() string { return "cortex: prepared run startup failed" }

// RunIdentity returns the identity used for the attempted creation.
func (e *PreparedStartError) RunIdentity() id.AgentRunID { return e.runID }

// Unwrap preserves startup and persistence failures for host-side handling.
func (e *PreparedStartError) Unwrap() error { return e.cause }
