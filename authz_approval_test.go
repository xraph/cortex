package cortex_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xraph/cortex"
)

// An authorizer that returns ApprovalRequired is still asking for
// approval, so every host that matched ErrRequiresApproval with errors.Is
// keeps working, and the engine can still recover the TTL and the reason
// through any amount of wrapping.
func TestApprovalRequiredMatchesErrRequiresApproval(t *testing.T) {
	if !errors.Is(cortex.ApprovalRequired{}, cortex.ErrRequiresApproval) {
		t.Fatal("a bare ApprovalRequired does not match ErrRequiresApproval")
	}

	wrapped := fmt.Errorf("x: %w", cortex.ApprovalRequired{Reason: "prod deploy", TTL: time.Hour})
	if !errors.Is(wrapped, cortex.ErrRequiresApproval) {
		t.Fatal("a wrapped ApprovalRequired does not match ErrRequiresApproval")
	}

	var approval cortex.ApprovalRequired
	if !errors.As(wrapped, &approval) {
		t.Fatal("errors.As could not recover ApprovalRequired through the wrap")
	}
	if approval.TTL != time.Hour {
		t.Errorf("TTL = %s, want %s", approval.TTL, time.Hour)
	}
	if approval.Reason != "prod deploy" {
		t.Errorf("Reason = %q, want %q", approval.Reason, "prod deploy")
	}

	if got, want := approval.Error(), "cortex: tool call requires approval: prod deploy"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if errors.Is(cortex.ApprovalRequired{}, cortex.ErrRunNotFound) {
		t.Error("ApprovalRequired matched an unrelated sentinel")
	}
}
