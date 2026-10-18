package optimization

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func fixtureSpec(t *testing.T) ExperimentSpec {
	t.Helper()
	base := fixtureVersion(t)
	spec := ExperimentSpec{
		SchemaVersion: 1, Scope: base.Scope, AgentID: base.AgentID,
		Baseline:   VersionRef{ID: base.ID, Digest: base.Digest},
		Controller: VersionRef{ID: id.New(PrefixVersion), Digest: strings.Repeat("a", 64)},
		Mode:       ModeSequential, Limits: DefaultLimits(),
		Budget:    Budget{MaxTokens: 100000, TokenOnly: true},
		Objective: DefaultObjective(), Requester: IdentityRef{Subject: "test-user"},
		Evaluation: ContentRef{Ref: "eval-plan", Revision: "1", Digest: strings.Repeat("b", 64)},
	}
	for i := range 30 {
		split := SplitDiscovery
		if i >= 10 {
			split = SplitValidation
		}
		c, err := SealTaskCase(TaskCase{ID: fmt.Sprintf("case-%d", i), Input: fmt.Sprintf("Task %d", i), Split: split,
			EnvironmentSnapshot: ContentRef{Ref: "fixture", Revision: "1", Digest: strings.Repeat("c", 64)}})
		if err != nil {
			t.Fatal(err)
		}
		spec.Tasks = append(spec.Tasks, c)
	}
	return spec
}

func TestSpecPreflightCountsTrials(t *testing.T) {
	spec := fixtureSpec(t)
	plan, err := Preflight(spec)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DiscoveryTrials != 210 || plan.ValidationTrials != 120 || plan.TotalTrials != 330 {
		t.Fatalf("baseline, composed candidates or validation missing: %+v", plan)
	}
	spec.Limits.MaxCandidates = 12
	if _, err = Preflight(spec); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("510 planned trials admitted by 500 cap: %v", err)
	}
	spec.Limits.MaxTrials = 510
	plan, err = Preflight(spec)
	if err != nil || plan.TotalTrials != 510 || plan.ValidationTrials != 120 {
		t.Fatalf("explicit larger host cap not honored: %+v %v", plan, err)
	}
	spec.Limits.MaxCandidates = math.MaxInt
	if _, err = Preflight(spec); err == nil {
		t.Fatal("trial count overflow accepted")
	}
}

func TestSpecRejectsSplitOverlap(t *testing.T) {
	spec := fixtureSpec(t)
	duplicate := spec.Tasks[10]
	duplicate.Input = "  Task 0\r\n"
	duplicate.EnvironmentSnapshot.Revision = "another-environment"
	var err error
	duplicate, err = SealTaskCase(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	spec.Tasks[10] = duplicate
	if err = ValidateSpec(spec); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("same normalized input reused under new ID and environment: %v", err)
	}
}

func TestSpecRefusesInvalidBoundsAndIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*ExperimentSpec){
		"scope":                    func(s *ExperimentSpec) { s.Scope.Levels = nil },
		"requester":                func(s *ExperimentSpec) { s.Requester.Subject = "" },
		"wrong version kind":       func(s *ExperimentSpec) { s.Baseline.ID = id.New(PrefixTrial) },
		"missing digest":           func(s *ExperimentSpec) { s.Controller.Digest = "" },
		"missing token cap":        func(s *ExperimentSpec) { s.Budget.MaxTokens = 0 },
		"implicit free money":      func(s *ExperimentSpec) { s.Budget.TokenOnly = false },
		"misleading money cap":     func(s *ExperimentSpec) { s.Budget.MaxUSDMicros = 1 },
		"missing pricing revision": func(s *ExperimentSpec) { s.Budget.TokenOnly = false; s.Budget.MaxUSDMicros = 1 },
		"zero concurrency":         func(s *ExperimentSpec) { s.Limits.Concurrency = 0 },
		"zero output cap":          func(s *ExperimentSpec) { s.Limits.MaxOutputTokens = 0 },
		"zero rounds":              func(s *ExperimentSpec) { s.Limits.MaxRounds = 0 },
		"zero trial duration":      func(s *ExperimentSpec) { s.Limits.TrialTimeout = 0 },
		"nonfinite objective":      func(s *ExperimentSpec) { s.Objective.MinImprovement = math.NaN() },
		"unknown mode":             func(s *ExperimentSpec) { s.Mode = "unknown" },
		"changed task":             func(s *ExperimentSpec) { s.Tasks[0].Input += " changed" },
		"duplicate case ID":        func(s *ExperimentSpec) { s.Tasks[1].ID = s.Tasks[0].ID },
	} {
		t.Run(name, func(t *testing.T) {
			spec := fixtureSpec(t)
			mutate(&spec)
			if err := ValidateSpec(spec); err == nil {
				t.Fatal("invalid experiment accepted")
			}
		})
	}
}

func TestTaskCaseDigestBindsFrozenContext(t *testing.T) {
	original := fixtureSpec(t).Tasks[0]
	changed := original
	changed.Context = []ContentRef{{Ref: "manual", Revision: "7", Digest: strings.Repeat("d", 64)}}
	sealed, err := SealTaskCase(changed)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Digest == original.Digest || sealed.InputDigest != original.InputDigest {
		t.Fatal("task identity must bind context separately from normalized input identity")
	}
	changed.Context[0].Revision = "8"
	if sealed.Context[0].Revision != "7" {
		t.Fatal("sealed task aliases caller context")
	}
}

func TestDecodeSpecRejectsAmbiguousFields(t *testing.T) {
	for _, raw := range []string{`{"schema_version":1,"schema_version":2}`, `{"budget":{"unlimited":true}}`} {
		if _, err := DecodeSpec([]byte(raw)); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("ambiguous spec accepted: %v", err)
		}
	}
}
