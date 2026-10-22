package planning

import (
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func requirementsPlan(t *testing.T) Plan {
	t.Helper()
	p := fixturePlan(t)
	p.Effects = []execution.EffectRequirement{{Tool: "lookup", Revision: "1", Class: execution.EffectReadOnly, Retry: execution.RetryPolicy{MaxAttempts: 1}}}
	p.Sources = []verification.Source{{Resource: "source", Revision: "1", Digest: evidence.Digest([]byte("source")), RetrievedAt: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}}
	p.Tasks[0].Effects = []string{"lookup"}
	p.Tasks[0].Sources = []string{"source"}
	sealed, err := Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestPlanBindsEffectAndSourceRequirements(t *testing.T) {
	for _, mode := range []string{"valid", "unknown_effect", "reclassified", "unknown_source", "source_revision", "uncovered_source", "missing_resource"} {
		t.Run(mode, func(t *testing.T) {
			p := requirementsPlan(t)
			switch mode {
			case "unknown_effect":
				p.Tasks[0].Effects = []string{"write"}
			case "reclassified":
				p.Effects[0].Class = execution.EffectNonIdempotent
			case "unknown_source":
				p.Tasks[0].Sources = []string{"other"}
			case "source_revision":
				p.Sources[0].Revision = "2"
			case "uncovered_source":
				p.Tasks[0].Sources = nil
			case "missing_resource":
				p.Tasks[0].Resources = nil
			}
			_, err := Seal(p, Limits{})
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}

func TestRevisionCannotReplaceHostEffectOrSourcePins(t *testing.T) {
	for _, mode := range []string{"effect", "source"} {
		p := requirementsPlan(t)
		next, err := Seal(p, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if mode == "effect" {
			next.Effects[0].Revision = "2"
		} else {
			next.Sources[0].Digest = evidence.Digest([]byte("replacement"))
		}
		if _, err = ApplyRevision(p, Revision{Plan: next, Actor: "host", Reason: "proposal"}, 1, Limits{}); err == nil {
			t.Fatalf("replaced host %s pin", mode)
		}
	}
}

func TestReadyRequiresPinnedToolAndSourceContext(t *testing.T) {
	for _, mode := range []string{"valid", "missing_tool", "tool_revision", "extra_tool", "source_digest", "source_time", "missing_source"} {
		t.Run(mode, func(t *testing.T) {
			p := requirementsPlan(t)
			child := p.Tasks[0]
			child.ID = id.New(PrefixTask)
			child.Output.Name = "summary"
			child.DependsOn = []id.ID{p.Tasks[0].ID}
			p.Tasks = append(p.Tasks, child)
			var err error
			p, err = Seal(p, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
			state := acceptedState(t, p, p.Tasks[0], at)
			state.Current.Context.Tools = []verification.ToolRevision{{Name: "lookup", Revision: "1"}}
			state.Current.Context.Sources = append([]verification.Source(nil), p.Sources...)
			switch mode {
			case "missing_tool":
				state.Current.Context.Tools = nil
			case "tool_revision":
				state.Current.Context.Tools[0].Revision = "2"
			case "extra_tool":
				state.Current.Context.Tools = append(state.Current.Context.Tools, verification.ToolRevision{Name: "write", Revision: "1"})
			case "source_digest":
				state.Current.Context.Sources[0].Digest = evidence.Digest([]byte("other"))
			case "source_time":
				state.Current.Context.Sources[0].RetrievedAt = state.Current.Context.Sources[0].RetrievedAt.Add(time.Hour)
			case "missing_source":
				state.Current.Context.Sources = nil
			}
			state.Reports[0].Binding = state.Current
			state.Reports[0], err = verification.SealReport(state.Reports[0])
			if err != nil {
				t.Fatal(err)
			}
			ready, err := Ready(p, []TaskState{state}, at)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "valid" {
				if len(ready) != 1 {
					t.Fatalf("pinned context not ready: %v", ready)
				}
			} else if len(ready) != 0 {
				t.Fatalf("unbound %s accepted: %v", mode, ready)
			}
		})
	}
}

func TestMutationRequiresExplicitWritableResource(t *testing.T) {
	p := requirementsPlan(t)
	p.Effects[0] = execution.EffectRequirement{Tool: "lookup", Revision: "1", Class: execution.EffectNonIdempotent, ReconcileRevision: "probe-v1", Retry: execution.RetryPolicy{MaxAttempts: 1}}
	if _, err := Seal(p, Limits{}); err == nil {
		t.Fatal("mutation allowed against read-only resource")
	}
	p.Resources[0].Access = AccessWrite
	p.Tasks[0].Resources[0].Access = AccessWrite
	if _, err := Seal(p, Limits{}); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionCannotReplaceOriginOrDropRequiredSource(t *testing.T) {
	p := requirementsPlan(t)
	p.Origin = Origin{Kind: "orchestration", ID: id.NewOrchestrationConfigID(), Digest: evidence.Digest([]byte("config"))}
	var err error
	p, err = Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"origin", "source"} {
		next, copyErr := Seal(p, Limits{})
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if mode == "origin" {
			next.Origin.Digest = evidence.Digest([]byte("other"))
		} else {
			next.Sources = nil
			next.Tasks[0].Sources = nil
		}
		if _, err = ApplyRevision(p, Revision{Plan: next, Actor: "host", Reason: "proposal"}, 1, Limits{}); err == nil {
			t.Fatalf("changed host %s pin", mode)
		}
	}
}
