package capture

import (
	"context"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
)

// Journal is the authenticated daemon's durable capture capability. Record
// commits an immutable intent before upload; Confirm commits verified readback.
// Both must bind the exact job and reference to its current execution claim,
// honor the original deadline and return success only after durable commit.
// A worker must never open the daemon's journal directly. An uncertain response
// stops capture; this port grants no retry, new identity or execution authority.
type Journal interface {
	Record(context.Context, sandbox.Job, evidence.Ref) error
	Confirm(context.Context, sandbox.Job, evidence.Ref) error
}
