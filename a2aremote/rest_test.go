package a2aremote

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func restCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Buffer
	if body == "" {
		reader = bytes.NewBufferString("")
	} else {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: body is not JSON: %q", method, path, rec.Body.String())
		}
	}
	return rec.Code, out
}

// The paths are the protocol's own, from the http annotations on the
// normative proto. A colon verb is unusual in Go routing and entirely
// ordinary in an AIP-style API.
func TestRESTSendMessage(t *testing.T) {
	gw := newFakeGateway()
	h := testService(t, gw, okResolver()).RESTHandler()

	code, body := restCall(t, h, http.MethodPost, "/worker/message:send",
		`{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}}`)

	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if gw.calls() != 1 {
		t.Fatalf("gateway called %d times, want 1", gw.calls())
	}
	// The tenant comes from the path, which is where the protocol puts it.
	if gw.params().Receivers[0].Agent != "worker" {
		t.Fatalf("receiver = %+v, want the tenant from the path", gw.params().Receivers)
	}
}

func TestRESTGetTask(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "done"})
	h := testService(t, gw, okResolver()).RESTHandler()

	code, body := restCall(t, h, http.MethodGet, "/worker/tasks/"+runID.String(), "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	if body["id"] != runID.String() {
		t.Fatalf("id = %v", body["id"])
	}
}

func TestRESTListTasks(t *testing.T) {
	gw := newFakeGateway()
	gw.addRun(&run.Run{ID: id.NewAgentRunID(), State: run.StateRunning})
	h := testService(t, gw, okResolver()).RESTHandler()

	code, body := restCall(t, h, http.MethodGet, "/worker/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if _, ok := body["tasks"]; !ok {
		t.Fatalf("body = %+v, want a tasks list", body)
	}
}

func TestRESTCancelTask(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateRunning})
	h := testService(t, gw, okResolver()).RESTHandler()

	code, body := restCall(t, h, http.MethodPost, "/worker/tasks/"+runID.String()+":cancel", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", code, body)
	}
	status, _ := body["status"].(map[string]any)
	if status["state"] != string(TaskStateCanceled) {
		t.Fatalf("state = %v, want canceled", status["state"])
	}
}

// An error keeps its meaning across the binding. REST says it with a
// status code as well as a body, because that is what a REST client
// reads first.
func TestRESTErrorsCarryTheProtocolStatus(t *testing.T) {
	gw := newFakeGateway()
	h := testService(t, gw, okResolver()).RESTHandler()

	code, body := restCall(t, h, http.MethodGet, "/worker/tasks/arun_notreal", "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a task that is not there", code)
	}
	e, _ := body["error"].(map[string]any)
	if e["code"] != float64(CodeTaskNotFound) {
		t.Fatalf("error = %+v, want the protocol's own code alongside the status", e)
	}
}

func TestRESTUnauthenticatedIs401(t *testing.T) {
	h := testService(t, newFakeGateway(), staticResolver{err: errTest}).RESTHandler()
	code, _ := restCall(t, h, http.MethodGet, "/worker/tasks", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestRESTUnsupportedOperations(t *testing.T) {
	h := testService(t, newFakeGateway(), okResolver()).RESTHandler()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/worker/message:stream"},
		{http.MethodGet, "/worker/tasks/arun_1:subscribe"},
		{http.MethodGet, "/worker/extendedAgentCard"},
		{http.MethodPost, "/worker/tasks/arun_1/pushNotificationConfigs"},
	} {
		code, body := restCall(t, h, tc.method, tc.path, "{}")
		if code == http.StatusOK {
			t.Errorf("%s %s answered 200 for something not implemented: %+v", tc.method, tc.path, body)
		}
		e, _ := body["error"].(map[string]any)
		if e == nil {
			t.Errorf("%s %s: no error body: %+v", tc.method, tc.path, body)
		}
	}
}

// The two bindings share one Service, so identical semantics must give
// identical outcomes. That is the property the shared service exists for,
// which makes it worth asserting rather than assuming.
func TestRESTAndJSONRPCAgree(t *testing.T) {
	runID := id.NewAgentRunID()

	gwRPC := newFakeGateway()
	gwRPC.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "same answer"})
	rpcResp := post(t, testService(t, gwRPC, okResolver()).JSONRPCHandler(),
		`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"tenant":"worker","id":"`+runID.String()+`"}}`)

	gwREST := newFakeGateway()
	gwREST.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "same answer"})
	_, restBody := restCall(t, testService(t, gwREST, okResolver()).RESTHandler(),
		http.MethodGet, "/worker/tasks/"+runID.String(), "")

	rpcResult, _ := rpcResp["result"].(map[string]any)
	rpcJSON, _ := json.Marshal(rpcResult)
	restJSON, _ := json.Marshal(restBody)

	// Timestamps differ by construction, so the comparison is on the
	// parts that carry meaning.
	var a, b Task
	_ = json.Unmarshal(rpcJSON, &a)
	_ = json.Unmarshal(restJSON, &b)
	a.Status.Timestamp, b.Status.Timestamp = "", ""

	if a.ID != b.ID || a.Status.State != b.Status.State || len(a.Artifacts) != len(b.Artifacts) {
		t.Fatalf("the bindings disagree:\n  jsonrpc: %+v\n  rest:    %+v", a, b)
	}
}
