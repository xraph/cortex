package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
)

// headerCountingRecorder counts WriteHeader calls, so a handler that
// writes its response and then has forge write it again shows up as a
// second header as well as a second body.
type headerCountingRecorder struct {
	*httptest.ResponseRecorder
	headers int
}

func (w *headerCountingRecorder) WriteHeader(code int) {
	w.headers++
	w.ResponseRecorder.WriteHeader(code)
}

// serveOnce serves one request and fails unless the response carries at
// most one WriteHeader and, when it has a body, exactly one JSON value.
func serveOnce(t *testing.T, handler http.Handler, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(testScopeContext(), method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := &headerCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(res, req)
	require.Equal(t, want, res.Code, "%s %s: %s", method, path, res.Body)
	require.LessOrEqual(t, res.headers, 1, "%s %s wrote its header %d times", method, path, res.headers)
	if res.Body.Len() > 0 {
		requireOneJSONValue(t, res.Body.Bytes(), method+" "+path)
	}
	return res.ResponseRecorder
}

func requireOneJSONValue(t *testing.T, body []byte, what string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	var first any
	require.NoError(t, decoder.Decode(&first), "%s: %s", what, body)
	var second any
	require.True(t, errors.Is(decoder.Decode(&second), io.EOF), "%s sent more than one JSON value: %s", what, body)
}

func bodyTestHandler(t *testing.T) http.Handler {
	t.Helper()
	eng, err := engine.New(engine.WithStore(contentStore(t)), engine.WithA2A(a2a.Options{}))
	require.NoError(t, err)
	return New(eng, nil).Handler()
}

func TestCreateResponseDecodesOnce(t *testing.T) {
	handler := bodyTestHandler(t)
	res := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(testScopeContext(), http.MethodPost, "/v1/agents", strings.NewReader(`{"name":"helper","system_prompt":"you help"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &created), res.Body.String())
	require.Equal(t, "helper", created["name"])
}

func TestListResponseDecodesOnce(t *testing.T) {
	handler := bodyTestHandler(t)
	serveOnce(t, handler, http.MethodPost, "/v1/agents", `{"name":"helper","system_prompt":"you help"}`, http.StatusCreated)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(testScopeContext(), http.MethodGet, "/v1/agents?limit=10&offset=0", nil))
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var listed struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &listed), res.Body.String())
	require.Len(t, listed.Items, 1)
}

// TestEveryHandlerWritesOneBody drives the success path of at least one
// route in every handler file and checks each response for a second
// header or a second JSON body.
func TestEveryHandlerWritesOneBody(t *testing.T) {
	handler := bodyTestHandler(t)
	steps := []struct {
		method, path, body string
		want               int
	}{
		// agent_handler.go
		{http.MethodPost, "/v1/agents", `{"name":"helper","system_prompt":"you help"}`, http.StatusCreated},
		{http.MethodGet, "/v1/agents?limit=10&offset=0", "", http.StatusOK},
		{http.MethodGet, "/v1/agents/helper", "", http.StatusOK},
		{http.MethodPut, "/v1/agents/helper", `{"system_prompt":"you help more"}`, http.StatusOK},
		{http.MethodPost, "/v1/agents/helper/clone", `{"new_name":"helper-two"}`, http.StatusCreated},
		{http.MethodPost, "/v1/agents/helper/preview-prompt", `{}`, http.StatusOK},
		// skill_handler.go
		{http.MethodPost, "/v1/skills", `{"name":"triage"}`, http.StatusCreated},
		{http.MethodGet, "/v1/skills?limit=10&offset=0", "", http.StatusOK},
		{http.MethodGet, "/v1/skills/triage", "", http.StatusOK},
		{http.MethodPut, "/v1/skills/triage", `{"description":"sorts things"}`, http.StatusOK},
		// trait_handler.go
		{http.MethodPost, "/v1/traits", `{"name":"careful"}`, http.StatusCreated},
		{http.MethodGet, "/v1/traits?limit=10&offset=0&category=general", "", http.StatusOK},
		{http.MethodGet, "/v1/traits/careful", "", http.StatusOK},
		{http.MethodPut, "/v1/traits/careful", `{"description":"checks twice"}`, http.StatusOK},
		// behavior_handler.go
		{http.MethodPost, "/v1/behaviors", `{"name":"escalate"}`, http.StatusCreated},
		{http.MethodGet, "/v1/behaviors?limit=10&offset=0", "", http.StatusOK},
		{http.MethodGet, "/v1/behaviors/escalate", "", http.StatusOK},
		{http.MethodPut, "/v1/behaviors/escalate", `{"description":"asks for help"}`, http.StatusOK},
		// persona_handler.go
		{http.MethodPost, "/v1/personas", `{"name":"support","identity":"a support engineer"}`, http.StatusCreated},
		{http.MethodGet, "/v1/personas?limit=10&offset=0", "", http.StatusOK},
		{http.MethodGet, "/v1/personas/support", "", http.StatusOK},
		{http.MethodPut, "/v1/personas/support", `{"identity":"a calm support engineer"}`, http.StatusOK},
		{http.MethodPost, "/v1/personas/support/clone", `{"new_name":"support-two"}`, http.StatusCreated},
		// session_handler.go
		{http.MethodPost, "/v1/agents/helper/sessions", `{"title":"first"}`, http.StatusCreated},
		{http.MethodGet, "/v1/agents/helper/sessions?limit=10&offset=0", "", http.StatusOK},
		{http.MethodGet, "/v1/agents/helper/sessions/count", "", http.StatusOK},
		// checkpoint_handler.go
		{http.MethodGet, "/v1/checkpoints?limit=10&offset=0", "", http.StatusOK},
		// run_handler.go
		{http.MethodGet, "/v1/runs?limit=10&offset=0", "", http.StatusOK},
		// orchestration_handler.go
		{http.MethodPost, "/v1/orchestrations", `{"name":"pair","strategy":"sequential","participants":[{"agent_name":"helper"},{"agent_name":"helper-two"}]}`, http.StatusCreated},
		{http.MethodGet, "/v1/orchestrations?limit=10&offset=0", "", http.StatusOK},
		{http.MethodGet, "/v1/orchestrations/pair", "", http.StatusOK},
		{http.MethodPut, "/v1/orchestrations/pair", `{"strategy":"sequential","participants":[{"agent_name":"helper"}]}`, http.StatusOK},
		{http.MethodGet, "/v1/orchestration-runs?limit=10&offset=0&status=completed", "", http.StatusOK},
		// a2a_handler.go
		{http.MethodGet, "/v1/a2a/conversations?limit=10&offset=0&status=open", "", http.StatusOK},
		// config_handler.go
		{http.MethodGet, "/v1/config", "", http.StatusOK},
		// tool_handler.go
		{http.MethodGet, "/v1/tools", "", http.StatusOK},
	}
	for _, step := range steps {
		serveOnce(t, handler, step.method, step.path, step.body, step.want)
	}

	// The routes below need the id of a session created above.
	created := serveOnce(t, handler, http.MethodPost, "/v1/agents/helper/sessions", `{"title":"second"}`, http.StatusCreated)
	var sess struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &sess))
	require.NotEmpty(t, sess.ID)
	// session_handler.go
	serveOnce(t, handler, http.MethodGet, "/v1/agents/helper/sessions/"+sess.ID, "", http.StatusOK)
	serveOnce(t, handler, http.MethodPut, "/v1/agents/helper/sessions/"+sess.ID, `{"title":"renamed"}`, http.StatusOK)
	// memory_handler.go
	serveOnce(t, handler, http.MethodGet, "/v1/agents/helper/memory?limit=10&session_id="+sess.ID, "", http.StatusOK)

	// a2a_handler.go: the conversation routes need the id a send returns.
	sent := serveOnce(t, handler, http.MethodPost, "/v1/agents/helper/messages", `{"from":"helper-two","content":"hello"}`, http.StatusCreated)
	var send struct {
		ConversationID string `json:"conversation_id"`
	}
	require.NoError(t, json.Unmarshal(sent.Body.Bytes(), &send))
	require.NotEmpty(t, send.ConversationID)
	serveOnce(t, handler, http.MethodGet, "/v1/a2a/conversations/"+send.ConversationID, "", http.StatusOK)
	serveOnce(t, handler, http.MethodGet, "/v1/agents/helper/inbox?limit=10&include_read=true&conversation_id="+send.ConversationID, "", http.StatusOK)
}
