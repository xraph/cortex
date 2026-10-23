package verification_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/verification"
)

func executionInputsFixture() verification.ExecutionInputs {
	return verification.ExecutionInputs{SchemaVersion: 1, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), SessionID: id.NewSessionID(), PreparedDigest: evidence.Digest([]byte("prepared")), Input: "actual task", History: []llm.Message{{Role: "user", Content: "prior task"}}, Model: "model", ModelRevision: "model:1", Tools: []verification.ToolRevision{{Name: "probe", Revision: "1"}}}
}

func TestExecutionInputsRoundTripAndLimits(t *testing.T) {
	original := executionInputsFixture()
	data, err := original.Encode()
	require.NoError(t, err)
	restored, err := verification.DecodeExecutionInputs(data)
	require.NoError(t, err)
	require.Equal(t, original, restored)
	restored.History[0].Content = "changed"
	require.Equal(t, "prior task", original.History[0].Content)
	for name, mutate := range map[string]func(*verification.ExecutionInputs){
		"schema":         func(v *verification.ExecutionInputs) { v.SchemaVersion = 2 },
		"run":            func(v *verification.ExecutionInputs) { v.RunID = id.NewAgentID() },
		"model revision": func(v *verification.ExecutionInputs) { v.ModelRevision = "" },
		"duplicate tool": func(v *verification.ExecutionInputs) { v.Tools = append(v.Tools, v.Tools[0]) },
		"invalid text":   func(v *verification.ExecutionInputs) { v.Input = string([]byte{255}) },
		"invalid history": func(v *verification.ExecutionInputs) {
			v.History = []llm.Message{{Role: "user", Content: string([]byte{255})}}
		},
		"too many messages": func(v *verification.ExecutionInputs) { v.History = make([]llm.Message, 101) },
		"oversize":          func(v *verification.ExecutionInputs) { v.Input = string(bytes.Repeat([]byte("a"), 1<<20)) },
	} {
		t.Run(name, func(t *testing.T) {
			v := executionInputsFixture()
			mutate(&v)
			_, encodeErr := v.Encode()
			require.Error(t, encodeErr)
		})
	}
	_, err = verification.DecodeExecutionInputs(append(data, []byte("{}")...))
	require.Error(t, err)
	_, err = verification.DecodeExecutionInputs(bytes.Replace(data, []byte(`"schema_version":1`), []byte(`"schema_version":1,"unknown":true`), 1))
	require.Error(t, err)
}

func TestExecutionInputsReferenceMustMatchSupportingEvidence(t *testing.T) {
	_, report, _ := fixture(t)
	binding := report.Binding
	before, err := binding.Digest()
	require.NoError(t, err)
	ref := binding.Candidate
	ref.ID = id.New(evidence.PrefixArtifact)
	binding.Context.ExecutionInputs = &ref
	require.Error(t, binding.Validate(), "input reference must be retained and authorized as support evidence")
	binding.Evidence = []evidence.Ref{ref}
	require.NoError(t, binding.Validate())
	after, err := binding.Digest()
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	binding.Context.ExecutionInputs.Version = "different"
	require.Error(t, binding.Validate())
}
