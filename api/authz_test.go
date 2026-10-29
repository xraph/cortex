package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/scopespy"
)

// recordingAuthorizer records every Operation it is asked about and
// answers each with deny. DecidedBy answers with name.
type recordingAuthorizer struct {
	mu   sync.Mutex
	ops  []Operation
	deny error
	name string
}

func (r *recordingAuthorizer) Authorize(_ context.Context, op Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
	return r.deny
}

func (r *recordingAuthorizer) DecidedBy(context.Context) (string, error) {
	return r.name, nil
}

func (r *recordingAuthorizer) seen() []Operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Operation(nil), r.ops...)
}

// concretePath fills every path parameter of a route pattern with a value
// that parses as nothing in particular, which is all a refused request
// needs: the guard answers before anything reads the parameters.
func concretePath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ":") || strings.HasPrefix(part, "*") || strings.HasPrefix(part, "{") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

func testScopeContext() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}})
}

func TestAuthorizerGuardsEveryRoute(t *testing.T) {
	eng, err := engine.New(engine.WithStore(scopespy.New()))
	require.NoError(t, err)
	authz := &recordingAuthorizer{deny: errors.New("SECRET grant missing")}
	router := forge.NewRouter()
	handler := New(eng, router, WithAuthorizer(authz)).Handler()

	routes := router.Routes()
	require.NotEmpty(t, routes)
	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		require.NotEmpty(t, route.OperationID, "route %s %s has no operation id", route.Method, route.Path)
		registered[route.OperationID] = true

		before := len(authz.seen())
		res := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(testScopeContext(), route.Method, concretePath(route.Path), nil)
		handler.ServeHTTP(res, req)
		require.Equal(t, http.StatusForbidden, res.Code, "%s %s (%s): %s", route.Method, route.Path, route.OperationID, res.Body)
		require.NotContains(t, res.Body.String(), "SECRET", "%s leaked the authorizer's reason", route.OperationID)

		seen := authz.seen()
		require.Len(t, seen, before+1, "%s %s asked the authorizer %d times", route.Method, route.Path, len(seen)-before)
		op := seen[len(seen)-1]
		require.Equal(t, route.OperationID, op.ID, "%s %s", route.Method, route.Path)
		write := route.Method != http.MethodGet && route.Method != http.MethodHead
		require.Equal(t, write, op.Write, "%s %s (%s)", route.Method, route.Path, route.OperationID)
	}

	asked := make(map[string]bool)
	for _, op := range authz.seen() {
		asked[op.ID] = true
	}
	require.Equal(t, sortedKeys(registered), sortedKeys(asked))
	t.Logf("guarded %d routes", len(routes))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAuthorizerRefusalStatus pins the status a refusal maps to: a
// cortex not-found or scope error keeps the status it has everywhere
// else in the module, and anything else is a 403 that never repeats the
// authorizer's reason.
func TestAuthorizerRefusalStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		deny error
		want int
	}{
		{name: "allowed", deny: nil, want: http.StatusOK},
		{name: "refused", deny: errors.New("SECRET role"), want: http.StatusForbidden},
		{name: "not found", deny: fmtWrap(cortex.ErrAgentNotFound), want: http.StatusNotFound},
		{name: "no scope", deny: fmtWrap(cortex.ErrNoScope), want: http.StatusPreconditionFailed},
		{name: "invalid scope", deny: fmtWrap(cortex.ErrInvalidScope), want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := engine.New(engine.WithStore(scopespy.New()))
			require.NoError(t, err)
			handler := New(eng, nil, WithAuthorizer(&recordingAuthorizer{deny: tc.deny})).Handler()
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, httptest.NewRequestWithContext(testScopeContext(), http.MethodGet, "/v1/tools", nil))
			require.Equal(t, tc.want, res.Code, res.Body.String())
			require.NotContains(t, res.Body.String(), "SECRET")
		})
	}
}

func fmtWrap(err error) error {
	return errors.Join(errors.New("SECRET lookup"), err)
}

// hostCheckpoint writes a pending checkpoint the way a host does, with no
// suspension behind it, so deciding it only records the decision.
func hostCheckpoint(t *testing.T, st *scopespy.Spy) *checkpoint.Checkpoint {
	t.Helper()
	cp := &checkpoint.Checkpoint{
		Entity:  cortex.NewEntity(),
		ID:      id.NewCheckpointID(),
		RunID:   id.NewAgentRunID(),
		AgentID: id.NewAgentID(),
		Scope:   cortex.ScopeFromContext(testScopeContext()),
		Reason:  "the host wants a second opinion",
		State:   "pending",
	}
	require.NoError(t, st.CreateCheckpoint(testScopeContext(), cp))
	return cp
}

func resolveOverHTTP(t *testing.T, handler http.Handler, cpID id.CheckpointID, body string) {
	t.Helper()
	res := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(testScopeContext(), http.MethodPost, "/v1/checkpoints/"+cpID.String()+"/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(res, req)
	require.Equal(t, http.StatusNoContent, res.Code, res.Body.String())
}

func TestResolveCheckpointDerivesDecidedBy(t *testing.T) {
	st := scopespy.New()
	eng, err := engine.New(engine.WithStore(st))
	require.NoError(t, err)
	cp := hostCheckpoint(t, st)
	handler := New(eng, nil, WithAuthorizer(&recordingAuthorizer{name: "user:alice"})).Handler()

	resolveOverHTTP(t, handler, cp.ID, `{"decision":"approved","decided_by":"mallory"}`)

	decided, err := st.GetCheckpoint(testScopeContext(), cp.ID)
	require.NoError(t, err)
	require.NotNil(t, decided.Decision)
	require.Equal(t, "user:alice", decided.Decision.DecidedBy)
}

func TestResolveCheckpointWithoutAuthorizerKeepsBodyDecidedBy(t *testing.T) {
	st := scopespy.New()
	eng, err := engine.New(engine.WithStore(st))
	require.NoError(t, err)
	cp := hostCheckpoint(t, st)
	handler := New(eng, nil).Handler()

	resolveOverHTTP(t, handler, cp.ID, `{"decision":"approved","decided_by":"bob"}`)

	decided, err := st.GetCheckpoint(testScopeContext(), cp.ID)
	require.NoError(t, err)
	require.NotNil(t, decided.Decision)
	require.Equal(t, "bob", decided.Decision.DecidedBy)
}

// TestCheckpointOpenAPIOmitsDecidedByPrincipal keeps the decider's
// principal out of the published schema as well as the response body.
func TestCheckpointOpenAPIOmitsDecidedByPrincipal(t *testing.T) {
	eng, err := engine.New()
	require.NoError(t, err)
	router := forge.NewRouter(forge.WithOpenAPI(forge.OpenAPIConfig{Title: "test", Version: "1"}))
	require.NoError(t, New(eng, router).RegisterRoutes(router))
	spec := router.OpenAPISpec()
	require.NotNil(t, spec)
	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	require.Contains(t, string(raw), "decided_at", "no schema describes a checkpoint decision")
	require.NotContains(t, string(raw), "decided_by_principal")
}
