package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

func TestPlanningHTTPUnavailableAndInvalidOperation(t *testing.T) {
	handler := New(nil, nil).Handler()
	for _, path := range []string{"/v1/planning/plans", "/v1/planning/plans/" + id.New(planning.PrefixPlan).String()} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		require.Contains(t, response.Body.String(), "planning_unavailable")
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/planning/commands", strings.NewReader(`{"operation":"claim"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
}

func TestPlanningHTTPCurrentAuthorityDenial(t *testing.T) {
	for _, err := range []error{evidence.ErrDenied, verification.ErrResultAccessDenied} {
		status, code, _ := planningFailure(err)
		require.Equal(t, http.StatusForbidden, status)
		require.Equal(t, "planning_denied", code)
	}
}
