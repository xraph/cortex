package assurance

import (
	"testing"
	"time"

	testrequire "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func TestPreparedProposalAccountingRejectsUnreservedWork(t *testing.T) {
	now := time.Now().UTC()
	binding := preparedProposalBinding{RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), SessionID: id.NewSessionID(), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, PreparedDigest: evidence.Digest([]byte("prepared")), InputDigest: evidence.Digest([]byte("input")), NotBefore: now, Reservation: execution.ReservationRequest{Allocation: execution.Resources{Steps: 1, Tokens: 100, FanOut: 1}, Deadline: now.Add(time.Minute)}}
	for _, mode := range []string{"valid", "normal purpose", "verification", "context", "tools", "session", "input", "steps", "deadline", "negative usage"} {
		t.Run(mode, func(t *testing.T) {
			current := &run.Run{ID: binding.RunID, AgentID: binding.AgentID, SessionID: binding.SessionID, Scope: binding.Scope, Input: "input", StartedAt: &now, Budget: execution.Budget{MaxSteps: 1, MaxTokens: 100, Deadline: binding.Reservation.Deadline}, Provenance: run.ExecutionProvenance{SchemaVersion: 1, PreparedDigest: binding.PreparedDigest, Purpose: run.PurposeEvaluation}}
			switch mode {
			case "normal purpose":
				current.Provenance.Purpose = run.PurposeNormal
			case "verification":
				current.Provenance.VerificationRequired = true
			case "context":
				current.Provenance.ContextSelection = &cognitive.ContextRecord{}
			case "tools":
				current.Usage.ToolCalls = 1
			case "session":
				current.SessionID = id.NewSessionID()
			case "input":
				current.Input = "changed"
			case "steps":
				current.Budget.MaxSteps = 0
			case "deadline":
				current.Budget.Deadline = binding.Reservation.Deadline.Add(time.Second)
			case "negative usage":
				current.Usage.ReportedTokens = -1
			}
			err := validatePreparedProposalRun(binding, current)
			if mode == "valid" {
				testrequire.NoError(t, err)
			} else {
				testrequire.Error(t, err)
			}
		})
	}
	current := &run.Run{Usage: execution.Usage{Steps: 2, Tokens: 101, ReportedTokens: 120}, StepCount: 3, TokensUsed: 99}
	minimum := preparedProposalUsage(current)
	testrequire.EqualValues(t, 3, minimum.Steps)
	testrequire.EqualValues(t, 120, minimum.Tokens)
	testrequire.Zero(t, minimum.Replans)
}
