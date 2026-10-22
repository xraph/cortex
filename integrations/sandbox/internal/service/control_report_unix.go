//go:build linux || darwin

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
	"github.com/xraph/cortex/sandbox"
)

const reportFrameLimit = 2 << 20

type reportSession interface {
	Deadline() time.Time
	Submit(context.Context, sandbox.Result) (WorkerReportReceipt, error)
	Close() error
}

type reportHeader struct {
	SchemaVersion int         `json:"schema_version"`
	Operation     string      `json:"operation"`
	Claim         WorkerClaim `json:"claim"`
	Bytes         int         `json:"bytes"`
	Digest        string      `json:"digest"`
}

type reportReply struct {
	SchemaVersion int                   `json:"schema_version"`
	Status        string                `json:"status"`
	RequestDigest string                `json:"request_digest,omitempty"`
	Receipt       *WorkerReportReceipt  `json:"receipt,omitempty"`
	Capture       *WorkerCaptureReceipt `json:"capture,omitempty"`
	Evidence      *evidenceReceipt      `json:"evidence,omitempty"`
	Image         *imageReceipt         `json:"image,omitempty"`
}

type workerReportSession struct {
	controller *Controller
	peer       *workerpeer.Peer
	claim      WorkerClaim
	deadline   time.Time
}

func (s *workerReportSession) Deadline() time.Time { return s.deadline }
func (s *workerReportSession) Close() error        { return s.peer.Close() }
func (s *workerReportSession) Submit(ctx context.Context, result sandbox.Result) (WorkerReportReceipt, error) {
	return s.controller.ProposeWorkerResult(ctx, s.peer, s.claim, result)
}

func (c *Controller) openReportSession(ctx context.Context, conn *net.UnixConn, claim WorkerClaim) (reportSession, error) {
	peer, err := workerpeer.Open(ctx, conn, claim.JobID)
	if err != nil {
		return nil, err
	}
	current, err := c.AuthorizeWorker(ctx, peer, claim)
	if err != nil {
		return nil, errors.Join(err, peer.Close())
	}
	return &workerReportSession{controller: c, peer: peer, claim: claim, deadline: acceptanceDeadline(current.Receipt)}, nil
}

func (s *WorkerControl) exchangeReport(ctx context.Context, conn *net.UnixConn, header reportHeader, body []byte) {
	deny := func() {
		if err := writeControlFrame(conn, reportReply{SchemaVersion: 2, Status: "denied"}); err != nil {
			return
		}
	}
	if header.SchemaVersion != 2 || uploadFrameLimit(header.Operation) == 0 || validateWorkerClaim(header.Claim) != nil || header.Bytes < 1 || header.Bytes > uploadFrameLimit(header.Operation) || !evidence.ValidDigest(header.Digest) || s.limits.Reports == 0 || s.beginReport == nil {
		deny()
		return
	}
	// Refuse overflow before authenticating or allocating a report buffer. One
	// accepted connection remains available outside the report upload ceiling.
	select {
	case s.reports <- struct{}{}:
		defer func() { <-s.reports }()
	default:
		if err := writeControlFrame(conn, reportReply{SchemaVersion: 2, Status: "busy", RequestDigest: evidence.Digest(body)}); err != nil {
			return
		}
		return
	}
	session, err := s.beginReport(ctx, conn, header.Claim)
	if err != nil || session == nil {
		if session != nil {
			if closeErr := session.Close(); closeErr != nil {
				return
			}
		}
		deny()
		return
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := session.Close(); closeErr != nil {
				return
			}
		}
	}()
	if !supportsUpload(session, header.Operation) {
		deny()
		return
	}
	deadline := session.Deadline()
	if deadline.IsZero() {
		deny()
		return
	}
	operation, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	stop := context.AfterFunc(operation, func() { _ = conn.Close() })
	defer stop()
	effective, _ := operation.Deadline()
	if err = errors.Join(conn.SetDeadline(effective), operation.Err()); err != nil {
		return
	}
	requestDigest := evidence.Digest(body)
	if err = writeControlFrame(conn, reportReply{SchemaVersion: 2, Status: "ready", RequestDigest: requestDigest}); err != nil {
		return
	}
	payload, err := readBoundedFrame(conn, header.Bytes)
	if err != nil || len(payload) != header.Bytes || evidence.Digest(payload) != header.Digest || requireControlEOF(conn) != nil {
		return
	}
	var reply reportReply
	if isReadStream(header.Operation) {
		reply, err = streamDownload(operation, session, header, payload, conn)
	} else {
		reply, err = submitUpload(operation, session, header, payload)
	}
	closeErr := session.Close()
	closed = true
	if err != nil || closeErr != nil || operation.Err() != nil {
		if isReadStream(header.Operation) {
			return
		}
		deny()
		return
	}
	if isReadStream(header.Operation) {
		if err = endEvidenceStream(conn); err != nil {
			return
		}
	}
	reply.SchemaVersion, reply.RequestDigest = 2, requestDigest
	if err = writeControlFrame(conn, reply); err != nil {
		return
	}
}

func reportReceiptMatches(receipt WorkerReportReceipt, header reportHeader) bool {
	return receipt.SchemaVersion == 1 && receipt.JobID == header.Claim.JobID && receipt.ClaimRevision == header.Claim.Revision && receipt.ProposalDigest == header.Digest
}

// ReportAt sends one bounded report through the verified private endpoint. The
// returned receipt acknowledges persistence only. A lost reply must be resolved
// through broker inspection, never by rerunning the job. ErrPending means upload
// capacity was refused before payload transfer; only that report may be retried
// with the same claim and original deadline. No retry occurs here.
func ReportAt(ctx context.Context, path string, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
	if err := validateWorkerClaim(claim); err != nil {
		return WorkerReportReceipt{}, err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := operation.Err(); err != nil {
		return WorkerReportReceipt{}, err
	}
	conn, err := dialWorkerControl(operation, path)
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	return reportConnection(operation, conn, claim, result, 30*time.Second)
}

func reportConnection(ctx context.Context, conn *net.UnixConn, claim WorkerClaim, result sandbox.Result, timeout time.Duration) (WorkerReportReceipt, error) {
	reply, header, err := uploadConnection(ctx, conn, timeout, func() (reportHeader, []byte, error) { return prepareReport(claim, result) })
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	if reply.Status != "reported" || reply.Evidence != nil || reply.Image != nil || reply.Capture != nil || reply.Receipt == nil || !reportReceiptMatches(*reply.Receipt, header) {
		return WorkerReportReceipt{}, broker.ErrDenied
	}
	return *reply.Receipt, nil
}

func uploadConnection(ctx context.Context, conn *net.UnixConn, timeout time.Duration, prepare func() (reportHeader, []byte, error)) (reportReply, reportHeader, error) {
	operation, finish, err := controlConnection(ctx, conn, timeout)
	if err != nil {
		return reportReply{}, reportHeader{}, err
	}
	defer finish()
	header, payload, err := prepare()
	if err != nil {
		return reportReply{}, reportHeader{}, err
	}
	requestDigest, err := sendUpload(operation, conn, header, payload)
	if err != nil {
		return reportReply{}, reportHeader{}, err
	}
	reply, err := receiveUploadReply(operation, conn, requestDigest)
	return reply, header, err
}

func sendUpload(operation context.Context, conn *net.UnixConn, header reportHeader, payload []byte) (string, error) {
	body, err := json.Marshal(header)
	if err != nil {
		return "", broker.ErrDenied
	}
	if err = writeBoundedFrame(conn, body, controlFrameLimit); err != nil {
		return "", broker.ErrDenied
	}
	requestDigest := evidence.Digest(body)
	var reply reportReply
	if _, err = readControlFrame(conn, &reply); err != nil || reply.SchemaVersion != 2 || reply.RequestDigest != requestDigest || reply.Receipt != nil || reply.Capture != nil || reply.Evidence != nil || reply.Image != nil {
		return "", broker.ErrDenied
	}
	if operationErr := operation.Err(); operationErr != nil {
		return "", operationErr
	}
	if reply.Status == "busy" {
		return "", broker.ErrPending
	}
	if reply.Status != "ready" {
		return "", broker.ErrDenied
	}
	if err = writeBoundedFrame(conn, payload, uploadFrameLimit(header.Operation)); err != nil {
		return "", broker.ErrDenied
	}
	if err = conn.CloseWrite(); err != nil {
		return "", broker.ErrDenied
	}
	return requestDigest, nil
}

func receiveUploadReply(operation context.Context, conn *net.UnixConn, requestDigest string) (reportReply, error) {
	// Decode into a new value so absent fields cannot inherit the ready reply.
	var final reportReply
	if _, err := readControlFrame(conn, &final); err != nil || requireControlEOF(conn) != nil {
		return reportReply{}, broker.ErrDenied
	}
	if operationErr := operation.Err(); operationErr != nil {
		return reportReply{}, operationErr
	}
	if final.SchemaVersion != 2 || final.RequestDigest != requestDigest {
		return reportReply{}, broker.ErrDenied
	}
	return final, nil
}

func prepareReport(claim WorkerClaim, result sandbox.Result) (reportHeader, []byte, error) {
	if validateWorkerClaim(claim) != nil || !boundedReportInput(result) {
		return reportHeader{}, nil, broker.ErrDenied
	}
	payload, err := json.Marshal(result)
	if err != nil || len(payload) > reportFrameLimit {
		return reportHeader{}, nil, broker.ErrDenied
	}
	return reportHeader{SchemaVersion: 2, Operation: "report", Claim: claim, Bytes: len(payload), Digest: evidence.Digest(payload)}, payload, nil
}

// Bound caller-owned fields before JSON encoding. Escaping can expand this
// bounded input, so the encoded length is checked independently afterwards.
func boundedReportInput(result sandbox.Result) bool {
	if !evidence.ValidDigest(result.JobDigest) || len(result.Signature) != 0 || len(result.Reason) > 4096 || len(result.Stdout) > 1<<20 || len(result.Stderr) > (1<<20)-len(result.Stdout) || len(result.Artifacts) > 256 {
		return false
	}
	switch result.State {
	case sandbox.StateExited:
		if result.ExitCode < 0 || result.ExitCode > 255 {
			return false
		}
	case sandbox.StateFailed, sandbox.StateCancelled:
	default:
		return false
	}
	remaining := reportFrameLimit - len(result.Stdout) - len(result.Stderr) - len(result.Reason)
	for _, ref := range result.Artifacts {
		if !boundedRefInput(ref, &remaining) {
			return false
		}
	}
	return true
}

func boundedRefInput(ref evidence.Ref, remaining *int) bool {
	if !evidence.ValidDigest(ref.SHA256) || len(ref.Scope.Levels) > *remaining/2 {
		return false
	}
	*remaining -= len(ref.Scope.Levels) * 2
	consume := func(value string) bool {
		if len(value) > *remaining {
			return false
		}
		*remaining -= len(value)
		return true
	}
	for _, field := range []string{ref.MediaType, ref.Version, ref.Producer, ref.SourceURI, string(ref.Sensitivity)} {
		if !consume(field) {
			return false
		}
	}
	for _, level := range ref.Scope.Levels {
		if !consume(level.Key) || !consume(level.Value) {
			return false
		}
	}
	return true
}
