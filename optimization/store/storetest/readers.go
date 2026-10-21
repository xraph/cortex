package storetest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func readers(t *testing.T, factory Factory) {
	t.Helper()
	resourceReaders(t, factory)
	t.Run("EvaluationCursorEscapedIdentityRoundTrips", func(t *testing.T) {
		s, ctx, bundle, _, _ := decisionFixtureWithEvaluation(t, factory, func(arm int, value *optimization.Evaluation) {
			value.ID = strings.Repeat("<", 500) + strconv.Itoa(arm) + "-" + strconv.Itoa(value.Repetition)
		})
		values := readAll(t, func(cursor string) (optimization.Page[optimization.Evaluation], error) {
			return s.ListEvaluations(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: 1, Cursor: cursor})
		})
		if len(values) != 12 {
			t.Fatalf("escaped identities lost records: %d", len(values))
		}
	})
	t.Run("CursorLongScopeRoundTrips", func(t *testing.T) {
		s := factory(t, time.Now)
		bundles := []optimization.ExperimentBundle{Fixture(t, strings.Repeat("p", 4096)), Fixture(t, strings.Repeat("p", 4096))}
		ctx := scoped(bundles[0].Experiment.Spec.Scope)
		for _, bundle := range bundles {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
				t.Fatal(err)
			}
		}
		values := readAll(t, func(cursor string) (optimization.Page[optimization.Experiment], error) {
			return s.ListExperiments(ctx, optimization.ListQuery{Limit: 1, Cursor: cursor})
		})
		assertReaderIDs(t, values, []string{bundles[0].Experiment.ID.String(), bundles[1].Experiment.ID.String()}, func(value optimization.Experiment) string { return value.ID.String() })
	})
	t.Run("ResourceFilterRejectsUnknownExperimentState", func(t *testing.T) {
		s := factory(t, time.Now)
		ctx := scoped(Fixture(t, "unknown-state").Experiment.Spec.Scope)
		if _, err := s.ListExperiments(ctx, optimization.ListQuery{State: "typo"}); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("unsupported state became empty results: %v", err)
		}
	})
	t.Run("EvaluationIdentityFitsResourceCursor", func(t *testing.T) {
		scenario, value := evaluatedTrial(t, factory)
		trial, err := scenario.store.GetTrial(scenario.ctx, scenario.trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = value.ValidateCaptured(scenario.bundle.Experiment, trial); err != nil {
			t.Fatalf("invalid identity fixture: %v", err)
		}
		for _, identity := range []string{strings.Repeat("j", 513), "job\n1", string([]byte{0xff})} {
			changed := value
			changed.ID = identity
			if validationErr := changed.ValidateCaptured(scenario.bundle.Experiment, trial); validationErr == nil {
				t.Fatalf("invalid job identity accepted: length=%d", len(identity))
			}
		}
	})
	t.Run("CursorOrdersCreationTimeBeforeID", func(t *testing.T) {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		s := factory(t, func() time.Time { return now })
		bundles := []optimization.ExperimentBundle{Fixture(t, "ordered"), Fixture(t, "ordered"), Fixture(t, "ordered")}
		slices.SortFunc(bundles, func(a, b optimization.ExperimentBundle) int {
			return -strings.Compare(a.Experiment.ID.String(), b.Experiment.ID.String())
		})
		ctx := scoped(bundles[0].Experiment.Spec.Scope)
		for _, bundle := range bundles {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Second)
		}
		cursor := ""
		for _, bundle := range bundles {
			page, err := s.ListExperiments(ctx, optimization.ListQuery{Limit: 1, Cursor: cursor})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != bundle.Experiment.ID {
				t.Fatalf("creation order lost: %+v %v", page, err)
			}
			cursor = page.NextCursor
		}
		if cursor != "" {
			t.Fatal("terminal page has a cursor")
		}
	})
}

func resourceReaders(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ResourceReadersPreserveIdentitiesAndFilters", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
		})
		decision.Outcome, decision.EvidenceClass = optimization.DecisionRecommend, optimization.EvidenceValidated
		decision.Comparison.Complete, decision.Comparison.MissingReasons = true, nil
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		_, _, other, _, _ := decisionFixture(t, func(*testing.T, func() time.Time) store.Store { return s })
		activation := activationRequest(bundle, decision)
		first := activate(ctx, t, s, activation)
		activation.ID, activation.ExpectedGeneration, activation.RollbackTo = id.New(optimization.PrefixActivation), 1, first.ID
		second := activate(ctx, t, s, activation)
		lessonIDs := make([]string, 0, 3)
		for i := range 3 {
			request := publicationRequest(bundle, lease, decision)
			if i == 1 {
				request.Lesson.Domain = "other"
			}
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(request) }); err != nil {
				t.Fatal(err)
			}
			lessonIDs = append(lessonIDs, request.Lesson.ID.String())
			if i == 2 {
				if err := s.Transact(ctx, func(tx store.Tx) error {
					return tx.RevokeLesson(optimization.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Reason: "test revocation", Actor: bundle.Experiment.Spec.Requester})
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		usage := reservation(bundle.Experiment.ID, lease, "reader-budget", 1)
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.Reserve(usage); return err }); err != nil {
			t.Fatal(err)
		}
		trials := readAll(t, func(cursor string) (optimization.Page[optimization.Trial], error) {
			return s.ListTrials(ctx, bundle.Experiment.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 2, Cursor: cursor}, Split: optimization.SplitValidation})
		})
		wantTrials := make([]string, 0, len(decision.EvaluationIDs))
		for _, evaluationID := range decision.EvaluationIDs {
			value, err := s.GetEvaluation(ctx, evaluationID)
			if err != nil {
				t.Fatal(err)
			}
			wantTrials = append(wantTrials, value.TrialID.String())
		}
		assertReaderIDs(t, trials, wantTrials, func(value optimization.Trial) string { return value.ID.String() })
		for _, trial := range trials {
			if trial.ExperimentID != bundle.Experiment.ID || trial.Split != optimization.SplitValidation {
				t.Fatal("trial filter ignored")
			}
		}
		trials[0].Attempts[0].Error = "changed by reader"
		storedTrial, err := s.GetTrial(ctx, trials[0].ID)
		if err != nil || storedTrial.Attempts[0].Error == "changed by reader" {
			t.Fatal("trial page aliases storage")
		}
		versions := readAll(t, func(cursor string) (optimization.Page[optimization.HarnessVersion], error) {
			return s.ListVersions(ctx, optimization.VersionQuery{PageQuery: optimization.PageQuery{Limit: 1, Cursor: cursor}, AgentID: bundle.Experiment.Spec.AgentID})
		})
		assertReaderIDs(t, versions, []string{bundle.Experiment.Spec.Baseline.ID.String(), bundle.Experiment.Spec.Controller.ID.String(), decision.Candidate.ID.String()}, func(value optimization.HarnessVersion) string { return value.ID.String() })
		candidates, err := s.ListCandidates(ctx, bundle.Experiment.ID, optimization.PageQuery{})
		if err != nil || len(candidates.Items) != 1 || candidates.Items[0].Version != decision.Candidate {
			t.Fatalf("candidate list lost finalist: %v", err)
		}
		evaluations := readAll(t, func(cursor string) (optimization.Page[optimization.Evaluation], error) {
			return s.ListEvaluations(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: 2, Cursor: cursor})
		})
		if len(evaluations) != 12 {
			t.Fatalf("evaluation count: %d", len(evaluations))
		}
		for _, value := range evaluations {
			if value.ExperimentID != bundle.Experiment.ID {
				t.Fatal("evaluation parent ignored")
			}
		}
		decisions, err := s.ListDecisions(ctx, bundle.Experiment.ID, optimization.PageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		assertReaderIDs(t, decisions.Items, []string{decision.ID.String()}, func(value optimization.Decision) string { return value.ID.String() })
		allocations, err := s.ListValidationAllocations(ctx, bundle.Experiment.ID, optimization.PageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		assertReaderIDs(t, allocations.Items, []string{decision.AllocationID.String()}, func(value optimization.ValidationAllocation) string { return value.ID.String() })
		reservations, err := s.ListReservations(ctx, bundle.Experiment.ID, optimization.PageQuery{})
		if err != nil || len(reservations.Items) != 1 || reservations.Items[0].Request.CallKey != usage.CallKey {
			t.Fatalf("reservation list lost debit: %v", err)
		}
		lessons := readAll(t, func(cursor string) (optimization.Page[optimization.Lesson], error) {
			return s.ListLessons(ctx, optimization.LessonQuery{PageQuery: optimization.PageQuery{Limit: 1, Cursor: cursor}})
		})
		assertReaderIDs(t, lessons, lessonIDs, func(value optimization.Lesson) string { return value.ID.String() })
		eligible, err := s.ListLessons(ctx, optimization.LessonQuery{Domain: "reasoning", EligibleOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		assertReaderIDs(t, eligible.Items, lessonIDs[:1], func(value optimization.Lesson) string { return value.ID.String() })
		publications := readAll(t, func(cursor string) (optimization.Page[optimization.Publication], error) {
			return s.ListPublications(ctx, optimization.PublicationQuery{PageQuery: optimization.PageQuery{Limit: 1, Cursor: cursor}, ExperimentID: bundle.Experiment.ID})
		})
		wantPublications := []string{lessonIDs[0] + "/1", lessonIDs[1] + "/1", lessonIDs[2] + "/1", lessonIDs[2] + "/2"}
		assertReaderIDs(t, publications, wantPublications, func(value optimization.Publication) string {
			return value.LessonID.String() + "/" + strconv.FormatInt(value.LocalRevision, 10)
		})
		pending, err := s.ListPublications(ctx, optimization.PublicationQuery{PendingOnly: true})
		if err != nil || len(pending.Items) != 3 {
			t.Fatalf("superseded publication remained pending: %v", err)
		}
		history := readAll(t, func(cursor string) (optimization.Page[optimization.Activation], error) {
			return s.ListActivationHistory(ctx, bundle.Experiment.Spec.AgentID, "", optimization.PageQuery{Limit: 1, Cursor: cursor})
		})
		assertReaderIDs(t, history, []string{first.ID.String(), second.ID.String()}, func(value optimization.Activation) string { return value.ID.String() })
		page, err := s.ListTrials(ctx, bundle.Experiment.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 1}, Split: optimization.SplitValidation})
		if err != nil || page.NextCursor == "" {
			t.Fatal("missing trial cursor")
		}
		query := optimization.PageQuery{Cursor: page.NextCursor, Limit: 1}
		if _, err = s.ListTrials(ctx, other.Experiment.ID, optimization.TrialQuery{PageQuery: query, Split: optimization.SplitValidation}); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal("cursor crossed parent")
		}
		if _, err = s.ListTrials(ctx, bundle.Experiment.ID, optimization.TrialQuery{PageQuery: query, Split: optimization.SplitDiscovery}); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal("cursor changed split")
		}
		if _, err = s.ListCandidates(ctx, bundle.Experiment.ID, query); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal("cursor crossed collection")
		}
		foreign := scoped(Fixture(t, "foreign-readers").Experiment.Spec.Scope)
		if _, err = s.ListTrials(foreign, bundle.Experiment.ID, optimization.TrialQuery{PageQuery: query, Split: optimization.SplitValidation}); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal("cursor crossed scope")
		}
		if _, err = s.ListTrials(foreign, bundle.Experiment.ID, optimization.TrialQuery{}); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("foreign parent read")
		}
		if _, err = s.ListVersions(context.Background(), optimization.VersionQuery{}); err == nil {
			t.Fatal("unscoped list succeeded")
		}
		if _, err = s.ListLessons(ctx, optimization.LessonQuery{PageQuery: optimization.PageQuery{Limit: 501}}); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal("unbounded list admitted")
		}
	})
}

func readAll[T any](t *testing.T, fetch func(string) (optimization.Page[T], error)) []T {
	t.Helper()
	values := make([]T, 0)
	cursor := ""
	seen := make(map[string]bool)
	for range 100 {
		page, err := fetch(cursor)
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, page.Items...)
		if page.NextCursor == "" {
			return values
		}
		if seen[page.NextCursor] || len(page.Items) == 0 {
			t.Fatal("pagination repeated or failed to advance")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func assertReaderIDs[T any](t *testing.T, values []T, expected []string, identity func(T) string) {
	t.Helper()
	actual := make([]string, 0, len(values))
	for _, value := range values {
		actual = append(actual, identity(value))
	}
	want := slices.Clone(expected)
	slices.Sort(actual)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Fatalf("identities differ: got %v want %v", actual, want)
	}
}
