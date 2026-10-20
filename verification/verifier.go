package verification

import (
	"context"
	"time"
)

// Request contains host-captured inputs only. It cannot register evaluators or
// execute callbacks. Each verifier returns only its registered criteria.
type Request struct {
	Contract         Contract  `json:"contract"`
	Binding          Binding   `json:"binding"`
	Deadline         time.Time `json:"deadline"`
	Verifier         string    `json:"verifier"`
	VerifierRevision string    `json:"verifier_revision"`
}

// Verifier evaluates a captured candidate without rerunning the original task.
// Implementations must reauthorize resource reads and host-stamp report times.
type Verifier interface {
	Verify(context.Context, Request) (Report, error)
}
