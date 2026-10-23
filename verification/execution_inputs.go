package verification

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// ExecutionInputs captures the scanned task and initial conversation before any
// generation. Store its canonical encoding as private evidence, not run metadata.
// ModelRevision and tool revisions are host pins, not provider attestations.
type ExecutionInputs struct {
	SchemaVersion  int            `json:"schema_version"`
	RunID          id.AgentRunID  `json:"run_id"`
	AgentID        id.AgentID     `json:"agent_id"`
	SessionID      id.SessionID   `json:"session_id"`
	PreparedDigest string         `json:"prepared_digest"`
	Input          string         `json:"input"`
	History        []llm.Message  `json:"history,omitempty"`
	Model          string         `json:"model"`
	ModelRevision  string         `json:"model_revision"`
	Tools          []ToolRevision `json:"tools,omitempty"`
}

// Encode validates and serializes an immutable input artifact of at most one MiB.
func (v ExecutionInputs) Encode() ([]byte, error) {
	if v.SchemaVersion != 1 || v.RunID.IsNil() || v.RunID.Prefix() != id.PrefixAgentRun || v.AgentID.IsNil() || v.AgentID.Prefix() != id.PrefixAgent || v.SessionID.IsNil() || v.SessionID.Prefix() != id.PrefixSession || !evidence.ValidDigest(v.PreparedDigest) || !validText(v.Input, v.Model, v.ModelRevision) || strings.TrimSpace(v.Model) == "" || strings.TrimSpace(v.ModelRevision) == "" || len(v.History) > 100 {
		return nil, ErrInvalid
	}
	if err := validateToolRevisions(v.Tools); err != nil {
		return nil, err
	}
	for _, msg := range v.History {
		if !validText(msg.Role, msg.Content, msg.ToolCallID) {
			return nil, ErrInvalid
		}
		for _, call := range msg.ToolCalls {
			if !validText(call.ID, call.Name, call.Arguments) {
				return nil, ErrInvalid
			}
		}
	}
	data, err := json.Marshal(v)
	if err != nil || len(data) > 1<<20 {
		return nil, ErrInvalid
	}
	return data, nil
}

// DecodeExecutionInputs accepts only the canonical versioned encoding. This
// rejects ambiguous duplicate keys, unknown fields and lossy UTF-8 conversions.
func DecodeExecutionInputs(data []byte) (ExecutionInputs, error) {
	var v ExecutionInputs
	if len(data) > 1<<20 || json.Unmarshal(data, &v) != nil {
		return ExecutionInputs{}, ErrInvalid
	}
	canonical, err := v.Encode()
	if err != nil || !bytes.Equal(data, canonical) {
		return ExecutionInputs{}, ErrInvalid
	}
	return v, nil
}

func validateToolRevisions(revisions []ToolRevision) error {
	if len(revisions) > 256 {
		return ErrInvalid
	}
	tools := make(map[string]bool, len(revisions))
	for _, tool := range revisions {
		if !validText(tool.Name, tool.Revision) || strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Revision) == "" || tools[tool.Name] {
			return ErrInvalid
		}
		tools[tool.Name] = true
	}
	return nil
}
