package verification_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

type resultContentAuth func(context.Context, verification.ResultContentRequest) error

func (f resultContentAuth) AuthorizeResultContent(ctx context.Context, r verification.ResultContentRequest) error {
	return f(ctx, r)
}

type resultPublisher func(context.Context, verification.ResultPublicationRequest) (safety.PublicationDecision, error)

func (f resultPublisher) PublishResult(ctx context.Context, r verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
	return f(ctx, r)
}

type resultReaderStore struct {
	receipt  func(context.Context, id.ID) (verification.CommitReceipt, error)
	snapshot func(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error)
}

func (s resultReaderStore) GetCommittedResult(ctx context.Context, id id.ID) (verification.CommitReceipt, error) {
	return s.receipt(ctx, id)
}
func (s resultReaderStore) ReadResultSnapshot(ctx context.Context, r verification.ResultReadRequest) (verification.ResultSnapshot, error) {
	return s.snapshot(ctx, r)
}

type resultClock struct{ now time.Time }

func (c *resultClock) Now() time.Time { return c.now }

func TestResultReaderPublishesOnlyAfterCurrentChecks(t *testing.T) {
	for _, status := range []safety.PublicationStatus{safety.StatusApproved, safety.StatusFlagged, safety.StatusRedacted} {
		t.Run(string(status), func(t *testing.T) {
			snapshot, read, now := resultSnapshotFixture(t)
			request := verification.ResultContentRequest{SchemaVersion: 1, RunID: read.RunID, ResultID: read.ResultID}
			ctx := cortex.WithScope(context.Background(), snapshot.Receipt.Request.Current.Scope)
			var steps []string
			held := false
			storage := resultReaderStore{receipt: func(context.Context, id.ID) (verification.CommitReceipt, error) {
				steps = append(steps, "receipt")
				return snapshot.Receipt, nil
			}, snapshot: func(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error) {
				require.True(t, held)
				steps = append(steps, "snapshot")
				return snapshot, nil
			}}
			auth := resultContentAuth(func(context.Context, verification.ResultContentRequest) error {
				steps = append(steps, "authorize")
				return nil
			})
			fence := commitFenceFunc(func(_ context.Context, b verification.Binding, next func(verification.Binding) error) error {
				held = true
				steps = append(steps, "acquire")
				defer func() { held = false; steps = append(steps, "release") }()
				return next(b)
			})
			publisher := resultPublisher(func(_ context.Context, req verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
				require.True(t, held)
				require.Equal(t, "result", req.Content)
				steps = append(steps, "screen")
				text := req.Content
				if status == safety.StatusRedacted {
					text = ""
				}
				return safety.PublicationDecision{Text: text, Status: status}, nil
			})
			reader, err := verification.NewResultReader(verification.ResultReaderConfig{Store: storage, Authorizer: auth, Fence: fence, Publisher: publisher, Clock: &resultClock{now: now}})
			require.NoError(t, err)
			result, err := reader.Read(ctx, request)
			require.NoError(t, err)
			require.Equal(t, request.RunID, result.RunID)
			require.Equal(t, status, result.PublicationStatus)
			require.Equal(t, status == safety.StatusRedacted, result.ContentChanged)
			require.Equal(t, verification.StatusPassed, result.VerificationStatus)
			require.Equal(t, []string{"authorize", "receipt", "acquire", "authorize", "snapshot", "screen", "snapshot", "authorize", "release"}, steps)
		})
	}
}

func TestResultReaderDiscardsContentOnFailure(t *testing.T) {
	for _, mode := range []string{"denied", "revoked", "scope", "run", "receipt", "stale binding", "deleted during screening", "expired during screening", "changed snapshot", "blocked", "provider error", "unscanned", "unknown status", "oversized", "invalid UTF-8", "changed approved", "cancelled during screening", "fence error", "no callback", "double callback", "retained callback"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, read, now := resultSnapshotFixture(t)
			request := verification.ResultContentRequest{SchemaVersion: 1, RunID: read.RunID, ResultID: read.ResultID}
			ctx := cortex.WithScope(context.Background(), snapshot.Receipt.Request.Current.Scope)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			clock := &resultClock{now: now}
			reads, auths, screens, receiptReads := 0, 0, 0, 0
			storage := resultReaderStore{receipt: func(context.Context, id.ID) (verification.CommitReceipt, error) {
				receiptReads++
				r := snapshot.Receipt
				if mode == "receipt" {
					r.Digest = "bad"
				}
				return r, nil
			}, snapshot: func(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error) {
				reads++
				if reads == 2 && mode == "deleted during screening" {
					return verification.ResultSnapshot{}, errors.New("deleted")
				}
				s := snapshot
				if reads == 2 && mode == "changed snapshot" {
					s.Output = "forged"
				}
				return s, nil
			}}
			auth := resultContentAuth(func(context.Context, verification.ResultContentRequest) error {
				auths++
				if mode == "denied" || (mode == "revoked" && auths == 3) {
					return errors.New("private grant reason")
				}
				return nil
			})
			var retained func(verification.Binding) error
			fence := commitFenceFunc(func(_ context.Context, b verification.Binding, next func(verification.Binding) error) error {
				if mode == "no callback" {
					return nil
				}
				if mode == "retained callback" {
					retained = next
					return nil
				}
				if mode == "stale binding" {
					b.Context.AuthorityRevision = "new"
				}
				err := next(b)
				if mode == "double callback" {
					_ = next(b)
				}
				if mode == "fence error" {
					return errors.New("private fence reason")
				}
				return err
			})
			publisher := resultPublisher(func(_ context.Context, req verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
				screens++
				switch mode {
				case "expired during screening":
					clock.now = clock.now.Add(2 * time.Hour)
				case "cancelled during screening":
					cancel()
				case "blocked":
					return safety.PublicationDecision{Text: req.Content}, safety.ErrBlocked
				case "provider error":
					return safety.PublicationDecision{Text: req.Content}, errors.New("private provider text")
				case "unscanned":
					return safety.PublicationDecision{Text: req.Content, Status: safety.StatusUnscanned}, nil
				case "unknown status":
					return safety.PublicationDecision{Text: req.Content, Status: "unknown"}, nil
				case "invalid UTF-8":
					return safety.PublicationDecision{Text: string([]byte{0xff}), Status: safety.StatusRedacted}, nil
				case "oversized":
					return safety.PublicationDecision{Text: string(make([]byte, (1<<20)+1)), Status: safety.StatusRedacted}, nil
				case "changed approved":
					return safety.PublicationDecision{Text: "rewritten", Status: safety.StatusApproved}, nil
				}
				return safety.PublicationDecision{Text: req.Content, Status: safety.StatusApproved}, nil
			})
			if mode == "scope" {
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			}
			if mode == "run" {
				request.RunID = id.NewAgentRunID()
			}
			reader, err := verification.NewResultReader(verification.ResultReaderConfig{Store: storage, Authorizer: auth, Fence: fence, Publisher: publisher, Clock: clock})
			require.NoError(t, err)
			result, err := reader.Read(ctx, request)
			require.Error(t, err)
			require.Zero(t, result)
			require.NotContains(t, err.Error(), "private")
			if mode == "denied" {
				require.Zero(t, reads)
				require.Zero(t, receiptReads)
				require.Zero(t, screens)
			}
			if retained != nil {
				require.Error(t, retained(snapshot.Receipt.Request.Current))
				require.Zero(t, reads)
			}
		})
	}
}

func TestResultReaderRequiresEveryHostPort(t *testing.T) {
	for _, mode := range []string{"store", "authorizer", "fence", "publisher", "clock"} {
		t.Run(mode, func(t *testing.T) {
			config := verification.ResultReaderConfig{Store: resultReaderStore{}, Authorizer: resultContentAuth(func(context.Context, verification.ResultContentRequest) error { return nil }), Fence: commitFenceFunc(func(_ context.Context, b verification.Binding, fn func(verification.Binding) error) error {
				return fn(b)
			}), Publisher: resultPublisher(func(context.Context, verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
				return safety.PublicationDecision{}, nil
			}), Clock: &resultClock{}}
			switch mode {
			case "store":
				var s *resultReaderStore
				config.Store = s
			case "authorizer":
				var a resultContentAuth
				config.Authorizer = a
			case "fence":
				var f commitFenceFunc
				config.Fence = f
			case "publisher":
				var p resultPublisher
				config.Publisher = p
			case "clock":
				var c *resultClock
				config.Clock = c
			}
			reader, err := verification.NewResultReader(config)
			require.Error(t, err)
			require.Nil(t, reader)
		})
	}
}

func TestResultReaderRechecksAfterFenceRelease(t *testing.T) {
	snapshot, read, now := resultSnapshotFixture(t)
	clock := &resultClock{now: now}
	reader, err := verification.NewResultReader(verification.ResultReaderConfig{Store: resultReaderStore{receipt: func(context.Context, id.ID) (verification.CommitReceipt, error) { return snapshot.Receipt, nil }, snapshot: func(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error) {
		return snapshot, nil
	}}, Authorizer: resultContentAuth(func(context.Context, verification.ResultContentRequest) error { return nil }), Fence: commitFenceFunc(func(_ context.Context, b verification.Binding, next func(verification.Binding) error) error {
		err := next(b)
		clock.now = now.Add(2 * time.Hour)
		return err
	}), Publisher: resultPublisher(func(_ context.Context, r verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
		return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
	}), Clock: clock})
	require.NoError(t, err)
	result, err := reader.Read(cortex.WithScope(context.Background(), snapshot.Receipt.Request.Current.Scope), verification.ResultContentRequest{SchemaVersion: 1, RunID: read.RunID, ResultID: read.ResultID})
	require.Error(t, err)
	require.Zero(t, result)
}

func TestResultReaderBoundsSlowScreening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		snapshot, read, now := resultSnapshotFixture(t)
		reader, err := verification.NewResultReader(verification.ResultReaderConfig{Store: resultReaderStore{receipt: func(context.Context, id.ID) (verification.CommitReceipt, error) { return snapshot.Receipt, nil }, snapshot: func(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error) {
			return snapshot, nil
		}}, Authorizer: resultContentAuth(func(context.Context, verification.ResultContentRequest) error { return nil }), Fence: commitFenceFunc(func(_ context.Context, b verification.Binding, next func(verification.Binding) error) error {
			return next(b)
		}), Publisher: resultPublisher(func(ctx context.Context, _ verification.ResultPublicationRequest) (safety.PublicationDecision, error) {
			<-ctx.Done()
			return safety.PublicationDecision{}, ctx.Err()
		}), Clock: &resultClock{now: now}})
		require.NoError(t, err)
		started := time.Now()
		result, err := reader.Read(cortex.WithScope(context.Background(), snapshot.Receipt.Request.Current.Scope), verification.ResultContentRequest{SchemaVersion: 1, RunID: read.RunID, ResultID: read.ResultID})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Zero(t, result)
		require.Equal(t, verification.MaxResultReadDuration, time.Since(started))
	})
}
