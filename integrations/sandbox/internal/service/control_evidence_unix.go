//go:build linux || darwin

package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"net"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
)

const evidenceChunkLimit = 64 << 10

type evidenceReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	JobID         id.ID  `json:"job_id"`
	ClaimRevision uint64 `json:"claim_revision"`
	ArtifactID    id.ID  `json:"artifact_id"`
	BindingDigest string `json:"binding_digest"`
}

type evidenceSession interface {
	ReadEvidence(context.Context, evidence.Ref, bool, io.Writer) error
	PutEvidence(context.Context, evidence.Artifact) (evidence.Ref, error)
}

type workerContentSession struct {
	*workerReportSession
	content WorkerContent
}

func (s *workerContentSession) ReadEvidence(ctx context.Context, ref evidence.Ref, artifact bool, destination io.Writer) error {
	if s.content.Evidence == nil {
		return broker.ErrDenied
	}
	if artifact {
		return s.content.Evidence.ReadArtifact(ctx, s.peer, s.claim, ref, destination)
	}
	return s.content.Evidence.ReadInput(ctx, s.peer, s.claim, ref, destination)
}
func (s *workerContentSession) PutEvidence(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
	if s.content.Evidence == nil {
		return evidence.Ref{}, broker.ErrDenied
	}
	return s.content.Evidence.PutArtifact(ctx, s.peer, s.claim, a)
}

func isEvidenceRead(operation string) bool {
	return operation == "input-read" || operation == "artifact-read"
}

func supportsUpload(session reportSession, operation string) bool {
	if configured, ok := session.(interface{ supportsContent(string) bool }); ok && !configured.supportsContent(operation) {
		return false
	}
	switch operation {
	case "image-read":
		_, ok := session.(imageSession)
		return ok
	case "report":
		return true
	case "capture-intent", "capture-confirm":
		_, ok := session.(captureSession)
		return ok
	case "artifact-put", "input-read", "artifact-read":
		_, ok := session.(evidenceSession)
		return ok
	default:
		return false
	}
}

func prepareEvidenceRead(claim WorkerClaim, ref evidence.Ref, artifact bool) (reportHeader, []byte, error) {
	header, body, err := prepareCapture(claim, ref, false)
	if err != nil {
		return reportHeader{}, nil, err
	}
	maximum := int64(1 << 30)
	header.Operation = "input-read"
	if artifact {
		maximum = 1 << 20
		header.Operation = "artifact-read"
	}
	if ref.Bytes > maximum {
		return reportHeader{}, nil, evidence.ErrTooLarge
	}
	return header, body, nil
}

func prepareArtifact(claim WorkerClaim, a evidence.Artifact) (reportHeader, []byte, error) {
	if len(a.Data) > 1<<20 {
		return reportHeader{}, nil, evidence.ErrTooLarge
	}
	if _, _, err := prepareCapture(claim, a.Ref, false); err != nil {
		return reportHeader{}, nil, err
	}
	if err := a.Validate(); err != nil {
		return reportHeader{}, nil, err
	}
	body, err := json.Marshal(a)
	if err != nil || len(body) > reportFrameLimit {
		return reportHeader{}, nil, broker.ErrDenied
	}
	return reportHeader{SchemaVersion: 2, Operation: "artifact-put", Claim: claim, Bytes: len(body), Digest: evidence.Digest(body)}, body, nil
}

func newEvidenceReceipt(header reportHeader, ref evidence.Ref) (*evidenceReceipt, error) {
	digest, err := ref.BindingDigest()
	if err != nil {
		return nil, err
	}
	return &evidenceReceipt{SchemaVersion: 1, JobID: header.Claim.JobID, ClaimRevision: header.Claim.Revision, ArtifactID: ref.ID, BindingDigest: digest}, nil
}
func evidenceReceiptMatches(receipt *evidenceReceipt, header reportHeader, ref evidence.Ref) bool {
	expected, err := newEvidenceReceipt(header, ref)
	return err == nil && receipt != nil && *receipt == *expected
}

func submitArtifact(ctx context.Context, session reportSession, header reportHeader, payload []byte) (reportReply, error) {
	provider, ok := session.(evidenceSession)
	if !ok {
		return reportReply{}, broker.ErrDenied
	}
	var a evidence.Artifact
	if decodeCanonical(payload, &a) != nil {
		return reportReply{}, broker.ErrDenied
	}
	if _, _, err := prepareArtifact(header.Claim, a); err != nil {
		return reportReply{}, err
	}
	expected, err := newEvidenceReceipt(header, a.Ref)
	if err != nil {
		return reportReply{}, err
	}
	ref, err := provider.PutEvidence(ctx, a)
	if err != nil {
		return reportReply{}, err
	}
	if !evidenceReceiptMatches(expected, header, ref) {
		return reportReply{}, broker.ErrDenied
	}
	return reportReply{Status: "stored", Evidence: expected}, ctx.Err()
}

func streamEvidence(ctx context.Context, session reportSession, header reportHeader, payload []byte, destination io.Writer) (reportReply, error) {
	provider, ok := session.(evidenceSession)
	if !ok {
		return reportReply{}, broker.ErrDenied
	}
	var ref evidence.Ref
	if decodeCanonical(payload, &ref) != nil {
		return reportReply{}, broker.ErrDenied
	}
	if _, _, err := prepareEvidenceRead(header.Claim, ref, header.Operation == "artifact-read"); err != nil {
		return reportReply{}, err
	}
	writer := &evidenceChunkWriter{destination: destination, remaining: ref.Bytes, hash: sha256.New()}
	if err := provider.ReadEvidence(ctx, ref, header.Operation == "artifact-read", writer); err != nil {
		return reportReply{}, err
	}
	if writer.err != nil {
		return reportReply{}, writer.err
	}
	if writer.remaining != 0 || hex.EncodeToString(writer.hash.Sum(nil)) != ref.SHA256 {
		return reportReply{}, evidence.ErrDigestMismatch
	}
	receipt, err := newEvidenceReceipt(header, ref)
	if err != nil {
		return reportReply{}, err
	}
	return reportReply{Status: "read", Evidence: receipt}, ctx.Err()
}

type evidenceChunkWriter struct {
	destination io.Writer
	remaining   int64
	hash        hash.Hash
	err         error
}

func (w *evidenceChunkWriter) Write(body []byte) (written int, err error) {
	if w.err != nil {
		return 0, w.err
	}
	defer func() {
		if err != nil {
			w.err = err
		}
	}()
	if int64(len(body)) > w.remaining {
		return 0, evidence.ErrTooLarge
	}
	for len(body) > 0 {
		n := min(len(body), evidenceChunkLimit)
		if err := writeBoundedFrame(w.destination, body[:n], evidenceChunkLimit); err != nil {
			return written, err
		}
		if _, err := w.hash.Write(body[:n]); err != nil {
			return written, err
		}
		w.remaining -= int64(n)
		written += n
		body = body[n:]
	}
	return written, nil
}

func endEvidenceStream(destination io.Writer) error {
	n, err := destination.Write([]byte{0, 0, 0, 0})
	if err == nil && n != 4 {
		return io.ErrShortWrite
	}
	return err
}

type evidenceChunkReader struct {
	source  io.Reader
	pending []byte
	ended   bool
}

func (r *evidenceChunkReader) Read(body []byte) (int, error) {
	if len(body) == 0 {
		return 0, nil
	}
	if r.ended {
		return 0, io.EOF
	}
	if len(r.pending) == 0 {
		var prefix [4]byte
		if _, err := io.ReadFull(r.source, prefix[:]); err != nil {
			return 0, broker.ErrDenied
		}
		size := binary.BigEndian.Uint32(prefix[:])
		if size == 0 {
			r.ended = true
			return 0, io.EOF
		}
		if size > evidenceChunkLimit {
			return 0, broker.ErrDenied
		}
		r.pending = make([]byte, int(size))
		if _, err := io.ReadFull(r.source, r.pending); err != nil {
			return 0, broker.ErrDenied
		}
	}
	n := copy(body, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// ReadEvidenceAt copies a bounded input or output readback through the private
// daemon. Bytes remain provisional until a bound final receipt and EOF arrive.
// The caller must unblock destination writes on cancellation. No retry occurs.
func ReadEvidenceAt(ctx context.Context, path string, claim WorkerClaim, ref evidence.Ref, artifact bool, destination io.Writer) error {
	if _, _, err := prepareEvidenceRead(claim, ref, artifact); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialWorkerControl(operation, path)
	if err != nil {
		return err
	}
	return readEvidenceConnection(operation, conn, claim, ref, artifact, destination, 30*time.Second)
}

// PutArtifactAt sends one bounded artifact. Success acknowledges the stored
// reference only; readback and capture confirmation remain separate operations.
func PutArtifactAt(ctx context.Context, path string, claim WorkerClaim, a evidence.Artifact) (evidence.Ref, error) {
	if _, _, err := prepareArtifact(claim, a); err != nil {
		return evidence.Ref{}, err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialWorkerControl(operation, path)
	if err != nil {
		return evidence.Ref{}, err
	}
	return putArtifactConnection(operation, conn, claim, a, 30*time.Second)
}

func putArtifactConnection(ctx context.Context, conn *net.UnixConn, claim WorkerClaim, a evidence.Artifact, timeout time.Duration) (evidence.Ref, error) {
	reply, header, err := uploadConnection(ctx, conn, timeout, func() (reportHeader, []byte, error) { return prepareArtifact(claim, a) })
	if err != nil {
		return evidence.Ref{}, err
	}
	if reply.Status != "stored" || reply.Image != nil || reply.Receipt != nil || reply.Capture != nil || !evidenceReceiptMatches(reply.Evidence, header, a.Ref) {
		return evidence.Ref{}, broker.ErrDenied
	}
	return detachedCaptureRef(a.Ref), nil
}

func readEvidenceConnection(ctx context.Context, conn *net.UnixConn, claim WorkerClaim, ref evidence.Ref, artifact bool, destination io.Writer, timeout time.Duration) error {
	reply, header, _, err := downloadConnection(ctx, conn, timeout, func() (reportHeader, []byte, error) { return prepareEvidenceRead(claim, ref, artifact) }, ref.Bytes, ref.Bytes, ref.SHA256, destination)
	if err != nil {
		return err
	}
	if reply.Status != "read" || reply.Receipt != nil || reply.Capture != nil || reply.Image != nil || !evidenceReceiptMatches(reply.Evidence, header, ref) {
		return broker.ErrDenied
	}
	return nil
}

func downloadConnection(ctx context.Context, conn *net.UnixConn, timeout time.Duration, prepare func() (reportHeader, []byte, error), size, maximum int64, digest string, destination io.Writer) (reportReply, reportHeader, int64, error) {
	operation, finish, err := controlConnection(ctx, conn, timeout)
	if err != nil {
		return reportReply{}, reportHeader{}, 0, err
	}
	defer finish()
	if destination == nil {
		return reportReply{}, reportHeader{}, 0, broker.ErrDenied
	}
	header, payload, err := prepare()
	if err != nil {
		return reportReply{}, reportHeader{}, 0, err
	}
	request, err := sendUpload(operation, conn, header, payload)
	if err != nil {
		return reportReply{}, reportHeader{}, 0, err
	}
	reader := &evidenceChunkReader{source: conn}
	counter := &downloadCounter{destination: destination}
	if copyErr := verifiedio.CopyLimit(operation, reader, counter, size, maximum, digest); copyErr != nil {
		return reportReply{}, reportHeader{}, 0, copyErr
	}
	reply, err := receiveUploadReply(operation, conn, request)
	if err != nil {
		return reportReply{}, reportHeader{}, 0, err
	}
	return reply, header, counter.bytes, nil
}

type downloadCounter struct {
	destination io.Writer
	bytes       int64
}

func (w *downloadCounter) Write(body []byte) (int, error) {
	n, err := w.destination.Write(body)
	w.bytes += int64(n)
	return n, err
}
