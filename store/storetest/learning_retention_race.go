package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

type lessonRetentionRace struct {
	source            id.ID
	lesson            id.ID
	stale             []assurance.Record
	correctionLocked  chan struct{}
	releaseCorrection chan struct{}
	deletionEntered   chan struct{}
	releaseDeletion   chan struct{}
	signal            sync.Once
}

type lessonRaceDriver struct {
	assurance.Driver
	runs assurance.RunDriver
	race *lessonRetentionRace
}

func (d lessonRaceDriver) TransactRun(ctx context.Context, identifier id.AgentRunID, next func(context.Context, assurance.RunTransaction) error) error {
	return d.runs.TransactRun(ctx, identifier, func(tc context.Context, tx assurance.RunTransaction) error {
		return next(tc, &lessonRaceTransaction{RunTransaction: tx, race: d.race, correcting: identifier == d.race.source})
	})
}

type lessonRaceTransaction struct {
	assurance.RunTransaction
	race               *lessonRetentionRace
	correcting, fenced bool
}

func (tx *lessonRaceTransaction) FenceLessonRevision(ctx context.Context, key, digest string) error {
	driver, ok := tx.RunTransaction.(assurance.LessonRetentionDriver)
	if !ok {
		return learning.ErrUnavailable
	}
	if !tx.correcting {
		tx.race.signal.Do(func() { close(tx.race.deletionEntered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tx.race.releaseDeletion:
		}
	}
	if err := driver.FenceLessonRevision(ctx, key, digest); err != nil {
		return err
	}
	tx.fenced = true
	if tx.correcting {
		close(tx.race.correctionLocked)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tx.race.releaseCorrection:
		}
	}
	return nil
}
func (tx *lessonRaceTransaction) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	if !tx.correcting && !tx.fenced && query.Kind == "lesson" && query.RunID == tx.race.lesson.String() {
		// Model the valid stale revision observation from a statement started before
		// correction commits. All writes still use the real backend transaction.
		tx.race.signal.Do(func() { close(tx.race.deletionEntered) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tx.race.releaseDeletion:
		}
		return append([]assurance.Record(nil), tx.race.stale...), nil
	}
	return tx.RunTransaction.Select(ctx, query)
}
func (tx *lessonRaceTransaction) EraseLessonPayloads(ctx context.Context, identifier id.ID) error {
	driver, ok := tx.RunTransaction.(assurance.LessonRetentionDriver)
	if !ok {
		return learning.ErrUnavailable
	}
	return driver.EraseLessonPayloads(ctx, identifier)
}

// QualifiedLearningRetentionRace checks a cross-run correction against a stale
// deletion observation, then inspects every historical payload in real storage.
func QualifiedLearningRetentionRace(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	repository := resultCapability[learning.Repository](t, s)
	ctx, original := AcceptedLessonFixture(t, s)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	accepted, err := repository.AcceptLesson(ctx, original)
	require.NoError(t, err)
	_, err = repository.ClaimLessonPublication(ctx, original.ID, 1, "worker")
	require.NoError(t, err)
	_, replacement := AcceptedLessonFixture(t, s)
	replacement.Corrects = &learning.Ref{ID: original.ID, Revision: 1, Digest: accepted.PayloadDigest}
	stale, err := driver.Select(ctx, assurance.Query{Kind: "lesson", RunID: original.ID.String(), Reverse: true, Limit: 1})
	require.NoError(t, err)
	require.Len(t, stale, 1)
	race := &lessonRetentionRace{source: replacement.Result.RunID, lesson: original.ID, stale: stale, correctionLocked: make(chan struct{}), releaseCorrection: make(chan struct{}), deletionEntered: make(chan struct{}), releaseDeletion: make(chan struct{})}
	runs, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	guarded := assurance.New(lessonRaceDriver{Driver: driver, runs: runs, race: race})
	correction := make(chan error, 1)
	go func() { _, writeErr := guarded.AcceptLesson(ctx, replacement); correction <- writeErr }()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-race.correctionLocked:
	}
	deletion := learning.DeletionRequest{ID: original.ID, ExpectedRevision: 1, Actor: "retention", Reason: "expired"}
	deleted := make(chan error, 1)
	go func() { _, deleteErr := guarded.DeleteLesson(ctx, deletion); deleted <- deleteErr }()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-race.deletionEntered:
	}
	close(race.releaseCorrection)
	require.NoError(t, <-correction)
	close(race.releaseDeletion)
	require.ErrorIs(t, <-deleted, learning.ErrConflict)
	current, err := repository.GetLesson(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), current.Revision)
	require.Equal(t, replacement.ID, current.SupersededBy.ID)
	deletion.ExpectedRevision = 2
	retention := resultCapability[learning.RetentionStore](t, s)
	_, err = retention.DeleteLesson(ctx, deletion)
	require.NoError(t, err)
	for _, kind := range assurance.LessonPayloadKinds() {
		rows, readErr := driver.Select(ctx, assurance.Query{Kind: kind, Limit: 100})
		require.NoError(t, readErr)
		for _, row := range rows {
			if len(row.ID) > len(original.ID.String()) && row.ID[:len(original.ID.String())] == original.ID.String() {
				require.True(t, row.Deleted)
				require.Equal(t, "{}", row.Body)
			}
		}
	}
}
