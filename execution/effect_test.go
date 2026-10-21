package execution

import (
	"testing"
	"time"
)

func TestEffectRequirementsRejectUnsafeDeclarations(t *testing.T) {
	valid := EffectRequirement{Tool: "write", Revision: "1", Class: EffectNonIdempotent, ReconcileRevision: "probe-v1", Retry: RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Second, MaxBackoff: 5 * time.Second}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unknown", "unbounded", "reconcile", "idempotency", "compensation", "backoff", "text"} {
		t.Run(mode, func(t *testing.T) {
			value := valid
			switch mode {
			case "unknown":
				value.Class = "invented"
			case "unbounded":
				value.Retry.MaxAttempts = 0
			case "reconcile":
				value.ReconcileRevision = ""
			case "idempotency":
				value.Class = EffectIdempotent
			case "compensation":
				value.Class = EffectCompensatable
			case "backoff":
				value.Retry.InitialBackoff = 0
			case "text":
				value.Revision = string([]byte{0xff})
			}
			if err := value.Validate(); err == nil {
				t.Fatalf("accepted unsafe %s", mode)
			}
		})
	}
}

func TestEffectRequirementsAcceptExplicitRecoveryContracts(t *testing.T) {
	for _, class := range []EffectClass{EffectReadOnly, EffectIdempotent, EffectNonIdempotent, EffectCompensatable} {
		t.Run(string(class), func(t *testing.T) {
			effect := EffectRequirement{Tool: "operation", Revision: "1", Class: class, Retry: RetryPolicy{MaxAttempts: 1}}
			if class != EffectReadOnly {
				effect.ReconcileRevision = "probe-v1"
			}
			if class == EffectIdempotent {
				effect.IdempotencyRevision = "key-contract-v1"
			}
			if class == EffectCompensatable {
				effect.Compensation = EffectReference{Tool: "undo", Revision: "1"}
			}
			if err := effect.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
