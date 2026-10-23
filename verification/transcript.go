package verification

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// ToolOutcome is a host-recorded result, never inferred from tool response text.
type ToolOutcome string

// Terminal outcomes distinguish successful responses from failures and denials.
const (
	ToolCompleted ToolOutcome = "completed"
	ToolFailed    ToolOutcome = "failed"
	ToolDenied    ToolOutcome = "denied"
)

// ToolSource identifies who supplied a result. External results are assertions
// by an authorized caller, not independent proof of an external effect.
type ToolSource string

const (
	ToolLocal    ToolSource = "local"
	ToolExternal ToolSource = "external"
)

// ToolResult attaches trusted outcome metadata to a conversation tool-call ID.
// The checked content and arguments live in ExecutionTranscript.Messages.
type ToolResult struct {
	CallID  string      `json:"call_id"`
	Outcome ToolOutcome `json:"outcome"`
	Source  ToolSource  `json:"source"`
}

// ExecutionTranscript preserves this run's checked model/tool conversation.
// Initial task/history are linked by InputsDigest. This is private evidence,
// not public conversation history or a crash-atomic external effect receipt.
type ExecutionTranscript struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         id.AgentRunID   `json:"run_id"`
	InputsDigest  string          `json:"inputs_digest"`
	Messages      []llm.Message   `json:"messages"`
	Tools         []ToolResult    `json:"tools,omitempty"`
	Usage         execution.Usage `json:"usage"`
}

// Encode validates a complete conversation and returns at most one MiB of
// canonical JSON. Every requested call has one response and one host outcome.
func (v ExecutionTranscript) Encode() ([]byte, error) {
	if v.SchemaVersion != 1 || v.RunID.IsNil() || v.RunID.Prefix() != id.PrefixAgentRun || !evidence.ValidDigest(v.InputsDigest) || len(v.Messages) == 0 || len(v.Messages) > 4096 || len(v.Tools) > 4096 {
		return nil, ErrInvalid
	}
	u := v.Usage
	if u.Steps < 1 || u.Tokens < 0 || u.ReportedTokens < 0 || u.ReportedTokens > u.Tokens || u.ToolCalls < 0 {
		return nil, ErrInvalid
	}
	pending := map[string]bool{}
	calls := map[string]bool{}
	steps := 0
	for i, msg := range v.Messages {
		if len(msg.ToolCalls) > 4096-len(calls) || !validMessageText(msg) {
			return nil, ErrInvalid
		}
		switch msg.Role {
		case "assistant":
			if len(pending) != 0 || msg.ToolCallID != "" || (len(msg.ToolCalls) == 0 && i != len(v.Messages)-1) {
				return nil, ErrInvalid
			}
			steps++
			for _, call := range msg.ToolCalls {
				if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" || calls[call.ID] {
					return nil, ErrInvalid
				}
				calls[call.ID], pending[call.ID] = true, true
			}
		case "tool":
			if len(msg.ToolCalls) != 0 || !pending[msg.ToolCallID] {
				return nil, ErrInvalid
			}
			delete(pending, msg.ToolCallID)
		default:
			return nil, ErrInvalid
		}
	}
	last := v.Messages[len(v.Messages)-1]
	if len(pending) != 0 || last.Role != "assistant" || len(last.ToolCalls) != 0 || steps != u.Steps || u.ToolCalls > len(calls) || len(v.Tools) != len(calls) {
		return nil, ErrInvalid
	}
	for _, result := range v.Tools {
		if !calls[result.CallID] || (result.Outcome != ToolCompleted && result.Outcome != ToolFailed && result.Outcome != ToolDenied) || (result.Source != ToolLocal && result.Source != ToolExternal) {
			return nil, ErrInvalid
		}
		delete(calls, result.CallID)
	}
	data, err := json.Marshal(v)
	if err != nil || len(data) > 1<<20 {
		return nil, ErrInvalid
	}
	return data, nil
}

// DecodeExecutionTranscript rejects noncanonical, ambiguous or future encodings.
func DecodeExecutionTranscript(data []byte) (ExecutionTranscript, error) {
	var v ExecutionTranscript
	if len(data) > 1<<20 || json.Unmarshal(data, &v) != nil {
		return ExecutionTranscript{}, ErrInvalid
	}
	canonical, err := v.Encode()
	if err != nil || !bytes.Equal(data, canonical) {
		return ExecutionTranscript{}, ErrInvalid
	}
	return v, nil
}

// ValidateFor checks a transcript against captured input, output and accounting.
// Unknown requested tools may appear only as locally denied, uncharged calls.
func (v ExecutionTranscript) ValidateFor(inputs ExecutionInputs, output string, usage execution.Usage) error {
	if _, err := v.Encode(); err != nil {
		return err
	}
	data, err := inputs.Encode()
	if err != nil || v.RunID != inputs.RunID || v.InputsDigest != evidence.Digest(data) || v.Usage != usage || v.Messages[len(v.Messages)-1].Content != output {
		return ErrInvalid
	}
	known := make(map[string]bool, len(inputs.Tools))
	for _, tool := range inputs.Tools {
		known[tool.Name] = true
	}
	results := make(map[string]ToolResult, len(v.Tools))
	for _, result := range v.Tools {
		results[result.CallID] = result
	}
	charged := 0
	for _, msg := range v.Messages {
		for _, call := range msg.ToolCalls {
			if known[call.Name] {
				charged++
				continue
			}
			result := results[call.ID]
			if result.Outcome != ToolDenied || result.Source != ToolLocal {
				return ErrInvalid
			}
		}
	}
	if charged != usage.ToolCalls {
		return ErrInvalid
	}
	return nil
}

func validMessageText(msg llm.Message) bool {
	if !validText(msg.Role, msg.Content, msg.ToolCallID) {
		return false
	}
	for _, call := range msg.ToolCalls {
		if !validText(call.ID, call.Name, call.Arguments) {
			return false
		}
	}
	return true
}
