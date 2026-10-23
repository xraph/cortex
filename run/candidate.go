package run

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"unicode/utf8"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// CandidateCapture ends generation under a registered intent. Output is private
// candidate material, not an authorized public answer. The trusted runtime
// supplies cumulative usage and the saved running revision before finalization.
type CandidateCapture struct {
	Transcript          []byte          `json:"transcript,omitempty"`
	SchemaVersion       int             `json:"schema_version"`
	IntentID            id.ID           `json:"intent_id"`
	ExpectedRunRevision int64           `json:"expected_run_revision"`
	Output              string          `json:"output"`
	FinishReason        string          `json:"finish_reason"`
	Usage               execution.Usage `json:"usage"`
}

// Freeze validates a final-answer capture and binds retries to identical content
// and accounting. It leaves room for capture, enqueue and commit revisions.
func (c CandidateCapture) Freeze() (CandidateCapture, string, error) {
	u := c.Usage
	if (c.SchemaVersion != 1 && c.SchemaVersion != 2) || c.IntentID.IsNil() || c.IntentID.Prefix() != verification.PrefixCaptureIntent || c.ExpectedRunRevision < 1 || c.ExpectedRunRevision >= math.MaxInt64-2 || !utf8.ValidString(c.Output) || u.Steps < 0 || u.Tokens < 0 || u.ReportedTokens < 0 || u.ReportedTokens > u.Tokens || u.ToolCalls < 0 {
		return CandidateCapture{}, "", verification.ErrInvalid
	}
	switch c.FinishReason {
	case "", "stop", "end_turn":
	default:
		return CandidateCapture{}, "", verification.ErrInvalid
	}
	if c.SchemaVersion == 1 {
		if len(c.Transcript) != 0 {
			return CandidateCapture{}, "", verification.ErrInvalid
		}
	} else {
		trace, err := verification.DecodeExecutionTranscript(c.Transcript)
		if err != nil || trace.Usage != c.Usage || trace.Messages[len(trace.Messages)-1].Content != c.Output {
			return CandidateCapture{}, "", verification.ErrInvalid
		}
		c.Transcript = bytes.Clone(c.Transcript)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return CandidateCapture{}, "", err
	}
	return c, evidence.Digest(data), nil
}

// CandidateStore atomically saves private evidence, finishes generation and
// enqueues verification. Run output stays empty until guarded result commit.
// It is a raw host storage capability: authorize capture and reserve the budget
// before use. It does not gate the engine's streams, history or tool dispatch.
type CandidateStore interface {
	CaptureCandidate(context.Context, CandidateCapture) (verification.Job, error)
}
