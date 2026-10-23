package a2aremote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

type handoffModel struct{ calls atomic.Int32 }

func (m *handoffModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.calls.Add(1)
	return nil, errors.New("handoff must not enter model")
}
func (m *handoffModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	m.calls.Add(1)
	return nil, errors.New("handoff must not enter model")
}

func TestHandoffHTTPAndSSEUseHostAdmissionWithoutAgentExecution(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	model := &handoffModel{}
	_, storage := newEngine(ctx, t, "handoffs", model, "worker")
	deadline := time.Now().UTC().Add(time.Hour)
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "checked report", Criteria: []verification.Criterion{{ID: "checked", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100, MaxToolCalls: 2, Deadline: deadline}
	plan, err := planning.Seal(planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"worker"}, Actor: "host", Reason: "requested", Tasks: []planning.Task{{ID: id.New(planning.PrefixTask), Instruction: "produce checked report", Owner: "worker", Criteria: []string{"checked"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "report", MediaType: "text/plain", MaxBytes: 1024}}}}, planning.Limits{})
	require.NoError(t, err)
	require.NoError(t, storage.CreatePlan(ctx, plan))
	h, err := coordination.SealHandoff(coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Fence: 1, Scope: scope, PlanID: plan.ID, PlanRevision: 1, PlanDigest: plan.Digest, TaskID: plan.Tasks[0].ID, ParentRunID: id.NewAgentRunID(), Sender: coordination.Identity{Agent: "planner", Node: "peer-a", Principal: "planner-principal"}, Receiver: coordination.Identity{Agent: "worker", Node: "peer-b", Principal: "worker-principal"}, Purpose: coordination.HandoffWork, GrantRef: "grant-1", Authority: planning.Authority{Budget: budget}, ContractDigest: contract.Digest, Criteria: []string{"checked"}, Deadline: deadline})
	require.NoError(t, err)
	current := coordination.HandoffAuthority{Sender: h.Sender, Receiver: h.Receiver, AssignmentID: h.AssignmentID, ParentRunID: h.ParentRunID, Fence: 1, GrantRef: h.GrantRef, Ceiling: h.Authority}
	accepted := make(map[string]bool)
	calls := 0
	receiver, err := coordination.NewHandoffReceiver(coordination.HandoffReceiverConfig{MaxConcurrent: 1, Resolve: func(resolveCtx context.Context, proposal coordination.Handoff) (coordination.HandoffContext, error) {
		storedPlan, readErr := storage.GetPlan(resolveCtx, proposal.PlanID)
		if readErr != nil {
			return coordination.HandoffContext{}, readErr
		}
		return coordination.HandoffContext{Plan: storedPlan, Authority: current}, nil
	}, Accept: func(_ context.Context, received coordination.Handoff) error {
		calls++
		accepted[received.Digest] = true
		return nil
	}})
	require.NoError(t, err)
	handler, err := coordination.NewA2AHandoffHandler("peer-b", receiver, nil)
	require.NoError(t, err)
	eng, err := engine.New(engine.WithStore(storage), engine.WithLLM(model), engine.WithA2AHandoffHandler(handler), engine.WithA2A(a2a.Options{}))
	require.NoError(t, err)
	svc := a2aremote.NewService(a2aremote.EngineGateway(eng), a2aremote.ResolverFunc(func(_ context.Context, credentials a2aremote.Credentials) (a2aremote.Peer, error) {
		if credentials.Header("authorization") != "Bearer fixture" {
			return a2aremote.Peer{}, errors.New("denied")
		}
		return a2aremote.Peer{Node: "peer-a", Scope: scope}, nil
	}), a2aremote.Options{Streaming: true})
	require.NotNil(t, svc)
	server := httptest.NewServer(svc.RESTHandler())
	defer server.Close()
	params, err := coordination.HandoffSendParams(h, "peer-a")
	require.NoError(t, err)
	envelope := &a2a.Envelope{ID: id.NewMessageID(), Scope: scope, Sender: params.Sender, Receivers: params.Receivers, Performative: params.Performative, Content: params.Content, Protocol: params.Protocol, Language: params.Language, Encoding: params.Encoding, ReplyBy: params.ReplyBy, OriginRunID: params.OriginRunID}
	request := a2aremote.SendMessageRequest{SenderName: "planner", Message: a2aremote.MessageFromEnvelope(envelope)}
	send := func(path, credential string, req a2aremote.SendMessageRequest) (int, string) {
		t.Helper()
		data, marshalErr := json.Marshal(req)
		require.NoError(t, marshalErr)
		httpRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(data))
		require.NoError(t, requestErr)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Authorization", credential)
		response, doErr := server.Client().Do(httpRequest)
		require.NoError(t, doErr)
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		return response.StatusCode, string(body)
	}
	for _, path := range []string{"/worker/message:send", "/worker/message:stream"} {
		code, body := send(path, "Bearer fixture", request)
		require.Equal(t, http.StatusOK, code, body)
		require.Contains(t, body, "queued")
		require.NotContains(t, body, "TASK_STATE_COMPLETED")
		_, err = eng.A2A().Drain(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, 2, calls, "host must see retries and deduplicate assignments")
	require.Len(t, accepted, 1)
	require.Zero(t, model.calls.Load())
	code, _ := send("/worker/message:send", "Bearer invalid", request)
	require.NotEqual(t, http.StatusOK, code)
	request.SenderName = "forged"
	code, _ = send("/worker/message:send", "Bearer fixture", request)
	require.NotEqual(t, http.StatusOK, code)
	_, err = eng.A2A().Drain(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Zero(t, model.calls.Load())
}
