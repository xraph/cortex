package verification_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/verification"
)

func transcriptFixture(t *testing.T) (verification.ExecutionInputs, verification.ExecutionTranscript) {
	t.Helper()
	inputs := executionInputsFixture()
	data, err := inputs.Encode()
	require.NoError(t, err)
	return inputs, verification.ExecutionTranscript{SchemaVersion: 1, RunID: inputs.RunID, InputsDigest: evidence.Digest(data), Usage: execution.Usage{Steps: 2, ToolCalls: 1, Tokens: 10, ReportedTokens: 10}, Messages: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Name: "probe", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "call", Content: "checked result"},
		{Role: "assistant", Content: "answer"},
	}, Tools: []verification.ToolResult{{CallID: "call", Outcome: verification.ToolCompleted, Source: verification.ToolLocal}}}
}

func TestExecutionTranscriptBindsResultsAndUsage(t *testing.T) {
	inputs, trace := transcriptFixture(t)
	require.NoError(t, trace.ValidateFor(inputs, "answer", trace.Usage))
	data, err := trace.Encode()
	require.NoError(t, err)
	restored, err := verification.DecodeExecutionTranscript(data)
	require.NoError(t, err)
	require.Equal(t, trace, restored)
	for name, mutate := range map[string]func(*verification.ExecutionTranscript){
		"missing result":    func(v *verification.ExecutionTranscript) { v.Messages = append(v.Messages[:1], v.Messages[2:]...) },
		"unknown result":    func(v *verification.ExecutionTranscript) { v.Messages[1].ToolCallID = "other" },
		"missing outcome":   func(v *verification.ExecutionTranscript) { v.Tools = nil },
		"duplicate outcome": func(v *verification.ExecutionTranscript) { v.Tools = append(v.Tools, v.Tools[0]) },
		"unknown outcome":   func(v *verification.ExecutionTranscript) { v.Tools[0].Outcome = "claimed" },
		"changed final":     func(v *verification.ExecutionTranscript) { v.Messages[2].Content = "other" },
		"changed input":     func(v *verification.ExecutionTranscript) { v.InputsDigest = evidence.Digest(nil) },
		"usage":             func(v *verification.ExecutionTranscript) { v.Usage.Steps++ },
		"invented tool":     func(v *verification.ExecutionTranscript) { v.Messages[0].ToolCalls[0].Name = "unregistered" },
		"invalid text":      func(v *verification.ExecutionTranscript) { v.Messages[1].Content = string([]byte{255}) },
	} {
		t.Run(name, func(t *testing.T) {
			in, v := transcriptFixture(t)
			usage := v.Usage
			mutate(&v)
			require.Error(t, v.ValidateFor(in, "answer", usage))
		})
	}
}

func TestTranscriptRecordsDeniedUnknownCall(t *testing.T) {
	inputs, trace := transcriptFixture(t)
	trace.Messages[0].ToolCalls[0].Name = "unregistered"
	trace.Tools[0].Outcome = verification.ToolDenied
	trace.Usage.ToolCalls = 0
	require.NoError(t, trace.ValidateFor(inputs, "answer", trace.Usage))
	trace.Tools[0].Source = verification.ToolExternal
	require.Error(t, trace.ValidateFor(inputs, "answer", trace.Usage))
}

func TestCaptureIntentBindsTranscriptReservation(t *testing.T) {
	inputs, trace := transcriptFixture(t)
	intent := captureIntentFixture(t)
	intent.SchemaVersion = 2
	intent.Template.Binding.RunID = inputs.RunID
	inputData, err := inputs.Encode()
	require.NoError(t, err)
	inputRef := intent.Template.Binding.Candidate
	inputRef.ID = id.New(evidence.PrefixArtifact)
	inputRef.SHA256 = evidence.Digest(inputData)
	inputRef.Bytes = int64(len(inputData))
	intent.Template.Binding.Context.PreparedDigest = inputs.PreparedDigest
	intent.Template.Binding.Context.ExecutionInputs = &inputRef
	intent.Template.Binding.Evidence = []evidence.Ref{inputRef}
	ref := intent.Template.Binding.Candidate
	ref.ID = id.New(evidence.PrefixArtifact)
	intent.Transcript = &ref
	intent.MaxTranscriptBytes = 1 << 20
	intent, err = verification.SealCaptureIntent(intent)
	require.NoError(t, err)
	_, err = intent.JobFor(2, "answer")
	require.Error(t, err)
	data, err := trace.Encode()
	require.NoError(t, err)
	job, err := intent.JobFor(2, "answer", data)
	require.NoError(t, err)
	require.Equal(t, ref.ID, job.Binding.Context.ExecutionTranscript.ID)
	require.Equal(t, evidence.Digest(data), job.Binding.Context.ExecutionTranscript.SHA256)
	require.Equal(t, *job.Binding.Context.ExecutionTranscript, job.Binding.Evidence[len(job.Binding.Evidence)-1])
	require.Equal(t, evidence.Digest(nil), intent.Transcript.SHA256)
	_, err = intent.JobFor(2, "changed candidate", data)
	require.Error(t, err)
	intent.MaxTranscriptBytes = 1
	intent, err = verification.SealCaptureIntent(intent)
	require.NoError(t, err)
	_, err = intent.JobFor(2, "answer", data)
	require.ErrorIs(t, err, evidence.ErrTooLarge)
}

func TestExecutionTranscriptRejectsAmbiguousEncoding(t *testing.T) {
	_, trace := transcriptFixture(t)
	data, err := trace.Encode()
	require.NoError(t, err)
	for _, bad := range [][]byte{
		append([]byte(" "), data...),
		append([]byte(`{"future":true,`), data[1:]...),
		append([]byte(`{"schema_version":1,`), data[1:]...),
		[]byte(`null`),
	} {
		_, err = verification.DecodeExecutionTranscript(bad)
		require.Error(t, err)
	}
	trace.Messages[1].Content = strings.Repeat("x", 1<<20)
	_, err = trace.Encode()
	require.Error(t, err)
}
