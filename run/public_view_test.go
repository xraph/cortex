package run

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestPublicRunViewWithholdsPrivateMaterial(t *testing.T) {
	for _, mode := range []string{"required", "committed", "configured", "invalid ref"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			r := &Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: StateCompleted, Revision: 7,
				Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}},
				Input: "SECRET input", Output: "SECRET candidate", Error: "SECRET error", FinishReason: "SECRET reason", PersonaRef: "SECRET persona",
				Metadata: map[string]any{"untrusted": "SECRET metadata"}, StartedAt: &now, CompletedAt: &now,
				Provenance: ExecutionProvenance{Diagnostics: []string{"SECRET diagnostic"}, AcceptanceRef: "SECRET contract"},
			}
			switch mode {
			case "required":
				r.Provenance.VerificationRequired = true
			case "committed":
				r.ResultRef = id.New(verification.PrefixResult).String()
			case "invalid ref":
				r.ResultRef = "SECRET invalid"
			}
			out := PublicView(r, mode == "configured")
			body, err := json.Marshal(out)
			if err != nil || strings.Contains(string(body), "SECRET") || out.ID != r.ID || out.Revision != r.Revision || out.State != r.State || out.Metadata["content_withheld"] != true {
				t.Fatalf("unsafe projection: %s %v", body, err)
			}
			if mode == "committed" && out.ResultRef != r.ResultRef {
				t.Fatal("lost result identity")
			}
			out.Scope.Levels[0].Value = "other"
			*out.StartedAt = time.Time{}
			out.Metadata["untrusted"] = "changed"
			if r.Scope.Levels[0].Value != "one" || r.StartedAt.IsZero() || r.Metadata["untrusted"] != "SECRET metadata" || r.Output != "SECRET candidate" {
				t.Fatal("projection mutated stored object")
			}
		})
	}
}

func TestPublicRunViewPreservesOrdinaryAndNil(t *testing.T) {
	legacy := &Run{Output: "ordinary"}
	if PublicView(legacy, false) != legacy || PublicView(nil, true) != nil || ContentProtected(nil, true) {
		t.Fatal("legacy or nil behavior changed")
	}
}
