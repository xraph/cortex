package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/verification"
)

func summaryContextFixture() engine.ContextBudgetConfig {
	config := contextBudgetFixture()
	config.Policy.Summary = cognitive.HistorySummaryPolicy{Revision: cognitive.ExtractiveHistoryRevision, MaxBytes: 1800, ExcerptBytes: 128, MaxMessages: 8}
	config.OptionalHistory = func(context.Context, cortex.Subject, [][]llm.Message) ([]int, error) { return []int{0}, nil }
	return config
}

func TestContextSummaryRequiresCapturedOriginals(t *testing.T) {
	_, err := engine.New(engine.WithContextBudget(summaryContextFixture()))
	require.ErrorIs(t, err, cortex.ErrContextUnavailable)
}

func TestContextSummaryCapturesExactHistoryAndResumes(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "approval resume", "revoked originals", "changed policy"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{approval: mode != "sync" && mode != "stream"}
			config := summaryContextFixture()
			classifications := 0
			config.OptionalHistory = func(context.Context, cortex.Subject, [][]llm.Message) ([]int, error) {
				classifications++
				return []int{0}, nil
			}
			opts := []engine.Option{engine.WithContextBudget(config), engine.WithTool(namedTool("probe"), noopHandler)}
			e := requiredToolEngine(t, f, client, auth, opts...)
			ag, overrides := requiredToolAgent(t, f)
			sid := id.NewSessionID()
			require.NoError(t, f.storage.CreateSession(f.ctx, &session.Session{ID: sid, AgentID: ag.ID}))
			original := []memory.Message{{Role: "user", Content: "Service enabled. " + strings.Repeat("private-old ", 500) + " Correction: service disabled."}, {Role: "assistant", Content: "Uncertain observation, check current state."}, {Role: "user", Content: "Required constraint: never change production."}, {Role: "assistant", Content: "Keep the requirement unchanged."}}
			require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, sid, original))
			overrides.SessionID = sid
			var current *run.Run
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "task", overrides, events))
				for event := range events {
					data, err := json.Marshal(event)
					require.NoError(t, err)
					require.NotContains(t, string(data), "private-old")
				}
				rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				current = rows[0]
			} else {
				var err error
				current, err = e.RunAgent(f.ctx, ag.Name, "task", overrides)
				require.NoError(t, err)
			}
			record := current.Provenance.ContextSelection
			require.NotNil(t, record)
			require.Equal(t, 2, record.SchemaVersion)
			require.NotNil(t, record.Summary)
			require.NoError(t, record.Validate())
			require.True(t, current.Provenance.UnattributedHistory, "summarizing never certifies unlinked history")
			intent, err := f.storage.GetRunCaptureIntent(f.ctx, current.ID)
			require.NoError(t, err)
			artifact, err := f.storage.LoadEvidence(f.ctx, *intent.Template.Binding.Context.ExecutionInputs)
			require.NoError(t, err)
			inputs, err := verification.DecodeExecutionInputs(artifact.Data)
			require.NoError(t, err)
			require.Len(t, inputs.OriginalHistory, 4)
			require.Len(t, inputs.History, 3)
			require.Equal(t, original[0].Content, inputs.OriginalHistory[0].Content)
			require.Contains(t, inputs.History[0].Content, "Service enabled.")
			require.Contains(t, inputs.History[0].Content, "Correction: service disabled.")
			require.Contains(t, inputs.History[0].Content, "Uncertain observation")
			require.Equal(t, original[2].Content, inputs.History[1].Content)
			require.Equal(t, original[3].Content, inputs.History[2].Content)
			require.NoError(t, current.ValidateExecutionInputs(inputs))
			wire, wireErr := json.Marshal(current.Provenance)
			require.NoError(t, wireErr)
			require.NotContains(t, string(wire), "private-old", "summary content stays in private evidence")
			for _, alteration := range []string{"span", "source", "role", "required", "schema"} {
				changed, decodeErr := verification.DecodeExecutionInputs(artifact.Data)
				require.NoError(t, decodeErr)
				switch alteration {
				case "span":
					changed.ContextSelection.Summary.Excerpts[0].Spans[0].Start++
				case "source":
					changed.OriginalHistory[0].Content = "replacement"
				case "role":
					changed.History[0].Role = "system"
				case "required":
					changed.History[1].Content = "weakened requirement"
				case "schema":
					changed.ContextSelection.SchemaVersion = 1
				}
				_, encodeErr := changed.Encode()
				require.ErrorIs(t, encodeErr, verification.ErrInvalid, alteration)
			}
			inputs.History[0].Content = "forged summary"
			_, err = inputs.Encode()
			require.ErrorIs(t, err, verification.ErrInvalid)
			if auth.approval {
				require.Equal(t, run.StatePaused, current.State)
				before := current.Revision
				if mode == "revoked originals" {
					require.NoError(t, f.storage.TombstoneEvidence(evidence.WithDeletionActor(f.ctx, "operator"), *intent.Template.Binding.Context.ExecutionInputs, "source withdrawn"))
				}
				if mode == "changed policy" {
					config.Policy.Summary.ExcerptBytes++
					opts[0] = engine.WithContextBudget(config)
				}
				require.NoError(t, f.storage.Close())
				f.storage = openSQLiteStoreForTest(t, f.path)
				resumed := &requiredToolClient{final: true}
				fresh := requiredToolEngine(t, f, resumed, auth, opts...)
				pending, readErr := fresh.ListPendingCheckpoints(f.ctx, nil)
				require.NoError(t, readErr)
				require.Len(t, pending, 1)
				err = fresh.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"})
				if mode == "revoked originals" || mode == "changed policy" {
					require.Error(t, err)
					require.Zero(t, resumed.calls)
					saved, loadErr := f.storage.GetRun(f.ctx, current.ID)
					require.NoError(t, loadErr)
					require.Equal(t, before, saved.Revision)
					return
				}
				require.NoError(t, err)
				current, err = f.storage.GetRun(f.ctx, current.ID)
				require.NoError(t, err)
			}
			require.Equal(t, verification.StatusPassed, current.VerificationStatus)
			require.Equal(t, 1, classifications)
		})
	}
}

func TestContextSummaryNeverDisplacesRequiredContent(t *testing.T) {
	f := newWorkerFixture(t)
	config := summaryContextFixture()
	config.Policy.MaxTokens = 900
	client := &requiredToolClient{final: true}
	auth := &requiredToolAuthority{}
	e := requiredToolEngine(t, f, client, auth, engine.WithContextBudget(config), engine.WithTool(namedTool("probe"), noopHandler))
	ag, overrides := requiredToolAgent(t, f)
	sid := id.NewSessionID()
	require.NoError(t, f.storage.CreateSession(f.ctx, &session.Session{ID: sid, AgentID: ag.ID}))
	overrides.SessionID = sid
	required := strings.Repeat("required ", 45)
	require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: strings.Repeat("old ", 1000)}, {Role: "user", Content: required}}))
	current, err := e.RunAgent(f.ctx, ag.Name, "task", overrides)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, current.VerificationStatus)
	require.Nil(t, current.Provenance.ContextSelection.Summary, "summary is omitted when full request cannot admit it")
	require.Equal(t, required, client.last.Messages[0].Content)
}
