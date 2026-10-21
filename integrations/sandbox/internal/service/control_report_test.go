package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type fixtureReportSession struct {
	claim    WorkerClaim
	submit   func(context.Context, sandbox.Result) (WorkerReportReceipt, error)
	closes   atomic.Int32
	closeErr error
	deadline time.Time
}

func (s *fixtureReportSession) Submit(ctx context.Context, result sandbox.Result) (WorkerReportReceipt, error) {
	if s.submit != nil {
		return s.submit(ctx, result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	return WorkerReportReceipt{SchemaVersion: 1, JobID: s.claim.JobID, ClaimRevision: s.claim.Revision, ProposalDigest: evidence.Digest(body)}, ctx.Err()
}
func (s *fixtureReportSession) Deadline() time.Time {
	if !s.deadline.IsZero() {
		return s.deadline
	}
	return time.Now().Add(time.Minute)
}
func (s *fixtureReportSession) Close() error { s.closes.Add(1); return s.closeErr }

func startReportControl(t *testing.T, begin func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error), limits ControlLimits) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, path := controlListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	server := &WorkerControl{limits: limits, authorize: func(context.Context, *net.UnixConn, WorkerClaim) error { return nil }, beginReport: begin}
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx, listener) }()
	return path, cancel, done
}

func testWireResult() sandbox.Result {
	return sandbox.Result{JobDigest: evidence.Digest([]byte("job")), State: sandbox.StateExited, ExitCode: 0, Stdout: []byte("reported output")}
}

func TestReportControlReturnsOnlyBoundDurableReceipt(t *testing.T) {
	claim := controlClaim()
	session := &fixtureReportSession{claim: claim}
	var starts atomic.Int32
	path, cancel, done := startReportControl(t, func(_ context.Context, _ *net.UnixConn, got WorkerClaim) (reportSession, error) {
		starts.Add(1)
		if got != claim {
			return nil, broker.ErrDenied
		}
		return session, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	result := testWireResult()
	receipt, err := reportConnection(t.Context(), dialControl(t, path), claim, result, time.Second)
	body, _ := json.Marshal(result)
	if err != nil || receipt.JobID != claim.JobID || receipt.ClaimRevision != claim.Revision || receipt.ProposalDigest != evidence.Digest(body) {
		t.Fatalf("report rejected: %+v %v", receipt, err)
	}
	if starts.Load() != 1 || session.closes.Load() != 1 {
		t.Fatal("report identity was not acquired and closed once")
	}
	// Authorization-only clients remain compatible on the same listener.
	if err = authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestReportControlAuthenticatesBeforeAcceptingPayload(t *testing.T) {
	var attempts atomic.Int32
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		attempts.Add(1)
		return nil, errors.New("private capability")
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	claim := controlClaim()
	result := testWireResult()
	body, _ := json.Marshal(result)
	header := reportHeader{SchemaVersion: 2, Operation: "report", Claim: claim, Bytes: len(body), Digest: evidence.Digest(body)}
	conn := dialControl(t, path)
	if err := writeControlFrame(conn, header); err != nil {
		t.Fatal(err)
	}
	var reply reportReply
	if _, err := readControlFrame(conn, &reply); err != nil || reply.Status != "denied" || reply.Receipt != nil {
		t.Fatalf("unverified peer admitted report: %+v %v", reply, err)
	}
	if attempts.Load() != 1 {
		t.Fatal("identity was not checked before payload")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func wireHeader(claim WorkerClaim, result sandbox.Result) (reportHeader, []byte) {
	body, _ := json.Marshal(result)
	return reportHeader{SchemaVersion: 2, Operation: "report", Claim: claim, Bytes: len(body), Digest: evidence.Digest(body)}, body
}

func awaitReportReady(t *testing.T, conn *net.UnixConn, header reportHeader) {
	t.Helper()
	if err := writeControlFrame(conn, header); err != nil {
		t.Fatal(err)
	}
	var reply reportReply
	if _, err := readControlFrame(conn, &reply); err != nil || reply.Status != "ready" || reply.Receipt != nil {
		t.Fatalf("report not admitted: %+v %v", reply, err)
	}
}

func TestReportControlBindsPayloadBeforeSubmission(t *testing.T) {
	for _, mode := range []string{"changed digest", "changed size", "unknown field", "duplicate field", "truncated", "extra frame", "missing EOF"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			result := testWireResult()
			header, body := wireHeader(claim, result)
			switch mode {
			case "changed digest":
				header.Digest = evidence.Digest([]byte("different"))
			case "changed size":
				header.Bytes++
			case "unknown field":
				body = append(body[:len(body)-1], []byte(`,"foreign":true}`)...)
			case "duplicate field":
				body = append(body[:len(body)-1], []byte(`,"exit_code":0}`)...)
			}
			if mode == "unknown field" || mode == "duplicate field" {
				header.Bytes = len(body)
				header.Digest = evidence.Digest(body)
			}
			var calls atomic.Int32
			session := &fixtureReportSession{claim: claim, submit: func(context.Context, sandbox.Result) (WorkerReportReceipt, error) {
				calls.Add(1)
				return WorkerReportReceipt{}, nil
			}}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: 100 * time.Millisecond})
			conn := dialControl(t, path)
			awaitReportReady(t, conn, header)
			if mode == "truncated" {
				_, _ = conn.Write([]byte{0, 0, 1, 0, '{'})
			} else {
				_ = writeBoundedFrame(conn, body, reportFrameLimit)
			}
			if mode == "extra frame" {
				_ = writeControlFrame(conn, header)
			}
			if mode != "missing EOF" {
				_ = conn.CloseWrite()
			}
			var reply reportReply
			if _, err := readControlFrame(conn, &reply); err == nil && reply.Status == "reported" {
				t.Fatal("malformed report acknowledged")
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if calls.Load() != 0 || session.closes.Load() != 1 {
				t.Fatal("malformed payload reached submission or leaked peer")
			}
		})
	}
}

func TestReportControlReservesAuthorizationCapacity(t *testing.T) {
	claim := controlClaim()
	header, _ := wireHeader(claim, testWireResult())
	var opens atomic.Int32
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		opens.Add(1)
		return &fixtureReportSession{claim: claim}, nil
	}, ControlLimits{Connections: 3, Reports: 1, Timeout: time.Second})
	first := dialControl(t, path)
	awaitReportReady(t, first, header)
	second := dialControl(t, path)
	if err := writeControlFrame(second, header); err != nil {
		t.Fatal(err)
	}
	var denied reportReply
	if _, err := readControlFrame(second, &denied); err != nil || denied.Status != "busy" {
		t.Fatal("report ceiling was not enforced", err)
	}
	if opens.Load() != 1 {
		t.Fatal("full report capacity acquired another process handle")
	}
	if _, pendingErr := reportConnection(t.Context(), dialControl(t, path), claim, testWireResult(), time.Second); !errors.Is(pendingErr, broker.ErrPending) {
		t.Fatalf("capacity was not distinguished from denial: %v", pendingErr)
	}
	if err := authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); err != nil {
		t.Fatal("report occupied reserved authorization capacity", err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestReportControlSanitizesSubmissionFailureAndClosesPeer(t *testing.T) {
	claim := controlClaim()
	for _, mode := range []string{"submit error", "wrong receipt", "open error with peer", "close error"} {
		t.Run(mode, func(t *testing.T) {
			session := &fixtureReportSession{claim: claim}
			if mode == "submit error" {
				session.submit = func(context.Context, sandbox.Result) (WorkerReportReceipt, error) {
					return WorkerReportReceipt{}, errors.New(claim.Token)
				}
			}
			if mode == "wrong receipt" {
				session.submit = func(context.Context, sandbox.Result) (WorkerReportReceipt, error) {
					return WorkerReportReceipt{SchemaVersion: 1}, nil
				}
			}
			if mode == "close error" {
				session.closeErr = errors.New(claim.Token)
			}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
				if mode == "open error with peer" {
					return session, errors.New(claim.Token)
				}
				return session, nil
			}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			receipt, err := reportConnection(t.Context(), dialControl(t, path), claim, testWireResult(), time.Second)
			if !errors.Is(err, broker.ErrDenied) || !receipt.JobID.IsNil() || strings.Contains(err.Error(), claim.Token) {
				t.Fatalf("unsafe report failure: %+v %v", receipt, err)
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.closes.Load() != 1 {
				t.Fatal("failed report leaked or closed the peer twice")
			}
		})
	}
}

func TestReportControlStopsAtSignedDeadlineAndShutdown(t *testing.T) {
	for _, mode := range []string{"expiry", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			header, _ := wireHeader(claim, testWireResult())
			session := &fixtureReportSession{claim: claim}
			if mode == "expiry" {
				session.deadline = time.Now().Add(100 * time.Millisecond)
			}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			conn := dialControl(t, path)
			awaitReportReady(t, conn, header)
			if mode == "shutdown" {
				cancel()
			}
			var reply [1]byte
			if n, err := conn.Read(reply[:]); n != 0 || err == nil {
				t.Fatal("expired or stopped upload remained open")
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.closes.Load() != 1 {
				t.Fatal("stopped upload retained peer handle")
			}
		})
	}
}

func TestReportControlRechecksAuthorityAfterUpload(t *testing.T) {
	for _, mode := range []string{"accepted", "revoked", "peer exit"} {
		t.Run(mode, func(t *testing.T) {
			var revoked atomic.Bool
			controller, claim, peer, _ := authorizationFixture(t, func(c *Controller, _ *fixtureWorkerPeer) {
				c.scanner.revalidate = func(context.Context, sandbox.Job) error {
					if revoked.Load() {
						return broker.ErrDenied
					}
					return nil
				}
			})
			current, err := controller.launcher.store.Load(t.Context(), claim.JobID)
			if err != nil {
				t.Fatal(err)
			}
			result := testWireResult()
			result.JobDigest = current.Receipt.Job.Digest
			header, payload := wireHeader(claim, result)
			session := &fixtureReportSession{claim: claim, deadline: acceptanceDeadline(current.Receipt), submit: func(ctx context.Context, result sandbox.Result) (WorkerReportReceipt, error) {
				return controller.proposeWorkerResult(ctx, peer, claim, result)
			}}
			path, cancel, done := startReportControl(t, func(ctx context.Context, _ *net.UnixConn, request WorkerClaim) (reportSession, error) {
				if _, authErr := controller.authorizeWorker(ctx, peer, request); authErr != nil {
					return nil, authErr
				}
				return session, nil
			}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			conn := dialControl(t, path)
			awaitReportReady(t, conn, header)
			if mode == "revoked" {
				revoked.Store(true)
			}
			if mode == "peer exit" {
				peer.dieAfterFirst.Store(true)
			}
			if err = writeBoundedFrame(conn, payload, reportFrameLimit); err != nil {
				t.Fatal(err)
			}
			if err = conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			var reply reportReply
			if _, err = readControlFrame(conn, &reply); err != nil {
				t.Fatal(err)
			}
			saved, err := controller.launcher.store.Load(t.Context(), claim.JobID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "accepted" {
				if reply.Status != "reported" || reply.Receipt == nil || saved.Proposal == nil || saved.Proposal.Digest != header.Digest {
					t.Fatal("authenticated report was not persisted")
				}
			} else if reply.Status != "denied" || reply.Receipt != nil || saved.Proposal != nil {
				t.Fatal("changed authority persisted a report")
			}
			cancel()
			_ = awaitControllerStop(t, done)
		})
	}
}

func TestReportControlLostAcknowledgementRetainsOneProposal(t *testing.T) {
	controller, claim, peer, _ := authorizationFixture(t)
	current, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	result := testWireResult()
	result.JobDigest = current.Receipt.Job.Digest
	header, payload := wireHeader(claim, result)
	submitted := make(chan error, 1)
	var writes atomic.Int32
	session := &fixtureReportSession{claim: claim, deadline: acceptanceDeadline(current.Receipt), submit: func(ctx context.Context, result sandbox.Result) (WorkerReportReceipt, error) {
		writes.Add(1)
		receipt, writeErr := controller.proposeWorkerResult(ctx, peer, claim, result)
		submitted <- writeErr
		return receipt, writeErr
	}}
	path, cancel, done := startReportControl(t, func(ctx context.Context, _ *net.UnixConn, request WorkerClaim) (reportSession, error) {
		if _, authErr := controller.authorizeWorker(ctx, peer, request); authErr != nil {
			return nil, authErr
		}
		return session, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	conn := dialControl(t, path)
	awaitReportReady(t, conn, header)
	if err = writeBoundedFrame(conn, payload, reportFrameLimit); err != nil {
		t.Fatal(err)
	}
	_ = conn.CloseWrite()
	_ = conn.Close()
	if err = awaitControllerStop(t, submitted); err != nil {
		t.Fatal(err)
	}
	saved, err := controller.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil || saved.Proposal == nil || saved.Proposal.Digest != header.Digest || writes.Load() != 1 {
		t.Fatal("lost acknowledgement lost or repeated report", err)
	}
	if _, err = controller.launcher.store.VerifyClaim(t.Context(), claim.JobID, claim.Token, claim.Revision); err == nil {
		t.Fatal("reported worker retained execution authority")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestReportClientRejectsUnboundReadinessAndReceipts(t *testing.T) {
	claim := controlClaim()
	result := testWireResult()
	for _, mode := range []string{"ready evidence", "busy evidence", "final evidence", "ready digest", "ready receipt", "ready capture", "busy capture", "final capture", "busy digest", "busy receipt", "wrong job", "wrong revision", "wrong result", "wrong schema", "missing receipt", "trailing frame"} {
		t.Run(mode, func(t *testing.T) {
			listener, path := controlListener(t)
			readPayload := make(chan bool, 1)
			go func() {
				conn, err := listener.AcceptUnix()
				if err != nil {
					readPayload <- false
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var header reportHeader
				body, err := readControlFrame(conn, &header)
				if err != nil {
					readPayload <- false
					return
				}
				ready := reportReply{SchemaVersion: 2, Status: "ready", RequestDigest: evidence.Digest(body)}
				if mode == "busy digest" || mode == "busy receipt" || mode == "busy capture" || mode == "busy evidence" {
					ready.Status = "busy"
				}
				if mode == "ready digest" || mode == "busy digest" {
					ready.RequestDigest = evidence.Digest([]byte("foreign"))
				}
				if mode == "ready evidence" || mode == "busy evidence" {
					ready.Evidence = &evidenceReceipt{SchemaVersion: 1}
				}
				if mode == "ready receipt" || mode == "busy receipt" {
					ready.Receipt = &WorkerReportReceipt{SchemaVersion: 1}
				}
				if mode == "ready capture" || mode == "busy capture" {
					ready.Capture = &WorkerCaptureReceipt{SchemaVersion: 1}
				}
				if err = writeControlFrame(conn, ready); err != nil {
					readPayload <- false
					return
				}
				payload, payloadErr := readBoundedFrame(conn, reportFrameLimit)
				readPayload <- payloadErr == nil && len(payload) > 0
				if payloadErr != nil {
					return
				}
				if requireControlEOF(conn) != nil {
					return
				}
				receipt := WorkerReportReceipt{SchemaVersion: 1, JobID: claim.JobID, ClaimRevision: claim.Revision, ProposalDigest: header.Digest}
				final := reportReply{SchemaVersion: 2, Status: "reported", RequestDigest: evidence.Digest(body), Receipt: &receipt}
				switch mode {
				case "final evidence":
					final.Evidence = &evidenceReceipt{SchemaVersion: 1}
				case "final capture":
					final.Capture = &WorkerCaptureReceipt{SchemaVersion: 1}
				case "wrong job":
					receipt.JobID = controlClaim().JobID
				case "wrong revision":
					receipt.ClaimRevision++
				case "wrong result":
					receipt.ProposalDigest = evidence.Digest([]byte("different"))
				case "wrong schema":
					receipt.SchemaVersion++
				case "missing receipt":
					final.Receipt = nil
				}
				_ = writeControlFrame(conn, final)
				if mode == "trailing frame" {
					_ = writeControlFrame(conn, final)
				}
			}()
			receipt, err := reportConnection(t.Context(), dialControl(t, path), claim, result, time.Second)
			if !errors.Is(err, broker.ErrDenied) || !receipt.JobID.IsNil() {
				t.Fatal("unbound report response accepted")
			}
			select {
			case sent := <-readPayload:
				if (strings.HasPrefix(mode, "ready") || strings.HasPrefix(mode, "busy")) && sent {
					t.Fatal("payload sent before bound readiness")
				}
			case <-time.After(time.Second):
				t.Fatal("report client did not close refused connection")
			}
		})
	}
}

func TestReportControlRejectsUnsupportedHeadersAndLimits(t *testing.T) {
	for _, limits := range []ControlLimits{{Connections: 2, Reports: -1, Timeout: time.Second}, {Connections: 5, Reports: 5, Timeout: time.Second}, {Connections: 2, Reports: 2, Timeout: time.Second}} {
		if limits.validate() == nil {
			t.Fatal("invalid report ceiling accepted")
		}
	}
	for _, mode := range []string{"disabled", "oversized", "negative", "unknown operation", "invalid digest", "invalid claim"} {
		t.Run(mode, func(t *testing.T) {
			header, _ := wireHeader(controlClaim(), testWireResult())
			limits := ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second}
			switch mode {
			case "disabled":
				limits.Reports = 0
			case "oversized":
				header.Bytes = reportFrameLimit + 1
			case "negative":
				header.Bytes = -1
			case "unknown operation":
				header.Operation = "complete"
			case "invalid digest":
				header.Digest = "invalid"
			case "invalid claim":
				header.Claim.Revision = 1
			}
			var calls atomic.Int32
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
				calls.Add(1)
				return nil, broker.ErrDenied
			}, limits)
			conn := dialControl(t, path)
			if err := writeControlFrame(conn, header); err != nil {
				t.Fatal(err)
			}
			var reply reportReply
			if _, err := readControlFrame(conn, &reply); err != nil || reply.Status != "denied" || calls.Load() != 0 {
				t.Fatal("invalid header reached identity acquisition", err)
			}
			cancel()
			_ = awaitControllerStop(t, done)
		})
	}
}

func TestReportClientBoundsDataBeforeEncoding(t *testing.T) {
	for _, mode := range []string{"stdout", "stderr", "reason", "metadata", "artifact count", "signature", "running", "exit"} {
		t.Run(mode, func(t *testing.T) {
			result := testWireResult()
			switch mode {
			case "stdout":
				result.Stdout = make([]byte, (1<<20)+1)
			case "stderr":
				result.Stdout = make([]byte, 1<<20)
				result.Stderr = []byte("x")
			case "reason":
				result.Reason = strings.Repeat("x", 4097)
			case "metadata":
				result.Artifacts = []evidence.Ref{{SHA256: evidence.Digest([]byte("artifact")), SourceURI: strings.Repeat("x", reportFrameLimit+1)}}
			case "artifact count":
				result.Artifacts = make([]evidence.Ref, 257)
			case "signature":
				result.Signature = []byte("not signed by broker")
			case "running":
				result.State = sandbox.StateRunning
			case "exit":
				result.ExitCode = 256
			}
			if _, _, err := prepareReport(controlClaim(), result); err == nil {
				t.Fatal("unbounded or nonterminal report encoded")
			}
		})
	}
}

func TestReportClientValidatesArtifactDigestsBeforeEncoding(t *testing.T) {
	result := testWireResult()
	result.Artifacts = []evidence.Ref{{SHA256: evidence.Digest([]byte("artifact"))}}
	if !boundedReportInput(result) {
		t.Fatal("bounded digest refused")
	}
	for _, digest := range []string{"", strings.Repeat("z", 64), strings.Repeat("a", reportFrameLimit+1)} {
		result.Artifacts[0].SHA256 = digest
		if boundedReportInput(result) {
			t.Fatal("invalid artifact digest reached report encoding")
		}
	}
}

func TestReportControlCarriesMaximumOutputWithoutLargerFrames(t *testing.T) {
	claim := controlClaim()
	// This exercises frame capacity, not throughput under race instrumentation.
	const timeout = 10 * time.Second
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		return &fixtureReportSession{claim: claim}, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: timeout})
	result := testWireResult()
	result.Stdout = bytes.Repeat([]byte("x"), 1<<20)
	if receipt, err := reportConnection(t.Context(), dialControl(t, path), claim, result, timeout); err != nil || receipt.JobID != claim.JobID {
		t.Fatal("bounded full output report failed", err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestReportControlRejectsOversizedPayloadPrefixBeforeSubmission(t *testing.T) {
	claim := controlClaim()
	header, _ := wireHeader(claim, testWireResult())
	header.Bytes = reportFrameLimit
	var calls atomic.Int32
	session := &fixtureReportSession{claim: claim, submit: func(context.Context, sandbox.Result) (WorkerReportReceipt, error) {
		calls.Add(1)
		return WorkerReportReceipt{}, nil
	}}
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	conn := dialControl(t, path)
	awaitReportReady(t, conn, header)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], reportFrameLimit+1)
	_, _ = conn.Write(prefix[:])
	_ = conn.CloseWrite()
	var reply reportReply
	if _, err := readControlFrame(conn, &reply); err == nil && reply.Status == "reported" {
		t.Fatal("oversized payload prefix accepted")
	}
	cancel()
	_ = awaitControllerStop(t, done)
	if calls.Load() != 0 || session.closes.Load() != 1 {
		t.Fatal("oversized report submitted or leaked handle")
	}
}
