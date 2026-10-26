package suspension_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/suspension"
)

type closureFence func(context.Context, suspension.ResumeClosureRequest, func(suspension.ResumeClosureRequest) error) error

func (f closureFence) WithResumeClosure(ctx context.Context, req suspension.ResumeClosureRequest, next func(suspension.ResumeClosureRequest) error) error {
	return f(ctx, req, next)
}

type closureAuthorizer func(context.Context, suspension.ResumeClosureRequest) error

func (f closureAuthorizer) AuthorizeResumeClosure(ctx context.Context, req suspension.ResumeClosureRequest) error {
	return f(ctx, req)
}

type closureStore struct {
	suspension.ResumeClosureStore
	close func(context.Context, suspension.ResumeClosureRequest) (suspension.ResumeClosure, error)
}

func (s *closureStore) CloseResume(ctx context.Context, req suspension.ResumeClosureRequest) (suspension.ResumeClosure, error) {
	return s.close(ctx, req)
}

func closureRequest() suspension.ResumeClosureRequest {
	now := time.Now().UTC()
	return suspension.ResumeClosureRequest{SchemaVersion: 1, Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "a"}}}, Version: suspension.ClaimVersion{RunID: id.NewAgentRunID(), SuspensionID: id.NewSuspensionID(), RunRevision: 2}, AdmissionDigest: evidence.Digest([]byte("admission")), StartDigest: evidence.Digest([]byte("start")), RunRevision: 4, Actor: "operator", Issuer: "supervisor", IssuerRevision: "1", ProofRef: "original writers fenced", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
}

func TestResumeCloserAuthority(t *testing.T) {
	for _, mode := range []string{"close", "denied", "cancelled", "changed", "mutated_authorization", "missing_callback", "repeat_callback", "escaped_callback", "bad_receipt", "store_error", "fence_error", "wrong_scope"} {
		t.Run(mode, func(t *testing.T) {
			req := closureRequest()
			ctx, cancel := context.WithCancel(cortex.WithScope(t.Context(), req.Scope))
			defer cancel()
			if mode == "wrong_scope" {
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "b"}}})
			}
			var calls, authorized int
			denied := errors.New("denied")
			var escaped func(suspension.ResumeClosureRequest) error
			storage := &closureStore{close: func(got context.Context, value suspension.ResumeClosureRequest) (suspension.ResumeClosure, error) {
				calls++
				require.NoError(t, got.Err())
				require.Equal(t, req.Scope.Canonical(), value.Scope.Canonical())
				deadline, ok := got.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
				_, digest, err := value.Freeze()
				require.NoError(t, err)
				if mode == "store_error" {
					return suspension.ResumeClosure{Fresh: true}, denied
				}
				if mode == "bad_receipt" {
					digest = evidence.Digest([]byte("substituted"))
				}
				return suspension.ResumeClosure{Request: value, RequestDigest: digest, EventID: id.New(event.PrefixEvent), RunRevision: value.RunRevision + 1, ClosedAt: value.ObservedAt, Fresh: true}, nil
			}}
			fence := closureFence(func(_ context.Context, offered suspension.ResumeClosureRequest, next func(suspension.ResumeClosureRequest) error) error {
				if mode == "missing_callback" {
					return nil
				}
				if mode == "escaped_callback" {
					escaped = next
					return nil
				}
				if mode == "changed" {
					offered.Scope.Levels[0].Value = "substitution"
				}
				err := next(offered)
				if mode == "repeat_callback" {
					_ = next(offered)
				}
				if mode == "fence_error" {
					return denied
				}
				return err
			})
			authorize := closureAuthorizer(func(_ context.Context, offered suspension.ResumeClosureRequest) error {
				authorized++
				if mode == "denied" {
					return denied
				}
				if mode == "cancelled" {
					cancel()
				}
				if mode == "mutated_authorization" {
					offered.Scope.Levels[0].Value = "mutated"
				}
				return nil
			})
			closer, err := suspension.NewResumeCloser(storage, fence, authorize)
			require.NoError(t, err)
			got, err := closer.Close(ctx, req)
			if mode == "close" || mode == "mutated_authorization" {
				require.NoError(t, err)
				require.True(t, got.Fresh)
			} else {
				require.Error(t, err)
				require.Zero(t, got)
			}
			switch mode {
			case "close", "mutated_authorization", "bad_receipt", "store_error", "repeat_callback", "fence_error":
				require.Equal(t, 1, calls)
			default:
				require.Zero(t, calls)
			}
			if mode == "denied" || mode == "cancelled" {
				require.Equal(t, 1, authorized)
			}
			if escaped != nil {
				require.Error(t, escaped(req))
				require.Zero(t, calls)
			}
			require.Equal(t, "a", req.Scope.Levels[0].Value)
		})
	}
}

func TestResumeCloserRequiresPorts(t *testing.T) {
	var nilStore *closureStore
	var nilFence closureFence
	var nilAuthority closureAuthorizer
	_, err := suspension.NewResumeCloser(nilStore, nilFence, nilAuthority)
	require.Error(t, err)
}

func TestResumeClosureBindings(t *testing.T) {
	for _, mode := range []string{"valid", "schema", "revision", "admission", "start", "proof", "issuer", "window", "time", "scope"} {
		t.Run(mode, func(t *testing.T) {
			req := closureRequest()
			switch mode {
			case "schema":
				req.SchemaVersion = 2
			case "revision":
				req.RunRevision = req.Version.RunRevision
			case "admission":
				req.AdmissionDigest = ""
			case "start":
				req.StartDigest = ""
			case "proof":
				req.ProofDigest = ""
			case "issuer":
				req.Issuer = " "
			case "window":
				req.ExpiresAt = req.ObservedAt.Add(6 * time.Minute)
			case "time":
				req.ExpiresAt = req.ObservedAt
			case "scope":
				req.Scope = cortex.Scope{}
			}
			frozen, digest, err := req.Freeze()
			if mode != "valid" {
				require.Error(t, err)
				require.Empty(t, digest)
				return
			}
			require.NoError(t, err)
			require.True(t, evidence.ValidDigest(digest))
			frozen.Scope.Levels[0].Value = "changed"
			require.Equal(t, "a", req.Scope.Levels[0].Value)
		})
	}
}
