package postgres

import (
	"testing"

	"github.com/xraph/cortex"
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
		{Key: "a", Value: "1"}, {Key: "b", Value: "2"},
		{Key: "c", Value: "3"}, {Key: "d", Value: "4"},
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
