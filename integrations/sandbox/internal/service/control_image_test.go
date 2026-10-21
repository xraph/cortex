package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type fixtureImageSession struct {
	fixtureReportSession
	data []byte
	read func(context.Context, string, io.Writer) (int64, error)
}

func (s *fixtureImageSession) ReadImage(ctx context.Context, pin string, destination io.Writer) (int64, error) {
	if s.read != nil {
		return s.read(ctx, pin, destination)
	}
	n, err := destination.Write(s.data)
	return int64(n), errors.Join(err, ctx.Err())
}

func TestImageTransportVerifiesBoundedContentAndReceipt(t *testing.T) {
	for _, size := range []int{0, 1, 64 << 10, 150 << 10, 1 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			claim := controlClaim()
			data := bytes.Repeat([]byte("x"), size)
			session := &fixtureImageSession{fixtureReportSession: fixtureReportSession{claim: claim}, data: data}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			var got bytes.Buffer
			if err := readImageConnection(t.Context(), dialControl(t, path), claim, imagePin(data), 1<<20, &got, time.Second); err != nil || !bytes.Equal(got.Bytes(), data) {
				t.Fatal("image stream", err)
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.closes.Load() != 1 {
				t.Fatal("peer leaked")
			}
		})
	}
}

func TestImageTransportRejectsUncertainOrCorruptTransfer(t *testing.T) {
	for _, mode := range []string{"corrupt", "partial", "oversized", "wrong-size", "provider-failure", "ignored-write-error", "close", "expired"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			data := []byte("image data")
			session := &fixtureImageSession{fixtureReportSession: fixtureReportSession{claim: claim}, data: data}
			switch mode {
			case "corrupt":
				session.data = []byte("other data")
			case "partial":
				session.data = []byte("image")
			case "oversized":
				session.data = append(bytes.Clone(data), 'x')
			case "close":
				session.closeErr = errors.New("close failed")
			case "expired":
				session.deadline = time.Now().Add(-time.Second)
			default:
				session.read = func(_ context.Context, _ string, destination io.Writer) (int64, error) {
					if mode == "ignored-write-error" {
						_, _ = destination.Write(append(bytes.Clone(data), 'x'))
					}
					n, err := destination.Write(data)
					if mode == "provider-failure" {
						return 0, broker.ErrDenied
					}
					if mode == "wrong-size" {
						return int64(n + 1), err
					}
					return int64(n), err
				}
			}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			if err := readImageConnection(t.Context(), dialControl(t, path), claim, imagePin(data), int64(len(data)), io.Discard, time.Second); err == nil {
				t.Fatal("unverified image acknowledged")
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if session.closes.Load() != 1 {
				t.Fatal("peer leaked")
			}
		})
	}
}

func TestImageTransportRequiresCapabilityBeforePayload(t *testing.T) {
	claim := controlClaim()
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) {
		return &fixtureReportSession{claim: claim}, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	defer func() { cancel(); _ = awaitControllerStop(t, done) }()
	header, _, err := prepareImageRead(claim, imagePin([]byte("data")), 1024)
	if err != nil {
		t.Fatal(err)
	}
	conn := dialControl(t, path)
	if err = writeControlFrame(conn, header); err != nil {
		t.Fatal(err)
	}
	var reply reportReply
	if _, err = readControlFrame(conn, &reply); err != nil || reply.Status != "denied" {
		t.Fatal("unsupported image service admitted payload", reply, err)
	}
}

func TestContentCapabilitiesRequireConfiguredServices(t *testing.T) {
	for _, content := range []WorkerContent{{}, {Evidence: &WorkerEvidence{}}, {Images: &WorkerImages{}}, {Evidence: &WorkerEvidence{}, Images: &WorkerImages{}}} {
		session := &workerContentSession{content: content}
		for _, operation := range []string{"image-read", "input-read", "artifact-read", "artifact-put"} {
			expected := content.Evidence != nil
			if operation == "image-read" {
				expected = content.Images != nil
			}
			if supportsUpload(session, operation) != expected {
				t.Fatal("unconfigured content capability advertised", operation)
			}
		}
	}
}

func TestImageClientRejectsUnboundCompletion(t *testing.T) {
	for _, mode := range []string{"pin", "job", "revision", "size", "schema", "status", "mixed", "request", "ready-image", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			data := []byte("image data")
			pin := imagePin(data)
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
				if mode == "ready-image" {
					ready.Image = &imageReceipt{SchemaVersion: 1}
				}
				if err = writeControlFrame(conn, ready); err != nil {
					return
				}
				if _, err = readBoundedFrame(conn, controlFrameLimit); err != nil || requireControlEOF(conn) != nil {
					return
				}
				if err = writeBoundedFrame(conn, data, evidenceChunkLimit); err != nil {
					return
				}
				if err = endEvidenceStream(conn); err != nil {
					return
				}
				receipt := &imageReceipt{SchemaVersion: 1, JobID: claim.JobID, ClaimRevision: claim.Revision, Digest: pin, Bytes: int64(len(data))}
				final := reportReply{SchemaVersion: 2, Status: "image-read", RequestDigest: evidence.Digest(body), Image: receipt}
				switch mode {
				case "pin":
					receipt.Digest = imagePin([]byte("foreign"))
				case "job":
					receipt.JobID = id.New(sandbox.PrefixJob)
				case "revision":
					receipt.ClaimRevision++
				case "size":
					receipt.Bytes++
				case "schema":
					receipt.SchemaVersion++
				case "status":
					final.Status = "read"
				case "mixed":
					final.Evidence = &evidenceReceipt{SchemaVersion: 1}
				case "request":
					final.RequestDigest = evidence.Digest([]byte("foreign"))
				}
				_ = writeControlFrame(conn, final)
				if mode == "trailing" {
					_ = writeControlFrame(conn, final)
				}
			}()
			if err := readImageConnection(t.Context(), dialControl(t, path), claim, pin, 1024, io.Discard, time.Second); err == nil {
				t.Fatal("unbound image completion accepted")
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("client did not close rejected exchange")
			}
		})
	}
}

func TestImageTransportValidatesRequestBeforeProviderAccess(t *testing.T) {
	for _, mode := range []string{"url", "zero", "oversized", "unknown", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			claim := controlClaim()
			request := imageRequest{Digest: imagePin([]byte("data")), Maximum: 1024}
			switch mode {
			case "url":
				request.Digest = "https://external.invalid/image"
			case "zero":
				request.Maximum = 0
			case "oversized":
				request.Maximum = (4 << 30) + 1
			}
			payload, _ := json.Marshal(request)
			if mode == "unknown" {
				payload = append(payload[:len(payload)-1], []byte(`,"unknown":true}`)...)
			}
			if mode == "duplicate" {
				payload = append(payload[:len(payload)-1], []byte(`,"maximum":1024}`)...)
			}
			var calls atomic.Int32
			session := &fixtureImageSession{fixtureReportSession: fixtureReportSession{claim: claim}, read: func(context.Context, string, io.Writer) (int64, error) { calls.Add(1); return 0, nil }}
			path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
			conn := dialControl(t, path)
			header := reportHeader{SchemaVersion: 2, Operation: "image-read", Claim: claim, Bytes: len(payload), Digest: evidence.Digest(payload)}
			awaitReportReady(t, conn, header)
			if err := writeBoundedFrame(conn, payload, controlFrameLimit); err != nil {
				t.Fatal(err)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			response, _ := io.ReadAll(conn)
			cancel()
			_ = awaitControllerStop(t, done)
			if calls.Load() != 0 || len(response) != 0 {
				t.Fatal("invalid image request reached provider")
			}
		})
	}
}

func TestImageTransportUsesSignedManifestService(t *testing.T) {
	access, c, claim, peer, source, root, layer := imageAccessFixture(t)
	var providerMu sync.Mutex
	path, cancel, done := startReportControl(t, func(ctx context.Context, _ *net.UnixConn, got WorkerClaim) (reportSession, error) {
		current, err := c.authorizeWorker(ctx, peer, got)
		if err != nil {
			return nil, err
		}
		return &fixtureImageSession{fixtureReportSession: fixtureReportSession{claim: claim, deadline: acceptanceDeadline(current.Receipt)}, read: func(ctx context.Context, pin string, destination io.Writer) (int64, error) {
			providerMu.Lock()
			defer providerMu.Unlock()
			return access.read(ctx, peer, claim, pin, destination)
		}}, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	defer func() { cancel(); _ = awaitControllerStop(t, done) }()
	for _, pin := range []string{root, layer} {
		var got bytes.Buffer
		if err := readImageConnection(t.Context(), dialControl(t, path), claim, pin, 1<<20, &got, time.Second); err != nil || !bytes.Equal(got.Bytes(), source.blobs[pin]) {
			t.Fatal("signed image transfer", err)
		}
	}
	if err := readImageConnection(t.Context(), dialControl(t, path), claim, imagePin([]byte("foreign")), 1<<20, io.Discard, time.Second); err == nil {
		t.Fatal("unrelated blob escaped manifest check")
	}
	providerMu.Lock()
	before := source.opens
	providerMu.Unlock()
	peer.dieAfterFirst.Store(true)
	err := readImageConnection(t.Context(), dialControl(t, path), claim, layer, 1<<20, io.Discard, time.Second)
	providerMu.Lock()
	after := source.opens
	providerMu.Unlock()
	if err == nil || after != before {
		t.Fatal("dead worker reached image provider", err)
	}
}

func TestImageTransportCancelsProviderAndJoinsPeer(t *testing.T) {
	claim := controlClaim()
	stopped := make(chan struct{})
	session := &fixtureImageSession{fixtureReportSession: fixtureReportSession{claim: claim}, read: func(ctx context.Context, _ string, _ io.Writer) (int64, error) {
		defer close(stopped)
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	path, cancel, done := startReportControl(t, func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error) { return session, nil }, ControlLimits{Connections: 2, Reports: 1, Timeout: 50 * time.Millisecond})
	if err := readImageConnection(t.Context(), dialControl(t, path), claim, imagePin([]byte("image")), 1024, io.Discard, time.Second); err == nil {
		t.Fatal("timed out image was acknowledged")
	}
	cancel()
	_ = awaitControllerStop(t, done)
	select {
	case <-stopped:
	default:
		t.Fatal("provider was not joined")
	}
	if session.closes.Load() != 1 {
		t.Fatal("retained peer was not closed")
	}
}
