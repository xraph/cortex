package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/tool"
)

type weatherIn struct {
	City string `json:"city" jsonschema:"the city to look up"`
}

func toolCatalogEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng, err := engine.New(
		tool.Register("weather", "Reads the weather for a city.",
			func(context.Context, cortex.Invocation, weatherIn) (string, error) { return "sunny", nil },
			tool.RetrySafe()),
		engine.WithExternalTool(llm.Tool{Name: "deploy", Description: "Deploys a service; the host runs it.", Parameters: map[string]any{"type": "object"}}),
	)
	require.NoError(t, err)
	return eng
}

func getTools(t *testing.T, eng *engine.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	res := httptest.NewRecorder()
	New(eng, nil).Handler().ServeHTTP(res, httptest.NewRequestWithContext(testScopeContext(), http.MethodGet, path, nil))
	return res
}

func TestListToolsReturnsEngineTools(t *testing.T) {
	res := getTools(t, toolCatalogEngine(t), "/v1/tools")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Equal(t, []map[string]any{
		{"name": "weather", "description": "Reads the weather for a city.", "kind": "registered", "retry_safe": true},
		{"name": "deploy", "description": "Deploys a service; the host runs it.", "kind": "external", "retry_safe": false},
	}, body.Items)
}

func TestGetToolSchemaReturnsParameters(t *testing.T) {
	eng := toolCatalogEngine(t)
	res := getTools(t, eng, "/v1/tools/weather/schema")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var want any
	for _, info := range eng.Tools() {
		if info.Def.Name == "weather" {
			raw, err := json.Marshal(info.Def.Parameters)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &want))
		}
	}
	require.NotNil(t, want)
	var got any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &got))
	require.Equal(t, want, got)
	require.Contains(t, res.Body.String(), `"city"`)

	ghost := getTools(t, eng, "/v1/tools/ghost/schema")
	require.Equal(t, http.StatusNotFound, ghost.Code, ghost.Body.String())
}
