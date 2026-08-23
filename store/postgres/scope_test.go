package postgres

import (
	"reflect"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/run"
)

func ws(vals ...string) cortex.Scope {
	keys := []string{"workspace", "project", "environment"}
	s := cortex.Scope{}
	for i, v := range vals {
		s.Levels = append(s.Levels, cortex.Level{Key: keys[i], Value: v})
	}
	return s
}

func TestScopeColumns(t *testing.T) {
	tests := []struct {
		name           string
		scope          cortex.Scope
		l0, l1, l2     string
		wantExtraCount int
	}{
		{"zero", cortex.Scope{}, "", "", "", 0},
		{"one level", ws("ws_x"), "workspace=ws_x", "", "", 0},
		{"two levels", ws("ws_x", "proj_y"), "workspace=ws_x", "project=proj_y", "", 0},
		{"three levels", ws("ws_x", "proj_y", "prod"), "workspace=ws_x", "project=proj_y", "environment=prod", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l0, l1, l2, extra := scopeColumns(tt.scope)
			if l0 != tt.l0 || l1 != tt.l1 || l2 != tt.l2 {
				t.Errorf("scopeColumns() = %q, %q, %q; want %q, %q, %q", l0, l1, l2, tt.l0, tt.l1, tt.l2)
			}
			if len(extra) != tt.wantExtraCount {
				t.Errorf("extra count = %d, want %d", len(extra), tt.wantExtraCount)
			}
		})
	}
}

func TestScopeColumns_OverflowGoesToExtra(t *testing.T) {
	s := cortex.Scope{Levels: []cortex.Level{
		{Key: "a", Value: "1"},
		{Key: "b", Value: "2"},
		{Key: "c", Value: "3"},
		{Key: "d", Value: "4"},
	}}
	_, _, _, extra := scopeColumns(s)
	if len(extra) != 1 || extra["d"] != "4" {
		t.Errorf("extra = %v, want {d:4}", extra)
	}
}

func TestScopePredicates_PrefixOmitsUnsetLevels(t *testing.T) {
	got := scopePredicates(ws("ws_x"), false)
	want := []scopePredicate{{Column: "scope_l0", Value: "workspace=ws_x"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("prefix predicates = %v, want %v", got, want)
	}
}

func TestScopePredicates_ExactPinsUnsetLevels(t *testing.T) {
	got := scopePredicates(ws("ws_x"), true)
	want := []scopePredicate{
		{Column: "scope_l0", Value: "workspace=ws_x"},
		{Column: "scope_l1", Value: ""},
		{Column: "scope_l2", Value: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("exact predicates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("predicate %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// A zero scope in prefix mode produces no predicates at all, which is an
// unfiltered query. Callers must reject a zero scope before they get here;
// this test exists so that contract is visible and cannot drift silently.
func TestScopePredicates_ZeroScopePrefixMatchesEverything(t *testing.T) {
	got := scopePredicates(cortex.Scope{}, false)
	if len(got) != 0 {
		t.Fatalf("prefix predicates on a zero scope = %v, want none", got)
	}
}

func TestScopePredicates_ZeroScopeExactPinsAllLevels(t *testing.T) {
	got := scopePredicates(cortex.Scope{}, true)
	want := []scopePredicate{
		{Column: "scope_l0", Value: ""},
		{Column: "scope_l1", Value: ""},
		{Column: "scope_l2", Value: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("exact predicates on a zero scope = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("predicate %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestListFilter_RunHasExactNoAppID guards run.ListFilter's shape. It never
// had AppID (the run store was never app-keyed the way agent's List is),
// but Exact needs to exist so ListRuns/CountRuns can ask for rows stored at
// exactly the caller's scope depth instead of anything beneath it.
func TestListFilter_RunHasExactNoAppID(t *testing.T) {
	typ := reflect.TypeOf(run.ListFilter{})
	if _, found := typ.FieldByName("AppID"); found {
		t.Error("run.ListFilter has AppID; scope comes from context now")
	}
	if _, found := typ.FieldByName("Exact"); !found {
		t.Error("run.ListFilter is missing Exact")
	}
}

// TestListFilter_AgentHasExactNoAppID guards agent.ListFilter's shape now
// that the agent surface has converted: AppID is gone (Create/Get/
// GetByName/Update/Delete/List/CountAgents are all scope-guarded, and
// UNIQUE (scope_canon, name) replaced the app_id-keyed index), and Exact
// exists so List/CountAgents can ask for rows stored at exactly the
// caller's scope depth instead of anything beneath it — same shape as
// run.ListFilter above. skill/trait/behavior/persona/orchestration still
// key on AppID; only agent and run convert this phase.
func TestListFilter_AgentHasExactNoAppID(t *testing.T) {
	typ := reflect.TypeOf(agent.ListFilter{})
	if _, found := typ.FieldByName("AppID"); found {
		t.Error("agent.ListFilter has AppID; scope comes from context now")
	}
	if _, found := typ.FieldByName("Exact"); !found {
		t.Error("agent.ListFilter is missing Exact")
	}
}
