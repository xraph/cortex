//go:build linux || darwin

package service

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

const captureFrameLimit = 32 << 10

// WorkerCaptureReceipt acknowledges a committed capture binding. It grants no
// upload, execution, grading or release authority and contains no claim token.
type WorkerCaptureReceipt struct {
	SchemaVersion int                      `json:"schema_version"`
	JobID         id.ID                    `json:"job_id"`
	ClaimRevision uint64                   `json:"claim_revision"`
	ArtifactID    id.ID                    `json:"artifact_id"`
	BindingDigest string                   `json:"binding_digest"`
	State         workerstore.CaptureState `json:"state"`
	Revision      uint64                   `json:"revision"`
}

type captureSession interface {
	Capture(context.Context, evidence.Ref, bool) (WorkerCaptureReceipt, error)
}

func uploadFrameLimit(operation string) int {
	switch operation {
	case "report", "artifact-put":
		return reportFrameLimit
	case "capture-intent", "capture-confirm", "input-read", "artifact-read":
		return captureFrameLimit
	default:
		return 0
	}
}

func captureReceiptMatches(receipt WorkerCaptureReceipt, header reportHeader, artifact id.ID) bool {
	if receipt.SchemaVersion != 1 || receipt.JobID != header.Claim.JobID || receipt.ClaimRevision != header.Claim.Revision || receipt.ArtifactID != artifact || receipt.BindingDigest != header.Digest {
		return false
	}
	if receipt.State == workerstore.CaptureVerified && receipt.Revision == 2 {
		return header.Operation == "capture-intent" || header.Operation == "capture-confirm"
	}
	return header.Operation == "capture-intent" && receipt.State == workerstore.CaptureIntent && receipt.Revision == 1
}

func prepareCapture(claim WorkerClaim, ref evidence.Ref, confirm bool) (reportHeader, []byte, error) {
	remaining := captureFrameLimit
	if validateWorkerClaim(claim) != nil || !boundedRefInput(ref, &remaining) || ref.Validate() != nil {
		return reportHeader{}, nil, broker.ErrDenied
	}
	payload, err := json.Marshal(ref)
	if err != nil || len(payload) > captureFrameLimit {
		return reportHeader{}, nil, broker.ErrDenied
	}
	operation := "capture-intent"
	if confirm {
		operation = "capture-confirm"
	}
	return reportHeader{SchemaVersion: 2, Operation: operation, Claim: claim, Bytes: len(payload), Digest: evidence.Digest(payload)}, payload, nil
}

// CaptureAt sends one capture intent or readback confirmation through a verified
// private endpoint. It authenticates before sending the bounded reference and
// never retries an uncertain response, including explicit capacity refusal.
func CaptureAt(ctx context.Context, path string, claim WorkerClaim, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
	if _, _, err := prepareCapture(claim, ref, confirm); err != nil {
		return WorkerCaptureReceipt{}, err
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialWorkerControl(operation, path)
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	return captureConnection(operation, conn, claim, ref, confirm, 30*time.Second)
}

func captureConnection(ctx context.Context, conn *net.UnixConn, claim WorkerClaim, ref evidence.Ref, confirm bool, timeout time.Duration) (WorkerCaptureReceipt, error) {
	reply, header, err := uploadConnection(ctx, conn, timeout, func() (reportHeader, []byte, error) { return prepareCapture(claim, ref, confirm) })
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	if reply.Status != "captured" || reply.Evidence != nil || reply.Receipt != nil || reply.Capture == nil || !captureReceiptMatches(*reply.Capture, header, ref.ID) {
		return WorkerCaptureReceipt{}, broker.ErrDenied
	}
	return *reply.Capture, nil
}

func submitUpload(ctx context.Context, session reportSession, header reportHeader, payload []byte) (reportReply, error) {
	if ctx.Err() != nil {
		return reportReply{}, ctx.Err()
	}
	if header.Operation == "artifact-put" {
		return submitArtifact(ctx, session, header, payload)
	}
	if header.Operation == "report" {
		var result sandbox.Result
		if decodeCanonical(payload, &result) != nil {
			return reportReply{}, broker.ErrDenied
		}
		receipt, err := session.Submit(ctx, result)
		if err != nil {
			return reportReply{}, err
		}
		if !reportReceiptMatches(receipt, header) {
			return reportReply{}, broker.ErrDenied
		}
		return reportReply{Status: "reported", Receipt: &receipt}, nil
	}
	if header.Operation != "capture-intent" && header.Operation != "capture-confirm" {
		return reportReply{}, broker.ErrDenied
	}
	writer, ok := session.(captureSession)
	if !ok {
		return reportReply{}, broker.ErrDenied
	}
	var ref evidence.Ref
	if decodeCanonical(payload, &ref) != nil || ref.Validate() != nil {
		return reportReply{}, broker.ErrDenied
	}
	receipt, err := writer.Capture(ctx, ref, header.Operation == "capture-confirm")
	if err != nil {
		return reportReply{}, err
	}
	if !captureReceiptMatches(receipt, header, ref.ID) {
		return reportReply{}, broker.ErrDenied
	}
	return reportReply{Status: "captured", Capture: &receipt}, nil
}

func (s *workerReportSession) Capture(ctx context.Context, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
	return s.controller.CaptureWorker(ctx, s.peer, s.claim, ref, confirm)
}
