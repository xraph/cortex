package service

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

type fixtureCaptureSession struct {
	fixtureReportSession
	capture func(context.Context, evidence.Ref, bool) (WorkerCaptureReceipt, error)
	calls   int
}

func (s *fixtureCaptureSession) Capture(ctx context.Context, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
	s.calls++
	if s.capture != nil {
		return s.capture(ctx, ref, confirm)
	}
	digest, err := ref.BindingDigest()
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	state, revision := workerstore.CaptureIntent, uint64(1)
	if confirm {
		state, revision = workerstore.CaptureVerified, 2
	}
	return WorkerCaptureReceipt{SchemaVersion: 1, JobID: s.claim.JobID, ClaimRevision: s.claim.Revision, ArtifactID: ref.ID, BindingDigest: digest, State: state, Revision: revision}, ctx.Err()
}
func wireCaptureRef(t *testing.T) evidence.Ref {
	t.Helper()
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, MediaType: "text/plain", Version: evidence.Digest([]byte("job")), Producer: "cortex/sandbox", SourceURI: "fixture", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("candidate")})
	if err != nil {
		t.Fatal(err)
	}
	return a.Ref
}
func TestCaptureTransportSharesAuthenticatedBoundedUpload(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		claim := controlClaim()
		session := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}
		path, cancel, done := startReportControl(t, func(_ context.Context, _ *net.UnixConn, got WorkerClaim) (reportSession, error) {
			if got != claim {
				return nil, broker.ErrDenied
			}
			return session, nil
		}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
		ref := wireCaptureRef(t)
		receipt, err := captureConnection(t.Context(), dialControl(t, path), claim, ref, confirm, time.Second)
		if err != nil || receipt.ArtifactID != ref.ID || receipt.JobID != claim.JobID || session.calls != 1 || session.closes.Load() != 1 {
			t.Fatal("capture transport", receipt, err)
		}
		cancel()
		_ = awaitControllerStop(t, done)
	}
}
func TestCaptureTransportRefusesUncertainAndUnboundAcknowledgements(t *testing.T) {
	for _, mode := range []string{"failure", "identity", "binding", "claim", "state", "close", "expired"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			ref := wireCaptureRef(t)
			session := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}
			plain := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}
			session.capture = func(ctx context.Context, r evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
				ack, err := plain.Capture(ctx, r, confirm)
				switch mode {
				case "failure":
					return WorkerCaptureReceipt{}, errors.New("private journal failure")
				case "identity":
					ack.ArtifactID = id.New(evidence.PrefixArtifact)
				case "binding":
					ack.BindingDigest = evidence.Digest([]byte("other"))
				case "claim":
					ack.ClaimRevision++
				case "state":
					ack.State = workerstore.CaptureDiscarded
				}
				return ack, err
			}
			if mode == "close" {
				session.closeErr = errors.New("peer close failed")
			}
			if mode == "expired" {
				session.deadline = time.Now().Add(-time.Second)
			}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			if ack, err := captureConnection(t.Context(), dialControl(t, path), claim, ref, false, time.Second); err == nil || !ack.ArtifactID.IsNil() {
				t.Fatal("unbound capture receipt", ack, err)
			}
			cancel()
			_ = awaitControllerStop(t, done)
			// Deadline cancellation can close the socket before the handler's
			// deferred peer cleanup runs. Join it before inspecting its state.
			if session.calls > 1 || session.closes.Load() != 1 {
				t.Fatal("capture replayed or identity leaked")
			}
		})
	}
}
func TestCaptureTransportDeniesUnsupportedSessionBeforePayload(t *testing.T) {
	claim := controlClaim()
	session := &fixtureReportSession{claim: claim}
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	conn := dialControl(t, path)
	header, _, err := prepareCapture(claim, wireCaptureRef(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeControlFrame(conn, header); err != nil {
		t.Fatal(err)
	}
	var reply reportReply
	if _, err = readControlFrame(conn, &reply); err != nil || reply.Status != "denied" {
		t.Fatal("unsupported session admitted payload", reply, err)
	}
	_ = conn.Close()
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestCaptureTransportEnforcesSmallerLimitAndSharedCapacity(t *testing.T) {
	claim := controlClaim()
	header, _, err := prepareCapture(claim, wireCaptureRef(t), false)
	if err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		opens.Add(1)
		return &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}, nil
	}, ControlLimits{Connections: 3, Reports: 1, Timeout: time.Second})
	oversized := header
	oversized.Bytes = captureFrameLimit + 1
	conn := dialControl(t, path)
	if err = writeControlFrame(conn, oversized); err != nil {
		t.Fatal(err)
	}
	var reply reportReply
	if _, err = readControlFrame(conn, &reply); err != nil || reply.Status != "denied" || opens.Load() != 0 {
		t.Fatal("oversized capture acquired peer", reply, err)
	}
	_ = conn.Close()
	first := dialControl(t, path)
	awaitReportReady(t, first, header)
	if _, err = reportConnection(t.Context(), dialControl(t, path), claim, testWireResult(), time.Second); !errors.Is(err, broker.ErrPending) {
		t.Fatal("capture bypassed shared capacity", err)
	}
	if _, err = captureConnection(t.Context(), dialControl(t, path), claim, wireCaptureRef(t), false, time.Second); !errors.Is(err, broker.ErrPending) {
		t.Fatal("capture overflow admitted", err)
	}
	if err = authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); err != nil {
		t.Fatal("capture occupied authorization reserve", err)
	}
	if opens.Load() != 1 {
		t.Fatal("overflow acquired another peer")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestCaptureTransportRejectsMalformedReferenceBeforePersistence(t *testing.T) {
	for _, mode := range []string{"unknown", "duplicate", "digest", "extra", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			header, body, err := prepareCapture(claim, wireCaptureRef(t), false)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown":
				body = append(body[:len(body)-1], []byte(`,"foreign":true}`)...)
			case "duplicate":
				body = append(body[:len(body)-1], []byte(`,"schema_version":1}`)...)
			}
			header.Bytes, header.Digest = len(body), evidence.Digest(body)
			if mode == "digest" {
				header.Digest = evidence.Digest([]byte("different"))
			}
			session := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			conn := dialControl(t, path)
			awaitReportReady(t, conn, header)
			if mode == "truncated" {
				_, _ = conn.Write([]byte{0, 0, 1, 0, '{'})
			} else {
				_ = writeBoundedFrame(conn, body, captureFrameLimit)
			}
			if mode == "extra" {
				_ = writeControlFrame(conn, header)
			}
			_ = conn.CloseWrite()
			var reply reportReply
			if _, err = readControlFrame(conn, &reply); err == nil && reply.Status == "captured" {
				t.Fatal("malformed capture accepted")
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.calls != 0 || session.closes.Load() != 1 {
				t.Fatal("malformed capture persisted or leaked peer")
			}
		})
	}
	ref := wireCaptureRef(t)
	ref.SourceURI = strings.Repeat("x", captureFrameLimit+1)
	if _, _, err := prepareCapture(controlClaim(), ref, false); err == nil {
		t.Fatal("unbounded reference encoded")
	}
}
