package optimization

import (
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func TestScopeKeyRejectsAmbiguousAndUnindexedScopes(t *testing.T) {
	good := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}, {Key: "project", Value: "two"}, {Key: "environment", Value: "test"}}}
	key, err := ScopeKey(good)
	if err != nil || key != "workspace=one/project=two/environment=test" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	invalid := []cortex.Scope{
		{}, {Levels: []cortex.Level{{Key: "workspace", Value: ""}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "one/two"}}},
		{Levels: []cortex.Level{{Key: "workspace=other", Value: "one"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "one"}, {Key: "workspace", Value: "two"}}},
		{Levels: []cortex.Level{{Key: "w", Value: "1"}, {Key: "p", Value: "2"}, {Key: "e", Value: "3"}, {Key: "trial", Value: "4"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: " one"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "one\x00two"}}},
	}
	for _, scope := range invalid {
		if _, err = ScopeKey(scope); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("accepted malformed scope %+v: %v", scope, err)
		}
	}
}

func TestOptimizationIDsKeepTheirEntityKind(t *testing.T) {
	for _, prefix := range []id.Prefix{PrefixHarness, PrefixVersion, PrefixExperiment, PrefixCandidate, PrefixTrial, PrefixDecision, PrefixLesson, PrefixEvent, PrefixActivation, PrefixAttempt, PrefixValidation} {
		original := id.New(prefix)
		parsed, err := ParseID(original.String(), prefix)
		if err != nil || parsed != original {
			t.Fatalf("identity failed for %s: %v", prefix, err)
		}
		if _, err = ParseID(id.NewAgentID().String(), prefix); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("accepted agent as %s: %v", prefix, err)
		}
	}
	if _, err := ParseID("", PrefixTrial); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("accepted empty ID: %v", err)
	}
}
