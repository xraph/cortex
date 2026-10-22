//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
)

const controlFrameLimit = 2048

// ControlLimits bounds private worker connections and the entire exchange,
// including request reads, authorization and response writes. Host grant
// callbacks must honor their contexts for handler shutdown to remain bounded.
type ControlLimits struct {
	Connections int
	// Reports permits up to four concurrent report, capture or content
	// exchanges and must be less than Connections. Zero disables those exchanges.
	Reports int
	Timeout time.Duration
}

func (l ControlLimits) validate() error {
	if l.Connections < 1 || l.Connections > 64 || l.Reports < 0 || l.Reports > 4 || l.Reports >= l.Connections || l.Timeout < 10*time.Millisecond || l.Timeout > 30*time.Second {
		return fmt.Errorf("cortex: invalid worker control limits")
	}
	return nil
}

// WorkerControl serves authorization, capture, content transfer and report
// receipt. It cannot launch, cancel or complete jobs. JSON frames are canonical;
// JSON and binary content chunks have explicit byte bounds.
type WorkerControl struct {
	limits      ControlLimits
	controller  *Controller
	authorize   func(context.Context, *net.UnixConn, WorkerClaim) error
	beginReport func(context.Context, *net.UnixConn, WorkerClaim) (reportSession, error)
	mu          sync.Mutex
	started     bool
	reports     chan struct{}
}

// WorkerContent binds optional content services to the controller. An omitted
// configuration leaves both capabilities unavailable on the private endpoint.
type WorkerContent struct {
	Evidence *WorkerEvidence
	Images   *WorkerImages
}

// NewWorkerControl binds the transport to kernel peer authentication and the
// controller's current grants, service configuration and durable claim checks.
// Optional content services must share the same controller. All transfer
// operations share the Reports capacity limit.
func NewWorkerControl(controller *Controller, limits ControlLimits, services ...WorkerContent) (*WorkerControl, error) {
	if runtime.GOOS != "linux" {
		return nil, errUnsupported
	}
	if controller == nil {
		return nil, fmt.Errorf("cortex: missing worker controller")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if len(services) > 1 {
		return nil, fmt.Errorf("cortex: multiple worker content configurations")
	}
	var content WorkerContent
	if len(services) == 1 {
		content = services[0]
		if (content.Evidence == nil && content.Images == nil) || (content.Evidence != nil && content.Evidence.controller != controller) || (content.Images != nil && content.Images.controller != controller) {
			return nil, fmt.Errorf("cortex: invalid worker content binding")
		}
	}
	begin := controller.openReportSession
	if len(services) == 1 {
		begin = func(ctx context.Context, conn *net.UnixConn, claim WorkerClaim) (reportSession, error) {
			session, err := controller.openReportSession(ctx, conn, claim)
			if err != nil {
				return nil, err
			}
			native, ok := session.(*workerReportSession)
			if !ok {
				return nil, errors.Join(broker.ErrDenied, session.Close())
			}
			return &workerContentSession{workerReportSession: native, content: content}, nil
		}
	}
	return &WorkerControl{controller: controller, limits: limits, beginReport: begin, authorize: func(ctx context.Context, conn *net.UnixConn, claim WorkerClaim) (err error) {
		peer, err := workerpeer.Open(ctx, conn, claim.JobID)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := peer.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}()
		_, err = controller.AuthorizeWorker(ctx, peer, claim)
		return err
	}}, nil
}

// Serve takes ownership of a host-created listener in a root-owned 0700
// directory, with a root-owned 0600 socket. It never removes or replaces a socket
// path. The host owns installation and fenced removal. Start after controller
// recovery; shutdown closes the listener, cancels and joins every exchange.
// The trusted directory owner must prevent replacement while serving.
func (s *WorkerControl) Serve(ctx context.Context, listener *net.UnixListener) error {
	return s.serveOwned(ctx, listener, nil)
}

func (s *WorkerControl) serveOwned(ctx context.Context, listener *net.UnixListener, ready func()) error {
	if listener == nil {
		return fmt.Errorf("cortex: missing worker control listener")
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	if runtime.GOOS != "linux" || s.controller == nil {
		return errUnsupported
	}
	if err := inspectControlSocket(listener.Addr().String(), 0); err != nil {
		return err
	}
	operation, finish, err := s.controller.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return s.serveWithReady(operation, listener, ready)
}

func (s *WorkerControl) serve(ctx context.Context, listener *net.UnixListener) error {
	return s.serveWithReady(ctx, listener, nil)
}

func (s *WorkerControl) serveWithReady(ctx context.Context, listener *net.UnixListener, ready func()) error {
	if listener == nil || s.authorize == nil || (s.limits.Reports > 0 && s.beginReport == nil) {
		return fmt.Errorf("cortex: incomplete worker control transport")
	}
	if err := s.limits.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return broker.ErrConflict
	}
	s.started = true
	s.reports = make(chan struct{}, s.limits.Reports)
	s.mu.Unlock()
	lifetime, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, func() { _ = listener.Close() })
	var handlers sync.WaitGroup
	defer func() { cancel(); _ = listener.Close(); stop(); handlers.Wait() }()
	if err := lifetime.Err(); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	slots := make(chan struct{}, s.limits.Connections)
	for {
		select {
		case <-lifetime.Done():
			return lifetime.Err()
		case slots <- struct{}{}:
		}
		conn, err := listener.AcceptUnix()
		if err != nil {
			<-slots
			if lifetime.Err() != nil {
				return lifetime.Err()
			}
			return err
		}
		if lifetime.Err() != nil {
			_ = conn.Close()
			<-slots
			return lifetime.Err()
		}
		handlers.Add(1)
		go func() { defer handlers.Done(); defer func() { <-slots }(); s.exchange(lifetime, conn) }()
	}
}

func (s *WorkerControl) exchange(parent context.Context, conn *net.UnixConn) {
	ctx, finish, err := controlConnection(parent, conn, s.limits.Timeout)
	if err != nil {
		return
	}
	defer finish()
	body, err := readBoundedFrame(conn, controlFrameLimit)
	if err != nil {
		return
	}
	var version struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err = json.Unmarshal(body, &version); err != nil {
		return
	}
	if version.SchemaVersion == 2 {
		var header reportHeader
		if decodeCanonical(body, &header) != nil {
			return
		}
		s.exchangeReport(ctx, conn, header, body)
		return
	}
	var claim WorkerClaim
	if decodeCanonical(body, &claim) != nil || requireControlEOF(conn) != nil || validateWorkerClaim(claim) != nil {
		return
	}
	reply := controlReply{SchemaVersion: 1, Status: "denied"}
	if err = s.authorize(ctx, conn, claim); ctx.Err() == nil {
		if err == nil || err == errWorkerActivating { //nolint:errorlint // A joined cleanup or authority failure must never permit retry.
			reply.Status = "authorized"
			if err != nil {
				reply.Status = "pending"
			}
			reply.ClaimDigest = evidence.Digest(body)
		}
	}
	// Deliberately discard internal errors, which may contain a capability or
	// private deployment data. A write failure never retries authorization.
	if writeErr := writeControlFrame(conn, reply); writeErr != nil {
		return
	}
}

type controlReply struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	ClaimDigest   string `json:"claim_digest,omitempty"`
}

// AuthorizeAt checks the private endpoint and root daemon before sending a
// claim. Success is a point-in-time decision, not a lease or completion proof.
// Use a verified launch credential, keep the socket outside guest mounts and
// call again after staging immediately before guest execution. ErrPending means
// the verified worker still has an outstanding service job. No retry occurs.
func AuthorizeAt(ctx context.Context, path string, claim WorkerClaim) error {
	if err := validateWorkerClaim(claim); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialWorkerControl(operation, path)
	if err != nil {
		return err
	}
	return authorizeConnection(operation, conn, claim, 30*time.Second)
}

func authorizeConnection(ctx context.Context, conn *net.UnixConn, claim WorkerClaim, timeout time.Duration) error {
	operation, finish, err := controlConnection(ctx, conn, timeout)
	if err != nil {
		return err
	}
	defer finish()
	if claimErr := validateWorkerClaim(claim); claimErr != nil {
		return claimErr
	}
	if writeErr := writeControlFrame(conn, claim); writeErr != nil {
		return broker.ErrDenied
	}
	if closeErr := conn.CloseWrite(); closeErr != nil {
		return broker.ErrDenied
	}
	var reply controlReply
	if _, readErr := readControlFrame(conn, &reply); readErr != nil {
		return broker.ErrDenied
	}
	if eofErr := requireControlEOF(conn); eofErr != nil {
		return broker.ErrDenied
	}
	body, err := json.Marshal(claim)
	if err != nil {
		return broker.ErrDenied
	}
	if operationErr := operation.Err(); operationErr != nil {
		return operationErr
	}
	if reply.SchemaVersion != 1 || reply.ClaimDigest != evidence.Digest(body) {
		return broker.ErrDenied
	}
	switch reply.Status {
	case "authorized":
		return nil
	case "pending":
		return broker.ErrPending
	default:
		return broker.ErrDenied
	}
}

func controlConnection(ctx context.Context, conn *net.UnixConn, timeout time.Duration) (context.Context, func(), error) {
	if conn == nil {
		return nil, nil, broker.ErrDenied
	}
	if err := (ControlLimits{Connections: 1, Timeout: timeout}).validate(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(operation, func() { _ = conn.Close() })
	finish := func() { stop(); cancel(); _ = conn.Close() }
	deadline, _ := operation.Deadline()
	if err := errors.Join(conn.SetDeadline(deadline), operation.Err()); err != nil {
		finish()
		return nil, nil, err
	}
	return operation, finish, nil
}

func readControlFrame(reader io.Reader, target any) ([]byte, error) {
	body, err := readBoundedFrame(reader, controlFrameLimit)
	if err != nil {
		return nil, err
	}
	if decodeErr := decodeCanonical(body, target); decodeErr != nil {
		return nil, decodeErr
	}
	return body, nil
}

func readBoundedFrame(reader io.Reader, limit int) ([]byte, error) {
	if limit < 1 || limit > reportFrameLimit {
		return nil, broker.ErrDenied
	}
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, broker.ErrDenied
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > reportFrameLimit || uint64(size) > uint64(limit) {
		return nil, broker.ErrDenied
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, broker.ErrDenied
	}
	return body, nil
}

func decodeCanonical(body []byte, target any) error {
	if err := json.Unmarshal(body, target); err != nil {
		return broker.ErrDenied
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(body, canonical) {
		return broker.ErrDenied
	}
	return nil
}

func writeControlFrame(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return broker.ErrDenied
	}
	return writeBoundedFrame(writer, body, controlFrameLimit)
}

func writeBoundedFrame(writer io.Writer, body []byte, limit int) error {
	size := len(body)
	if size < 1 || size > reportFrameLimit || size > limit {
		return broker.ErrDenied
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(size))
	n, err := writer.Write(header[:])
	if err != nil {
		return err
	}
	if n != len(header) {
		return io.ErrShortWrite
	}
	n, err = writer.Write(body)
	if err == nil && n != size {
		return io.ErrShortWrite
	}
	return err
}

func requireControlEOF(reader io.Reader) error {
	var extra [1]byte
	n, err := reader.Read(extra[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		return broker.ErrDenied
	}
	return nil
}
