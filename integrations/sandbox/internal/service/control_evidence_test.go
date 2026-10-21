package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
)

type fixtureEvidenceSession struct {
	fixtureReportSession
	data []byte
	read func(context.Context, evidence.Ref, bool, io.Writer) error
	put  func(context.Context, evidence.Artifact) (evidence.Ref, error)
}

func TestEvidenceDownloadRejectsUnboundCompletionAndMalformedChunks(t *testing.T) {
	for _, mode := range []string{"missing-end", "missing-receipt", "wrong-request", "wrong-binding", "mixed-receipt", "trailing-frame", "oversized-chunk", "truncated-chunk", "ready-evidence"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			ref := wireCaptureRef(t)
			listener, path := controlListener(t)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				conn, err := listener.AcceptUnix()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var header reportHeader
				body, err := readControlFrame(conn, &header)
				if err != nil {
					return
				}
				ready := reportReply{SchemaVersion: 2, Status: "ready", RequestDigest: evidence.Digest(body)}
				if mode == "ready-evidence" {
					ready.Evidence = &evidenceReceipt{SchemaVersion: 1}
				}
				if err = writeControlFrame(conn, ready); err != nil {
					return
				}
				if _, err = readBoundedFrame(conn, captureFrameLimit); err != nil {
					return
				}
				if requireControlEOF(conn) != nil {
					return
				}
				if mode == "oversized-chunk" || mode == "truncated-chunk" {
					var prefix [4]byte
					size := uint32(evidenceChunkLimit + 1)
					if mode == "truncated-chunk" {
						size = 10
					}
					binary.BigEndian.PutUint32(prefix[:], size)
					_, _ = conn.Write(prefix[:])
					return
				}
				_ = writeBoundedFrame(conn, []byte("candidate"), evidenceChunkLimit)
				if mode == "missing-end" {
					return
				}
				_ = endEvidenceStream(conn)
				if mode == "missing-receipt" {
					return
				}
				receipt, _ := newEvidenceReceipt(header, ref)
				final := reportReply{SchemaVersion: 2, Status: "read", RequestDigest: evidence.Digest(body), Evidence: receipt}
				switch mode {
				case "wrong-request":
					final.RequestDigest = evidence.Digest([]byte("foreign"))
				case "wrong-binding":
					receipt.BindingDigest = evidence.Digest([]byte("foreign"))
				case "mixed-receipt":
					final.Capture = &WorkerCaptureReceipt{SchemaVersion: 1}
				}
				_ = writeControlFrame(conn, final)
				if mode == "trailing-frame" {
					_ = writeControlFrame(conn, final)
				}
			}()
			if err := readEvidenceConnection(t.Context(), dialControl(t, path), claim, ref, false, io.Discard, time.Second); err == nil {
				t.Fatal("invalid stream accepted")
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("refused stream leaked connection")
			}
		})
	}
}

func TestEvidenceTransportSharesCapacityAndPreservesAuthorityReserve(t *testing.T) {
	claim := controlClaim()
	header, _, err := prepareEvidenceRead(claim, wireCaptureRef(t), false)
	if err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		opens.Add(1)
		return &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}}, nil
	}, ControlLimits{Connections: 3, Reports: 1, Timeout: time.Second})
	conn := dialControl(t, path)
	awaitReportReady(t, conn, header)
	if err = readEvidenceConnection(t.Context(), dialControl(t, path), claim, wireCaptureRef(t), false, io.Discard, time.Second); !errors.Is(err, broker.ErrPending) {
		t.Fatal("download bypassed capacity", err)
	}
	if _, err = reportConnection(t.Context(), dialControl(t, path), claim, testWireResult(), time.Second); !errors.Is(err, broker.ErrPending) {
		t.Fatal("report bypassed capacity", err)
	}
	if err = authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); err != nil {
		t.Fatal("authorization reserve lost", err)
	}
	if opens.Load() != 1 {
		t.Fatal("overflow acquired peer")
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestEvidenceStreamCancellationJoinsProviderAndPeer(t *testing.T) {
	claim := controlClaim()
	entered := make(chan struct{})
	session := &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}, read: func(ctx context.Context, _ evidence.Ref, _ bool, _ io.Writer) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	client := make(chan error, 1)
	conn, ref := dialControl(t, path), wireCaptureRef(t)
	go func() {
		client <- readEvidenceConnection(t.Context(), conn, claim, ref, false, io.Discard, time.Second)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	_ = awaitControllerStop(t, done)
	select {
	case err := <-client:
		if err == nil {
			t.Fatal("cancelled stream accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled client did not finish")
	}
	if session.closes.Load() != 1 {
		t.Fatal("peer leaked")
	}
}
func (s *fixtureEvidenceSession) ReadEvidence(ctx context.Context, ref evidence.Ref, artifact bool, destination io.Writer) error {
	if s.read != nil {
		return s.read(ctx, ref, artifact, destination)
	}
	_, err := destination.Write(s.data)
	return errors.Join(err, ctx.Err())
}
func (s *fixtureEvidenceSession) PutEvidence(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
	if s.put != nil {
		return s.put(ctx, a)
	}
	s.data = bytes.Clone(a.Data)
	return a.Ref, ctx.Err()
}

func TestEvidenceTransportStreamsBoundContentAndStoresUpload(t *testing.T) {
	for _, size := range []int{0, 1, 64 << 10, 150 << 10, 1 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			claim := controlClaim()
			a, err := evidence.Seal(evidence.Artifact{Ref: wireCaptureRef(t), Data: bytes.Repeat([]byte("x"), size)})
			if err != nil {
				t.Fatal(err)
			}
			session := &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			if ref, putErr := putArtifactConnection(t.Context(), dialControl(t, path), claim, a, time.Second); putErr != nil || ref.ID != a.Ref.ID {
				t.Fatal(ref, putErr)
			}
			for _, artifact := range []bool{false, true} {
				var got bytes.Buffer
				if readErr := readEvidenceConnection(t.Context(), dialControl(t, path), claim, a.Ref, artifact, &got, time.Second); readErr != nil || !bytes.Equal(got.Bytes(), a.Data) {
					t.Fatal("evidence stream", readErr)
				}
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.closes.Load() != 3 {
				t.Fatal("peer lifetime leaked", session.closes.Load())
			}
		})
	}
}

func TestEvidenceTransportRefusesPartialCorruptAndUncertainReads(t *testing.T) {
	for _, mode := range []string{"partial", "corrupt", "too-long", "post-read-error", "ignored-write-error", "close", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			ref := wireCaptureRef(t)
			session := &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}, data: []byte("candidate")}
			switch mode {
			case "partial":
				session.data = []byte("candi")
			case "corrupt":
				session.data = []byte("different")
			case "too-long":
				session.data = []byte("candidate extra")
			case "post-read-error":
				session.read = func(_ context.Context, _ evidence.Ref, _ bool, w io.Writer) error {
					_, _ = w.Write([]byte("candidate"))
					return evidence.ErrDenied
				}
			case "ignored-write-error":
				session.read = func(_ context.Context, _ evidence.Ref, _ bool, w io.Writer) error {
					_, _ = w.Write([]byte("candidate extra"))
					_, _ = w.Write([]byte("candidate"))
					return nil
				}
			case "close":
				session.closeErr = errors.New("peer close failed")
			case "deadline":
				session.deadline = time.Now().Add(-time.Second)
			}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			if err := readEvidenceConnection(t.Context(), dialControl(t, path), claim, ref, false, io.Discard, time.Second); err == nil {
				t.Fatal("unverified stream succeeded")
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.closes.Load() != 1 {
				t.Fatal("peer leaked")
			}
		})
	}
}

func TestEvidenceTransportRequiresCapabilityBeforePayload(t *testing.T) {
	claim := controlClaim()
	for _, operation := range []string{"input-read", "artifact-read", "artifact-put"} {
		t.Run(operation, func(t *testing.T) {
			session := &fixtureReportSession{claim: claim}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			header, _, err := prepareEvidenceRead(claim, wireCaptureRef(t), false)
			if err != nil {
				t.Fatal(err)
			}
			header.Operation = operation
			conn := dialControl(t, path)
			if err = writeControlFrame(conn, header); err != nil {
				t.Fatal(err)
			}
			var reply reportReply
			if _, err = readControlFrame(conn, &reply); err != nil || reply.Status != "denied" {
				t.Fatal("unsupported evidence service admitted payload", reply, err)
			}
			_ = conn.Close()
			cancel()
			_ = awaitControllerStop(t, done)
		})
	}
}

func TestEvidenceUploadDoesNotAcknowledgeChangedOrUncertainReference(t *testing.T) {
	for _, mode := range []string{"failure", "binding", "close"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			a := evidence.Artifact{Ref: wireCaptureRef(t), Data: []byte("candidate")}
			calls := 0
			session := &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim}, put: func(_ context.Context, a evidence.Artifact) (evidence.Ref, error) {
				calls++
				if mode == "failure" {
					return evidence.Ref{}, broker.ErrDenied
				}
				if mode == "binding" {
					a.Ref.Version = "changed"
				}
				return a.Ref, nil
			}}
			if mode == "close" {
				session.closeErr = errors.New("peer close failed")
			}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			if ref, err := putArtifactConnection(t.Context(), dialControl(t, path), claim, a, time.Second); err == nil || !ref.ID.IsNil() || calls != 1 {
				t.Fatal("uncertain upload acknowledged", ref, err)
			}
			cancel()
			_ = awaitControllerStop(t, done)
		})
	}
}

type journalEvidenceSession struct {
	fixtureEvidenceSession
	capture func(context.Context, evidence.Ref, bool) (WorkerCaptureReceipt, error)
}

func (s *journalEvidenceSession) Capture(ctx context.Context, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
	return s.capture(ctx, ref, confirm)
}

func TestEvidenceTransportUsesCurrentAuthorityAndDurableCaptureJournal(t *testing.T) {
	access, c, claim, peer, input, output, repo := workerEvidenceFixture(t)
	// Service membership stays an explicit fixture seam; bytes and capture state
	// cross the actual socket protocol, controller checks and durable journal.
	session := &journalEvidenceSession{fixtureEvidenceSession: fixtureEvidenceSession{
		fixtureReportSession: fixtureReportSession{claim: claim},
		read: func(ctx context.Context, ref evidence.Ref, artifact bool, w io.Writer) error {
			return access.read(ctx, peer, claim, ref, artifact, w)
		},
		put: func(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
			return access.put(ctx, peer, claim, a)
		},
	}, capture: func(ctx context.Context, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
		return c.captureWorker(ctx, peer, claim, ref, confirm)
	}}
	path, cancel, done := startReportControl(t, func(ctx context.Context, _ *net.UnixConn, got WorkerClaim) (reportSession, error) {
		current, err := c.authorizeWorker(ctx, peer, got)
		if err != nil {
			return nil, err
		}
		session.deadline = acceptanceDeadline(current.Receipt)
		return session, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	defer func() { cancel(); _ = awaitControllerStop(t, done) }()
	var content bytes.Buffer
	if err := readEvidenceConnection(t.Context(), dialControl(t, path), claim, input.Ref, false, &content, time.Second); err != nil || !bytes.Equal(content.Bytes(), input.Data) {
		t.Fatal("declared input", err)
	}
	if _, err := putArtifactConnection(t.Context(), dialControl(t, path), claim, output, time.Second); err == nil || repo.puts != 0 {
		t.Fatal("upload bypassed capture intent", err)
	}
	if _, err := captureConnection(t.Context(), dialControl(t, path), claim, output.Ref, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := putArtifactConnection(t.Context(), dialControl(t, path), claim, output, time.Second); err != nil {
		t.Fatal(err)
	}
	content.Reset()
	if err := readEvidenceConnection(t.Context(), dialControl(t, path), claim, output.Ref, true, &content, time.Second); err != nil || !bytes.Equal(content.Bytes(), output.Data) {
		t.Fatal("readback", err)
	}
	if _, err := captureConnection(t.Context(), dialControl(t, path), claim, output.Ref, true, time.Second); err != nil {
		t.Fatal(err)
	}
	captures, err := c.launcher.store.Captures(t.Context(), claim.JobID)
	if err != nil || len(captures) != 1 || captures[0].State != workerstore.CaptureVerified {
		t.Fatal("confirmation did not persist", captures, err)
	}
	if _, err = putArtifactConnection(t.Context(), dialControl(t, path), claim, output, time.Second); err == nil || repo.puts != 1 {
		t.Fatal("upload after confirmation", err)
	}
	peer.dieAfterFirst.Store(true)
	if err = readEvidenceConnection(t.Context(), dialControl(t, path), claim, output.Ref, true, io.Discard, time.Second); err == nil || repo.opens != 2 {
		t.Fatal("dead peer reached provider", err)
	}
}

func TestEvidenceTransportValidatesUntrustedPayloadBeforeProviderAccess(t *testing.T) {
	for _, operation := range []string{"input-read", "artifact-read", "artifact-put"} {
		for _, mode := range []string{"unknown-field", "duplicate-field", "content", "oversized"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				claim := controlClaim()
				ref := wireCaptureRef(t)
				var payload []byte
				if operation == "artifact-put" {
					a := evidence.Artifact{Ref: ref, Data: []byte("candidate")}
					if mode == "content" {
						a.Data = []byte("corrupted")
					}
					if mode == "oversized" {
						a.Data = bytes.Repeat([]byte("x"), (1<<20)+1)
						a.Ref.Bytes = int64(len(a.Data))
						a.Ref.SHA256 = evidence.Digest(a.Data)
					}
					payload, _ = json.Marshal(a)
				} else {
					if mode == "content" {
						ref.SHA256 = "invalid"
					}
					if mode == "oversized" {
						ref.Bytes = (1 << 30) + 1
					}
					payload, _ = json.Marshal(ref)
				}
				if mode == "unknown-field" {
					payload = append(payload[:len(payload)-1], []byte(`,"unknown":true}`)...)
				}
				if mode == "duplicate-field" {
					payload = append(append(bytes.Clone(payload[:len(payload)-1]), ','), payload[1:]...)
				}
				var calls atomic.Int32
				session := &fixtureEvidenceSession{fixtureReportSession: fixtureReportSession{claim: claim},
					read: func(context.Context, evidence.Ref, bool, io.Writer) error { calls.Add(1); return nil },
					put:  func(context.Context, evidence.Artifact) (evidence.Ref, error) { calls.Add(1); return ref, nil },
				}
				path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
				conn := dialControl(t, path)
				header := reportHeader{SchemaVersion: 2, Operation: operation, Claim: claim, Bytes: len(payload), Digest: evidence.Digest(payload)}
				awaitReportReady(t, conn, header)
				if err := writeBoundedFrame(conn, payload, uploadFrameLimit(operation)); err != nil {
					t.Fatal(err)
				}
				if err := conn.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, conn)
				cancel()
				_ = awaitControllerStop(t, done)
				if calls.Load() != 0 {
					t.Fatal("unvalidated payload reached provider")
				}
			})
		}
	}
}
