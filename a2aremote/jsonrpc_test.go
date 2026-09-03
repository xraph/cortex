package a2aremote

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func post(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.Len() == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	return out
}

func errorCode(t *testing.T, resp map[string]any) int {
	t.Helper()
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no error: %+v", resp)
	}
	code, ok := e["code"].(float64)
	if !ok {
		t.Fatalf("error carries no code: %+v", e)
	}
	return int(code)
}

func TestJSONRPCSendMessage(t *testing.T) {
	gw := newFakeGateway()
	h := testService(t, gw, okResolver()).JSONRPCHandler()

	resp := post(t, h, `{
		"jsonrpc":"2.0","id":1,"method":"SendMessage",
		"params":{"tenant":"worker","message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}}
	}`)

	if resp["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}
	if resp["id"] != float64(1) {
		t.Errorf("id = %v, want it echoed", resp["id"])
	}
	if _, ok := resp["result"]; !ok {
		t.Fatalf("no result: %+v", resp)
	}
	if gw.calls() != 1 {
		t.Errorf("gateway called %d times, want 1", gw.calls())
	}
}

func TestJSONRPCGetTask(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "done"})
	h := testService(t, gw, okResolver()).JSONRPCHandler()

	resp := post(t, h, `{"jsonrpc":"2.0","id":"abc","method":"GetTask","params":{"tenant":"worker","id":"`+runID.String()+`"}}`)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", resp)
	}
	if result["id"] != runID.String() {
		t.Errorf("task id = %v", result["id"])
	}
}

func TestJSONRPCUnknownMethod(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()
	resp := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"DoTheThing","params":{}}`)
	if got := errorCode(t, resp); got != CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", got, CodeMethodNotFound)
	}
}

// The 0.x spellings must not work: answering them would leave a client
// talking a protocol version this server does not actually implement.
func TestJSONRPCRefusesThe0xMethodNames(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()
	for _, method := range []string{"message/send", "tasks/get", "tasks/cancel"} {
		resp := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`)
		if got := errorCode(t, resp); got != CodeMethodNotFound {
			t.Errorf("%s: code = %d, want method not found", method, got)
		}
	}
}

func TestJSONRPCMalformedBody(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()
	resp := post(t, h, `{not json`)
	if got := errorCode(t, resp); got != CodeParse {
		t.Fatalf("code = %d, want %d", got, CodeParse)
	}
}

func TestJSONRPCWrongVersionField(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()
	resp := post(t, h, `{"jsonrpc":"1.0","id":1,"method":"SendMessage","params":{}}`)
	if got := errorCode(t, resp); got != CodeInvalidRequest {
		t.Fatalf("code = %d, want %d", got, CodeInvalidRequest)
	}
}

// A service error keeps its code through the binding. A binding that
// flattened everything to -32603 would leave a peer unable to tell "no
// such task" from "the server broke".
func TestJSONRPCServiceErrorKeepsItsCode(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()
	resp := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"tenant":"worker","id":"arun_notreal"}}`)
	if got := errorCode(t, resp); got != CodeTaskNotFound {
		t.Fatalf("code = %d, want %d", got, CodeTaskNotFound)
	}
}

func TestJSONRPCUnauthenticated(t *testing.T) {
	h := testService(t, newFakeGateway(), staticResolver{err: errTest}).JSONRPCHandler()
	resp := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"tenant":"worker"}}`)
	if got := errorCode(t, resp); got != CodeInvalidRequest {
		t.Fatalf("code = %d, want a refusal", got)
	}
}

// A notification carries no id and gets no response, per JSON-RPC.
func TestJSONRPCNotificationGetsNoBody(t *testing.T) {
	gw := newFakeGateway()
	h := testService(t, gw, okResolver()).JSONRPCHandler()

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(
		`{"jsonrpc":"2.0","method":"SendMessage","params":{"tenant":"worker","message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for a notification", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a notification got a body: %q", rec.Body.String())
	}
	if gw.calls() != 1 {
		t.Errorf("a notification must still be acted on, calls = %d", gw.calls())
	}
}

func TestJSONRPCRejectsGET(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestJSONRPCRejectsAnUnknownProtocolVersion(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).JSONRPCHandler()

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"tenant":"worker"}}`))
	req.Header.Set("A2A-Version", "0.1.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	if got := errorCode(t, resp); got != CodeVersionNotSupported {
		t.Fatalf("code = %d, want %d", got, CodeVersionNotSupported)
	}
}

// The credentials a resolver sees must be the request's own.
func TestJSONRPCPassesCredentialsToTheResolver(t *testing.T) {
	var seen Credentials
	res := ResolverFunc(func(_ context.Context, cred Credentials) (Peer, error) {
		seen = cred
		return Peer{Node: "peer.example", Scope: testScope()}, nil
	})
	h := testService(t, newFakeGateway(), res).JSONRPCHandler()

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{"tenant":"worker"}}`))
	req.Header.Set("Authorization", "Bearer sekrit")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen.Header("authorization") != "Bearer sekrit" {
		t.Fatalf("the resolver did not see the request's credentials: %+v", seen.Headers)
	}
	if seen.RemoteAddr == "" {
		t.Error("the resolver should see where the request came from")
	}
}

func TestCardHandlerServesExposedAgentsOnly(t *testing.T) {
	gw := newFakeGateway()
	gw.agents["secret"] = &agent.Config{ID: id.NewAgentID(), Name: "secret"}
	svc := NewService(gw, okResolver(), Options{
		Card:         CardOptions{BaseURL: "https://cortex.example/a2a"},
		Exposed:      []string{"worker"},
		DefaultAgent: "worker",
	})
	h := svc.CardHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/worker"+WellKnownCardPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("exposed agent: status = %d, body = %s", rec.Code, rec.Body)
	}

	// A card is public, so exposure is opt-in. An agent nobody exposed is
	// not merely undocumented, it is not there.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/secret"+WellKnownCardPath, http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexposed agent: status = %d, want 404", rec.Code)
	}

	// The default agent is also reachable at the root path, so plain
	// discovery finds something.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, WellKnownCardPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("root card: status = %d", rec.Code)
	}
}
