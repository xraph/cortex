package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestVerificationFailureHTTPRecovery(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateCompleted, VerificationStatus: verification.StatusPending, Output: "SECRET candidate"}
	for cause, want := range map[error]int{cortex.ErrRunUnverified: 409, cortex.ErrVerificationUnavailable: 503} {
		wrapped := fmt.Errorf("SECRET diagnostic: %w", cause)
		mapped := mapRunError(wrapped, r)
		httpErr, ok := mapped.(interface {
			StatusCode() int
			ResponseBody() any
		})
		if !ok || httpErr.StatusCode() != want {
			t.Fatalf("wrong HTTP mapping: %v", mapped)
		}
		data, err := json.Marshal(httpErr.ResponseBody())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), r.ID.String()) || !strings.Contains(string(data), `"retryable":false`) || strings.Contains(string(data), "SECRET") {
			t.Fatalf("unsafe recovery body: %s", data)
		}
		initial := mapStoreError(wrapped)
		status, ok := initial.(statusCoder)
		if !ok || status.StatusCode() != want || strings.Contains(initial.Error(), "SECRET") {
			t.Fatalf("wrong initial failure: %v", initial)
		}
	}
}
