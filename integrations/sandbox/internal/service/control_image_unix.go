//go:build linux || darwin

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
)

type imageRequest struct {
	Digest  string `json:"digest"`
	Maximum int64  `json:"maximum"`
}

type imageReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	JobID         id.ID  `json:"job_id"`
	ClaimRevision uint64 `json:"claim_revision"`
	Digest        string `json:"digest"`
	Bytes         int64  `json:"bytes"`
}

type imageSession interface {
	ReadImage(context.Context, string, io.Writer) (int64, error)
}

func prepareImageRead(claim WorkerClaim, pin string, maximum int64) (reportHeader, []byte, error) {
	if validateWorkerClaim(claim) != nil || !image.ValidDigest(pin) || maximum <= 0 || maximum > 4<<30 {
		return reportHeader{}, nil, broker.ErrDenied
	}
	payload, err := json.Marshal(imageRequest{Digest: pin, Maximum: maximum})
	if err != nil {
		return reportHeader{}, nil, err
	}
	return reportHeader{SchemaVersion: 2, Operation: "image-read", Claim: claim, Bytes: len(payload), Digest: evidence.Digest(payload)}, payload, nil
}

func isReadStream(operation string) bool {
	return isEvidenceRead(operation) || operation == "image-read"
}

func streamDownload(ctx context.Context, session reportSession, header reportHeader, payload []byte, destination io.Writer) (reportReply, error) {
	if header.Operation != "image-read" {
		return streamEvidence(ctx, session, header, payload, destination)
	}
	provider, ok := session.(imageSession)
	if !ok {
		return reportReply{}, broker.ErrDenied
	}
	var request imageRequest
	if decodeCanonical(payload, &request) != nil {
		return reportReply{}, broker.ErrDenied
	}
	if _, _, err := prepareImageRead(header.Claim, request.Digest, request.Maximum); err != nil {
		return reportReply{}, err
	}
	writer := &evidenceChunkWriter{destination: destination, remaining: request.Maximum, hash: sha256.New()}
	size, err := provider.ReadImage(ctx, request.Digest, writer)
	if err != nil {
		return reportReply{}, err
	}
	if writer.err != nil {
		return reportReply{}, writer.err
	}
	if size < 0 || size > request.Maximum || request.Maximum-writer.remaining != size || hex.EncodeToString(writer.hash.Sum(nil)) != request.Digest[7:] {
		return reportReply{}, evidence.ErrDigestMismatch
	}
	receipt := &imageReceipt{SchemaVersion: 1, JobID: header.Claim.JobID, ClaimRevision: header.Claim.Revision, Digest: request.Digest, Bytes: size}
	return reportReply{Status: "image-read", Image: receipt}, ctx.Err()
}

// ReadImageAt copies one pinned image blob through the private daemon. Its
// completion receipt binds the request, worker claim, digest and observed size.
// Destination writes must unblock on cancellation. No retry occurs.
func ReadImageAt(ctx context.Context, path string, claim WorkerClaim, pin string, maximum int64, destination io.Writer) error {
	if _, _, err := prepareImageRead(claim, pin, maximum); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialWorkerControl(operation, path)
	if err != nil {
		return err
	}
	return readImageConnection(operation, conn, claim, pin, maximum, destination, 30*time.Second)
}

func readImageConnection(ctx context.Context, conn *net.UnixConn, claim WorkerClaim, pin string, maximum int64, destination io.Writer, timeout time.Duration) error {
	digest := ""
	if image.ValidDigest(pin) {
		digest = pin[7:]
	}
	reply, header, size, err := downloadConnection(ctx, conn, timeout, func() (reportHeader, []byte, error) { return prepareImageRead(claim, pin, maximum) }, -1, maximum, digest, destination)
	if err != nil {
		return err
	}
	expected := imageReceipt{SchemaVersion: 1, JobID: header.Claim.JobID, ClaimRevision: header.Claim.Revision, Digest: pin, Bytes: size}
	if reply.Status != "image-read" || reply.Receipt != nil || reply.Capture != nil || reply.Evidence != nil || reply.Image == nil || *reply.Image != expected {
		return broker.ErrDenied
	}
	return nil
}

func (s *workerContentSession) ReadImage(ctx context.Context, pin string, destination io.Writer) (int64, error) {
	if s.content.Images == nil {
		return 0, broker.ErrDenied
	}
	return s.content.Images.Read(ctx, s.peer, s.claim, pin, destination)
}
func (s *workerContentSession) supportsContent(operation string) bool {
	switch operation {
	case "image-read":
		return s.content.Images != nil
	case "input-read", "artifact-read", "artifact-put":
		return s.content.Evidence != nil
	default:
		return true
	}
}
