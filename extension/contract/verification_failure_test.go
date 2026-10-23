package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func TestVerificationFailureContractRecovery(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), Output: "SECRET candidate"}
	for cause, want := range map[error]dc.ErrorCode{cortex.ErrRunUnverified: dc.CodeConflict, cortex.ErrVerificationUnavailable: dc.CodeUnavailable} {
		wrapped := fmt.Errorf("SECRET diagnostic: %w", cause)
		mapped := mapRunError(wrapped, r)
		var failure *dc.Error
		if !errors.As(mapped, &failure) || failure.Code != want || failure.Retryable || failure.Details["run_id"] != r.ID.String() {
			t.Fatalf("unsafe recovery error: %v", mapped)
		}
		data, err := json.Marshal(failure)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "SECRET") {
			t.Fatalf("private data: %s", data)
		}
		if !errors.As(mapError(wrapped), &failure) || failure.Retryable {
			t.Fatalf("unsafe initial failure: %v", failure)
		}
	}
}

func TestFailedRunCommandPreservesRecoveryIdentity(t *testing.T) {
	_, s, _ := testService(t)
	d := dispatcher.New(nil)
	r := &run.Run{ID: id.NewAgentRunID(), Output: "SECRET output"}
	if err := command(d, s, "runs.execute", "run", func(context.Context, ExecuteInput) (*run.Run, error) { return r, cortex.ErrRunUnverified }); err != nil {
		t.Fatal(err)
	}
	data, err := dispatch(t, d, principal("recovery"), "runs.execute", ExecuteInput{})
	var failure *dc.Error
	if !errors.As(err, &failure) || failure.Details["run_id"] != r.ID.String() || failure.Retryable {
		t.Fatalf("lost recovery identity: %v", err)
	}
	if strings.Contains(string(data), "SECRET") {
		t.Fatalf("failed command exposed output: %s", data)
	}
}
