package evidence_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func artifact(t *testing.T) evidence.Artifact {
	t.Helper()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{
		SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}},
		Version: "1", MediaType: "text/plain", Producer: "test", Sensitivity: evidence.SensitivityInternal,
		CreatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), RetainUntil: time.Date(2027, 10, 18, 12, 0, 0, 0, time.UTC),
	}, Data: []byte("captured result")})
	require.NoError(t, err)
	return a
}

func TestArtifactDigestMismatchRejected(t *testing.T) {
	a := artifact(t)
	a.Data[0] = 'X'
	require.ErrorIs(t, a.Validate(), evidence.ErrDigestMismatch)
}

func TestEvidenceDeniedAcrossScope(t *testing.T) {
	a := artifact(t)
	backend := &fixtureStorage{artifact: a}
	repo, err := evidence.NewRepository(backend, allowAll{}, 1024)
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "two"}}})
	_, err = repo.Open(ctx, a.Ref)
	require.ErrorIs(t, err, evidence.ErrDenied)
	require.Zero(t, backend.loads)
}

func TestRepositoryRechecksDigestAndAuthority(t *testing.T) {
	a := artifact(t)
	ctx := cortex.WithScope(context.Background(), a.Ref.Scope)
	t.Run("corrupt bytes", func(t *testing.T) {
		backend := &fixtureStorage{artifact: a}
		backend.artifact.Data = []byte("forged result!!")
		repo, err := evidence.NewRepository(backend, allowAll{}, 1024)
		require.NoError(t, err)
		_, err = repo.Open(ctx, a.Ref)
		require.ErrorIs(t, err, evidence.ErrDigestMismatch)
	})
	t.Run("metadata substitution", func(t *testing.T) {
		backend := &fixtureStorage{artifact: a}
		backend.artifact.Ref.Producer = "other"
		repo, err := evidence.NewRepository(backend, allowAll{}, 1024)
		require.NoError(t, err)
		_, err = repo.Open(ctx, a.Ref)
		require.ErrorIs(t, err, evidence.ErrDigestMismatch)
	})
	t.Run("revoked during retrieval", func(t *testing.T) {
		denied := errors.New("revoked")
		auth := &revokingAuthorizer{denied: denied}
		repo, err := evidence.NewRepository(&fixtureStorage{artifact: a}, auth, 1024)
		require.NoError(t, err)
		_, err = repo.Open(ctx, a.Ref)
		require.ErrorIs(t, err, denied)
	})
	t.Run("valid read", func(t *testing.T) {
		repo, err := evidence.NewRepository(&fixtureStorage{artifact: a}, allowAll{}, 1024)
		require.NoError(t, err)
		reader, err := repo.Open(ctx, a.Ref)
		require.NoError(t, err)
		defer reader.Close()
		got, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, a.Data, got)
	})
}

func TestEvidenceLimitsAndDeleteReason(t *testing.T) {
	a := artifact(t)
	backend := &fixtureStorage{artifact: a}
	repo, err := evidence.NewRepository(backend, allowAll{}, 2)
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), a.Ref.Scope)
	_, err = repo.Put(ctx, a)
	require.ErrorIs(t, err, evidence.ErrTooLarge)
	require.Zero(t, backend.saves)
	require.Error(t, repo.Delete(ctx, a.Ref, ""))
	require.Zero(t, backend.deletes)
}

type fixtureStorage struct {
	artifact              evidence.Artifact
	loads, saves, deletes int
}

func (s *fixtureStorage) SaveEvidence(_ context.Context, a evidence.Artifact) error {
	s.saves++
	s.artifact = a
	return nil
}
func (s *fixtureStorage) LoadEvidence(_ context.Context, _ evidence.Ref) (evidence.Artifact, error) {
	s.loads++
	return s.artifact, nil
}
func (s *fixtureStorage) TombstoneEvidence(_ context.Context, _ evidence.Ref, _ string) error {
	s.deletes++
	return nil
}

type allowAll struct{}

func (allowAll) AuthorizeEvidence(context.Context, evidence.Action, evidence.Ref) error { return nil }

type revokingAuthorizer struct {
	calls  int
	denied error
}

func (a *revokingAuthorizer) AuthorizeEvidence(context.Context, evidence.Action, evidence.Ref) error {
	a.calls++
	if a.calls > 1 {
		return a.denied
	}
	return nil
}
