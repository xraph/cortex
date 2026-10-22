package verification

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// CheckRequest contains a single registered criterion and captured inputs.
type CheckRequest struct {
	Criterion Criterion
	Binding   Binding
	Deadline  time.Time
}

// SchemaRequest selects an immutable host schema manifest.
type SchemaRequest struct{ CheckRequest }

// TestRequest selects an immutable host test manifest, never candidate commands.
type TestRequest struct{ CheckRequest }

// StateRequest selects an authorized external-state observation manifest.
type StateRequest struct{ CheckRequest }

// HumanRequest selects a review policy for an authenticated human verdict.
type HumanRequest struct{ CheckRequest }

// CheckResult contains evaluator output without caller-controlled timestamps,
// report identity or binding. The host supplies those after evaluation.
type CheckResult struct {
	Status      Status
	Score       *float64
	EvidenceIDs []id.ID
	Human       *HumanVerdict
	Reason      string
}

// SchemaChecker validates captured content against a registered schema.
type SchemaChecker interface {
	CheckSchema(context.Context, SchemaRequest) (CheckResult, error)
}

// TestEvaluator evaluates a captured artifact in a separately qualified runner.
// Registration alone does not establish sandbox isolation.
type TestEvaluator interface {
	EvaluateTests(context.Context, TestRequest) (CheckResult, error)
}

// StateChecker observes authorized state without repeating the original task.
type StateChecker interface {
	CheckState(context.Context, StateRequest) (CheckResult, error)
}

// HumanReviewer resolves an authenticated review bound to exact evidence.
type HumanReviewer interface {
	Review(context.Context, HumanRequest) (CheckResult, error)
}

// Registration is a host-created, kind-specific evaluator binding. Its private
// dispatch function cannot be supplied through a serialized Request.
type Registration struct {
	name, revision, manifest string
	kind                     Kind
	check                    func(context.Context, CheckRequest) (CheckResult, error)
}

// RegisterSchema binds a schema evaluator to its immutable implementation and manifest.
func RegisterSchema(name, revision, manifest string, checker SchemaChecker) Registration {
	r := Registration{name: name, revision: revision, manifest: manifest, kind: KindSchema}
	if checker != nil {
		r.check = func(ctx context.Context, req CheckRequest) (CheckResult, error) {
			return checker.CheckSchema(ctx, SchemaRequest{req})
		}
	}
	return r
}

// RegisterTests binds a test evaluator to its immutable implementation and manifest.
func RegisterTests(name, revision, manifest string, checker TestEvaluator) Registration {
	r := Registration{name: name, revision: revision, manifest: manifest, kind: KindTest}
	if checker != nil {
		r.check = func(ctx context.Context, req CheckRequest) (CheckResult, error) {
			return checker.EvaluateTests(ctx, TestRequest{req})
		}
	}
	return r
}

// RegisterState binds a state observer to its immutable implementation and manifest.
func RegisterState(name, revision, manifest string, checker StateChecker) Registration {
	r := Registration{name: name, revision: revision, manifest: manifest, kind: KindState}
	if checker != nil {
		r.check = func(ctx context.Context, req CheckRequest) (CheckResult, error) {
			return checker.CheckState(ctx, StateRequest{req})
		}
	}
	return r
}

// RegisterHuman binds a review adapter to its immutable policy revision.
func RegisterHuman(name, revision, manifest string, checker HumanReviewer) Registration {
	r := Registration{name: name, revision: revision, manifest: manifest, kind: KindHuman}
	if checker != nil {
		r.check = func(ctx context.Context, req CheckRequest) (CheckResult, error) {
			return checker.Review(ctx, HumanRequest{req})
		}
	}
	return r
}
