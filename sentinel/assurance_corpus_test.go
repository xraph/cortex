package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

type assuranceGrant struct {
	calls  int
	denyAt int
	mutate bool
}

func (g *assuranceGrant) AuthorizeBenchmark(context.Context, BenchmarkRoster, string) error {
	return nil
}
func (g *assuranceGrant) AuthorizeAssuranceObservation(context.Context, AssuranceManifest, AssuranceResultRef, AssuranceObservation) error {
	return nil
}
func (g *assuranceGrant) AuthorizeEvidence(context.Context, evidence.Action, evidence.Ref) error {
	return nil
}
func (g *assuranceGrant) AuthorizeAssurance(ctx context.Context, m AssuranceManifest, _ string, refs []AssuranceResultRef) error {
	g.calls++
	if g.calls == g.denyAt {
		return evidence.ErrDenied
	}
	if g.mutate {
		m.Scope.Levels[0].Value = "mutated"
		m.Slots[0].PreparedDigest = "changed"
		if len(refs) > 0 {
			refs[0].Artifact.Scope.Levels[0].Value = "mutated"
		}
		s := cortex.ScopeFromContext(ctx)
		s.Levels[0].Value = "mutated"
	}
	return nil
}
func assuranceFixture(t *testing.T) (context.Context, *AssuranceCorpusRunner, *calibrationStorage, *assuranceGrant, []AssuranceResultRef) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "assurance-fixture"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	manifest := AssuranceManifest{SchemaVersion: 1, CorpusRevision: AssuranceCorpusRevision, Scope: scope, Mode: "deterministic_fixture", ModelRevision: "fixture-provider-v1", EnvironmentRevision: "fixture-runtime-v1", Producer: "independent-fixture-assessor", FrozenAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}
	for _, arm := range FixedAssuranceArms() {
		for _, c := range FixedAssuranceCases() {
			manifest.Slots = append(manifest.Slots, AssuranceSlot{CaseID: c.ID, ArmID: arm.ID, PreparedDigest: evidence.Digest([]byte("prepared-" + arm.ID))})
		}
	}
	storage := &calibrationStorage{items: map[id.ID]evidence.Artifact{}, deleted: map[id.ID]bool{}}
	grant := &assuranceGrant{}
	repo, err := evidence.NewRepository(storage, grant, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewAssuranceCorpusRunner(repo, nil, grant, manifest)
	if err != nil {
		t.Fatal(err)
	}
	refs := []AssuranceResultRef{}
	for i, slot := range manifest.Slots {
		if i == 0 {
			continue
		}
		observation := AssuranceObservation{SchemaVersion: 1, ManifestDigest: runner.Digest(), CaseID: slot.CaseID, ArmID: slot.ArmID, PreparedDigest: slot.PreparedDigest, SourceDigest: evidence.Digest([]byte(slot.CaseID + slot.ArmID)), ObservedAt: now, Telemetry: AssuranceTelemetry{Outcome: CalibrationSuccess, ClaimedSuccess: assurancePtr(true), Verified: assurancePtr(slot.ArmID != "unchecked"), Abstained: assurancePtr(false), Recovered: assurancePtr(true), UnauthorizedOperations: assurancePtr(0), SecretLeaks: assurancePtr(0), ArtifactSubstitutions: assurancePtr(0), DuplicateEffects: assurancePtr(0), UnsafePublications: assurancePtr(0), LatencyMillis: assurancePtr(int64(i + 1)), CostMicros: assurancePtr(int64(100)), Currency: "USD"}}
		body, _ := json.Marshal(observation)
		a, sealErr := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, MediaType: "application/json", Version: "v1", Producer: manifest.Producer, Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(2 * time.Hour)}, Data: body})
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		ref, putErr := repo.Put(ctx, a)
		if putErr != nil {
			t.Fatal(putErr)
		}
		refs = append(refs, AssuranceResultRef{CaseID: slot.CaseID, ArmID: slot.ArmID, Artifact: ref})
	}
	return ctx, runner, storage, grant, refs
}

func TestAssuranceCorpusCompleteRosterAndReproducibleReport(t *testing.T) {
	ctx, r, _, grant, refs := assuranceFixture(t)
	grant.mutate = true
	got, err := r.Report(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Arms) != 5 || len(got.Evidence) != 59 || len(got.MissingComparisons) != 4 || got.Mode != "deterministic_fixture" {
		t.Fatalf("lost roster: %+v", got)
	}
	if got.Arms[0].Metrics.Planned != 12 || got.Arms[0].Metrics.Calibration.Missing != 1 || got.Arms[0].Metrics.TotalCostMicros != nil {
		t.Fatalf("missing became free success: %+v", got.Arms[0])
	}
	if got.Arms[1].Metrics.Calibration.Successes != 12 || got.Arms[1].Metrics.VerifiedTaskSuccess.Numerator != 12 || *got.Arms[1].Metrics.TotalCostMicros != 1200 || got.Arms[1].Metrics.RequiredSecurityTrials != 5 {
		t.Fatalf("accounting: %+v", got.Arms[1])
	}
	again, err := r.Report(ctx, refs)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("not deterministic: %v", err)
	}
	if cortex.ScopeFromContext(ctx).Levels[0].Value != "assurance-fixture" {
		t.Fatal("callback changed caller scope")
	}
}

func TestAssuranceCorpusRejectsSubstitutionRevocationAndPrunedManifest(t *testing.T) {
	for _, mode := range []string{"duplicate", "wrong-slot", "changed-bytes", "revoked-after-read", "deleted", "wrong-scope", "future", "pruned", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, s, g, refs := assuranceFixture(t)
			switch mode {
			case "duplicate":
				refs = append(refs, refs[0])
			case "wrong-slot":
				refs[0].CaseID = "coding"
			case "changed-bytes":
				a := s.items[refs[0].Artifact.ID]
				a.Data = []byte("{}")
				s.items[a.Ref.ID] = a
			case "revoked-after-read":
				s.afterLoad = func(int) { g.denyAt = g.calls + 1 }
			case "deleted":
				s.deleted[refs[0].Artifact.ID] = true
			case "wrong-scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "foreign"}}})
			case "future":
				refs[0].Artifact.CreatedAt = time.Now().Add(time.Hour)
			case "pruned":
				m := r.manifest
				m.Slots = m.Slots[:59]
				if _, err := NewAssuranceCorpusRunner(r.repository, nil, g, m); err == nil {
					t.Fatal("pruned denominator accepted")
				}
				return
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if result, err := r.Report(ctx, refs); err == nil || result != nil {
				t.Fatalf("exposed invalid report: %+v %v", result, err)
			}
		})
	}
}

func TestAssuranceCorpusUnavailableEvidenceIsNotMissing(t *testing.T) {
	ctx, r, s, _, refs := assuranceFixture(t)
	delete(s.items, refs[0].Artifact.ID)
	if report, err := r.Report(ctx, refs); report != nil || !errors.Is(err, evidence.ErrNotFound) {
		t.Fatalf("outage disguised as empty: %+v %v", report, err)
	}
}

func TestAssuranceCorpusUsesRetainedSentinelComparison(t *testing.T) {
	for _, mode := range []string{"complete", "pending", "unsafe"} {
		t.Run(mode, func(t *testing.T) {
			_, r, _, g, _ := assuranceFixture(t)
			ctx, b, _ := benchmarkFixture(t, mode)
			m := r.cloneManifest()
			m.Scope = b.roster.Scope
			m.Comparisons = []AssuranceComparison{{ArmID: "reflection", Roster: b.roster}}
			runner, err := NewAssuranceCorpusRunner(r.repository, b.store, g, m)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runner.Report(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Comparisons) != 1 || len(got.MissingComparisons) != 3 || got.Arms[0].Metrics.Calibration.Missing != 12 {
				t.Fatalf("lost completeness: %+v", got)
			}
			compared := got.Comparisons[0].Comparison
			if mode == "complete" && (!compared.Conclusive || compared.MeanDelta == nil || math.Abs(*compared.MeanDelta-.4) > 1e-10) {
				t.Fatalf("not using recorded comparison: %+v", compared)
			}
			if mode == "pending" && compared.Conclusive {
				t.Fatal("pending scores became conclusive")
			}
			if mode == "unsafe" && compared.RequiredCriteriaPassed {
				t.Fatal("required failures dropped")
			}
		})
	}
}

func TestAssuranceFixtureArtifact(t *testing.T) {
	ctx, r, _, _, refs := assuranceFixture(t)
	report, err := r.Report(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if report.Mode != "deterministic_fixture" || len(report.Arms) != 5 {
		t.Fatal("fixture report identity lost")
	}
	directory := os.Getenv("CORTEX_ASSURANCE_ARTIFACT_DIR")
	if directory == "" {
		return
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("artifact directory must be absolute")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	corpus := struct {
		Revision string          `json:"revision"`
		Cases    []AssuranceCase `json:"cases"`
		Arms     []AssuranceArm  `json:"arms"`
	}{AssuranceCorpusRevision, FixedAssuranceCases(), FixedAssuranceArms()}
	summary := struct {
		Origin             string               `json:"origin"`
		Mode               string               `json:"mode"`
		CorpusRevision     string               `json:"corpus_revision"`
		Arms               []AssuranceArmReport `json:"arms"`
		MissingComparisons []string             `json:"missing_comparisons"`
	}{"TestAssuranceFixtureArtifact: synthetic authenticated telemetry, no model calls", report.Mode, AssuranceCorpusRevision, report.Arms, report.MissingComparisons}
	for name, value := range map[string]any{"assurance-corpus-v1.json": corpus, "assurance-fixture-report.json": summary} {
		body, encodeErr := json.MarshalIndent(value, "", "  ")
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if writeErr := os.WriteFile(filepath.Join(directory, name), append(body, '\n'), 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
}
