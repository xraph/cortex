package cortex_test

import (
	"encoding/json"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/run"
)

func TestRun_CarriesScope(t *testing.T) {
	r := run.Run{Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}}
	if got := r.Scope.Canonical(); got != "workspace=ws_x" {
		t.Errorf("run scope = %q, want workspace=ws_x", got)
	}
}

func TestAgentConfig_ScopeSurvivesJSON(t *testing.T) {
	c := agent.Config{
		Name:  "assistant",
		Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}},
	}
	blob, err := json.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back agent.Config
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Scope.Canonical() != c.Scope.Canonical() {
		t.Errorf("scope after round trip = %q, want %q", back.Scope.Canonical(), c.Scope.Canonical())
	}
}
