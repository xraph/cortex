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
	Timeout     time.Duration
}

func (l ControlLimits) validate() error {
	if l.Connections < 1 || l.Connections > 64 || l.Timeout < 10*time.Millisecond || l.Timeout > 30*time.Second {
		return fmt.Errorf("cortex: invalid worker control limits")
	}
	return nil
}

// WorkerControl serves authorization only. It cannot launch, cancel or complete
// jobs. Each connection carries exactly one canonical, length-prefixed request.
type WorkerControl struct {
	limits     ControlLimits
	controller *Controller
	authorize  func(context.Context, *net.UnixConn, WorkerClaim) error
	mu         sync.Mutex
	started    bool
}

// NewWorkerControl binds the transport to kernel peer authentication and the
// controller's current grants, service configuration and durable claim checks.
func NewWorkerControl(controller *Controller, limits ControlLimits) (*WorkerControl, error) {
	if runtime.GOOS != "linux" {
		return nil, errUnsupported
	}
	if controller == nil {
		return nil, fmt.Errorf("cortex: missing worker controller")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &WorkerControl{controller: controller, limits: limits, authorize: func(ctx context.Context, conn *net.UnixConn, claim WorkerClaim) (err error) {
		peer, err := workerpeer.Open(ctx, conn, claim.JobID)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, peer.Close()) }()
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
	return s.serve(operation, listener)
}

func (s *WorkerControl) serve(ctx context.Context, listener *net.UnixListener) error {
	if listener == nil || s.authorize == nil {
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
	s.mu.Unlock()
	lifetime, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, func() { _ = listener.Close() })
	var handlers sync.WaitGroup
	defer func() { cancel(); _ = listener.Close(); stop(); handlers.Wait() }()
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
	ctx, finish := context.WithTimeout(parent, s.limits.Timeout)
	defer finish()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return
	}
	var claim WorkerClaim
	body, err := readControlFrame(conn, &claim)
	if err != nil || requireControlEOF(conn) != nil || validateWorkerClaim(claim) != nil {
		return
	}
	reply := controlReply{SchemaVersion: 1, Status: "denied"}
	if err = s.authorize(ctx, conn, claim); err == nil && ctx.Err() == nil {
		reply.Status = "authorized"
		reply.ClaimDigest = evidence.Digest(body)
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
// call again after staging immediately before guest execution. No retry occurs.
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
	if conn == nil {
		return broker.ErrDenied
	}
	defer conn.Close()
	if err := validateWorkerClaim(claim); err != nil {
		return err
	}
	if err := (ControlLimits{Connections: 1, Timeout: timeout}).validate(); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(operation, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := operation.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if err := operation.Err(); err != nil {
		return err
	}
	if err := writeControlFrame(conn, claim); err != nil {
		return broker.ErrDenied
	}
	if err := conn.CloseWrite(); err != nil {
		return broker.ErrDenied
	}
	var reply controlReply
	if _, err := readControlFrame(conn, &reply); err != nil {
		return broker.ErrDenied
	}
	if err := requireControlEOF(conn); err != nil {
		return broker.ErrDenied
	}
	body, err := json.Marshal(claim)
	if err != nil {
		return broker.ErrDenied
	}
	if operationErr := operation.Err(); operationErr != nil {
		return operationErr
	}
	if reply.SchemaVersion != 1 || reply.Status != "authorized" || reply.ClaimDigest != evidence.Digest(body) {
		return broker.ErrDenied
	}
	return nil
}

func readControlFrame(reader io.Reader, target any) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, broker.ErrDenied
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > controlFrameLimit {
		return nil, broker.ErrDenied
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, broker.ErrDenied
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, broker.ErrDenied
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(body, canonical) {
		return nil, broker.ErrDenied
	}
	return body, nil
}

func writeControlFrame(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return broker.ErrDenied
	}
	size := len(body)
	if size < 1 || size > controlFrameLimit {
		return broker.ErrDenied
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(size))
	frame := append(header[:], body...)
	n, err := writer.Write(frame)
	if err == nil && n != len(frame) {
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
