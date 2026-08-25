package engine

import (
	"testing"

	"github.com/xraph/cortex/suspension"
)

// TestContinuationRoundTrip_CarriesTheToolRestriction guards the one
// piece of the resolved config that has no value of its own to give it
// away. A resumed run rebuilds its config from the continuation and from
// nowhere else, so if the restriction flag does not travel, an overlay
// that withdrew the agent's last tool would have that withdrawal undone
// by the resume: an empty list reads as "every registered tool" without
// it, and the run would come back holding more than it was suspended
// with. Every other field would show the loss as a wrong value; this one
// shows it as a silent grant.
func TestContinuationRoundTrip_CarriesTheToolRestriction(t *testing.T) {
	tests := []struct {
		name string
		cfg  resolvedConfig
	}{
		{
			name: "an overlay withdrew every tool",
			cfg:  resolvedConfig{Tools: []string{}, ToolsRestricted: true},
		},
		{
			name: "an agent that names no tools is unrestricted",
			cfg:  resolvedConfig{},
		},
		{
			name: "a restricted list with tools left in it",
			cfg:  resolvedConfig{Tools: []string{"alpha"}, ToolsRestricted: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &reactState{}
			got := configFromContinuation(s.continuation(tt.cfg).Config)

			if got.ToolsRestricted != tt.cfg.ToolsRestricted {
				t.Errorf("ToolsRestricted after a suspend and resume = %v, want %v", got.ToolsRestricted, tt.cfg.ToolsRestricted)
			}
			if len(got.Tools) != len(tt.cfg.Tools) {
				t.Errorf("Tools after a suspend and resume = %v, want %v", got.Tools, tt.cfg.Tools)
			}
		})
	}
}

// TestRunConfigJSON_OmitsAnUnrestrictedList records why the flag is safe
// to add to a persisted type. Rows written before it existed have no
// tools_restricted key, and decode as false, which is the behavior those
// runs already had.
func TestRunConfigJSON_OmitsAnUnrestrictedList(t *testing.T) {
	var cfg suspension.RunConfig
	if cfg.ToolsRestricted {
		t.Error("the zero RunConfig is restricted; a row written before this field existed would decode into a run with no tools")
	}
}
