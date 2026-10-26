package sentinel

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

type calibrationStorage struct {
	items     map[id.ID]evidence.Artifact
	deleted   map[id.ID]bool
	reads     int
	afterLoad func(int)
}

func (s *calibrationStorage) SaveEvidence(_ context.Context, a evidence.Artifact) error {
	s.items[a.Ref.ID] = a
	return nil
}
func (s *calibrationStorage) LoadEvidence(_ context.Context, r evidence.Ref) (evidence.Artifact, error) {
	s.reads++
	if s.afterLoad != nil {
		s.afterLoad(s.reads)
	}
	if s.deleted[r.ID] {
		return evidence.Artifact{}, evidence.ErrDeleted
	}
	a, ok := s.items[r.ID]
	if !ok {
		return a, evidence.ErrNotFound
	}
	return a, nil
}
func (s *calibrationStorage) TombstoneEvidence(_ context.Context, r evidence.Ref, _ string) error {
	s.deleted[r.ID] = true
	return nil
}

type calibrationGrant struct {
	calls  int
	denyAt int
}

func (g *calibrationGrant) AuthorizeEvidence(context.Context, evidence.Action, evidence.Ref) error {
	return nil
}
func (g *calibrationGrant) AuthorizeCalibration(_ context.Context, _ engine.OperationalCalibrationRequest, _ CalibrationRegistration) error {
	g.calls++
	if g.calls == g.denyAt {
		return evidence.ErrDenied
	}
	return nil
}

func calibrationFixture(t *testing.T) (context.Context, *EvidenceCalibrationProvider, *calibrationStorage, *calibrationGrant, engine.OperationalCalibrationRequest, CalibrationRegistration, CalibrationCohort) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "a"}, {Key: "project", Value: "b"}, {Key: "env", Value: "c"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	binding := cognitive.CalibrationBinding{Scope: scope, TaskClass: "retrieval", ProviderRevision: "provider-v1", ModelRevision: "model-v1", PromptRevision: evidence.Digest([]byte("prompt")), ControllerRevision: "controller-v1", PolicyRevision: evidence.Digest([]byte("policy")), EnvironmentRevision: "test-v1", AcceptanceDigest: evidence.Digest([]byte("acceptance")), Tools: []cognitive.CalibrationTool{}}
	cohort := CalibrationCohort{SchemaVersion: 1, Revision: "cohort-v1", EvaluatorRevision: "evaluator-v1", Binding: binding, DiscoveryItems: []string{evidence.Digest([]byte("discovery"))}, HoldoutItems: []string{evidence.Digest([]byte("one")), evidence.Digest([]byte("two"))}, WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	cohort.Trials = []CalibrationTrial{{ItemDigest: cohort.HoldoutItems[0], Outcome: CalibrationSuccess}, {ItemDigest: cohort.HoldoutItems[1], Outcome: CalibrationMissing}}
	storage := &calibrationStorage{items: map[id.ID]evidence.Artifact{}, deleted: map[id.ID]bool{}}
	grant := &calibrationGrant{}
	repo, err := evidence.NewRepository(storage, grant, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(value any) evidence.Ref {
		body, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		artifact, e := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), MediaType: "application/json", Scope: scope, Version: "v1", Producer: "trusted-evaluator", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(2 * time.Hour)}, Data: body})
		if e != nil {
			t.Fatal(e)
		}
		ref, e := repo.Put(ctx, artifact)
		if e != nil {
			t.Fatal(e)
		}
		return ref
	}
	source := seal(cohort)
	measurement, err := MeasureCalibration(cohort, source)
	if err != nil {
		t.Fatal(err)
	}
	profile := seal(measurement)
	registration := CalibrationRegistration{AgentID: id.NewAgentID(), Model: "test-model", Binding: binding, Profile: profile, Producer: "trusted-evaluator"}
	provider, err := NewEvidenceCalibrationProvider(repo, grant, []CalibrationRegistration{registration})
	if err != nil {
		t.Fatal(err)
	}
	request := engine.OperationalCalibrationRequest{RunID: id.NewAgentRunID(), AgentID: registration.AgentID, Scope: scope, PreparedDigest: evidence.Digest([]byte("prepared")), Model: registration.Model, ModelRevision: binding.ModelRevision, PromptDigest: binding.PromptRevision, PolicyDigest: binding.PolicyRevision, AcceptanceDigest: binding.AcceptanceDigest, Tools: binding.Tools}
	return ctx, provider, storage, grant, request, registration, cohort
}

func TestEvidenceCalibrationProvider(t *testing.T) {
	for _, mode := range []string{"measured", "denied", "revoked-after-hydration", "deleted-support", "deleted-profile-after-hydration", "tampered-profile", "noncanonical", "other-deep-scope", "other-model", "other-tools", "duplicate-route", "detached-registration"} {
		t.Run(mode, func(t *testing.T) {
			ctx, p, s, g, r, registration, _ := calibrationFixture(t)
			switch mode {
			case "denied":
				g.denyAt = 1
			case "revoked-after-hydration":
				g.denyAt = 2
			case "deleted-support":
				s.afterLoad = func(n int) {
					if n == 2 {
						for key := range s.items {
							if key != registration.Profile.ID {
								s.deleted[key] = true
							}
						}
					}
				}
			case "deleted-profile-after-hydration":
				s.afterLoad = func(n int) {
					if n == 2 {
						s.deleted[registration.Profile.ID] = true
					}
				}
			case "tampered-profile", "noncanonical":
				a := s.items[registration.Profile.ID]
				var m CalibrationMeasurement
				if err := json.Unmarshal(a.Data, &m); err != nil {
					t.Fatal(err)
				}
				m.Profile.Estimate = .9
				a.Data, _ = json.Marshal(m)
				if mode == "noncanonical" {
					a.Data = append(a.Data, ' ')
				}
				a, _ = evidence.Seal(a)
				s.items[a.Ref.ID] = a
				p.registrations[0].Profile = a.Ref
			case "other-deep-scope":
				r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
				r.Scope.Levels[2].Value = "other"
			case "other-model":
				r.Model = "other"
			case "other-tools":
				r.Tools = []cognitive.CalibrationTool{{Name: "write", Revision: "v1"}}
			case "duplicate-route":
				if _, err := NewEvidenceCalibrationProvider(p.repository, g, []CalibrationRegistration{registration, registration}); err == nil {
					t.Fatal("duplicate route admitted")
				}
				return
			case "detached-registration":
				r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
				ctx = cortex.WithScope(ctx, r.Scope)
				registration.Binding.Scope.Levels[2].Value = "mutated"
			}
			got, err := p.ObserveCalibration(ctx, r)
			switch mode {
			case "measured":
				if err != nil || got.Observation == nil || got.Observation.Origin != cognitive.CalibrationMeasured || got.Observation.Profile.Trials != 2 || got.Observation.Profile.Missing != 1 || got.Observation.Profile.Estimate != .5 || g.calls != 2 {
					t.Fatalf("observation=%+v error=%v grants=%d", got, err, g.calls)
				}
			case "other-model", "other-tools":
				if err != nil || got.Observation != nil || s.reads != 0 {
					t.Fatalf("incompatible cohort exposed: %+v %v", got, err)
				}
			case "detached-registration":
				if err != nil || got.Observation == nil {
					t.Fatalf("caller mutation changed frozen registration: %v", err)
				}
			default:
				if err == nil || got.Observation != nil {
					t.Fatalf("unsafe observation: %+v %v", got, err)
				}
			}
		})
	}
}

func TestCalibrationCohortRejectsIncompleteOrLeakedRoster(t *testing.T) {
	for _, mode := range []string{"missing-trial", "duplicate-holdout", "discovery-leak", "wrong-order", "invalid-outcome", "changed-bytes", "all-success", "no-success"} {
		t.Run(mode, func(t *testing.T) {
			_, _, storage, _, _, registration, cohort := calibrationFixture(t)
			var ref evidence.Ref
			for _, a := range storage.items {
				if a.Ref.ID != registration.Profile.ID {
					ref = a.Ref
				}
			}
			switch mode {
			case "missing-trial":
				cohort.Trials = cohort.Trials[:1]
			case "duplicate-holdout":
				cohort.HoldoutItems[1] = cohort.HoldoutItems[0]
			case "discovery-leak":
				cohort.DiscoveryItems[0] = cohort.HoldoutItems[0]
			case "wrong-order":
				cohort.Trials[0], cohort.Trials[1] = cohort.Trials[1], cohort.Trials[0]
			case "invalid-outcome":
				cohort.Trials[0].Outcome = "model-says-success"
			case "changed-bytes":
				cohort.Revision = "changed"
			case "all-success", "no-success":
				for i := range cohort.Trials {
					cohort.Trials[i].Outcome = CalibrationFailure
					if mode == "all-success" {
						cohort.Trials[i].Outcome = CalibrationSuccess
					}
				}
			}
			if mode != "changed-bytes" {
				body, _ := json.Marshal(cohort)
				a, _ := evidence.Seal(evidence.Artifact{Ref: ref, Data: body})
				ref = a.Ref
			}
			got, err := MeasureCalibration(cohort, ref)
			if mode == "all-success" || mode == "no-success" {
				if err != nil || got.Profile.Validate() != nil {
					t.Fatalf("boundary profile rejected: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("invalid roster admitted")
			}
		})
	}
}
