//go:build linux || darwin

package workerstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/sandbox"
)

// This is an in-process test bridge. Production workers need authenticated
// daemon transport and must not open or share the daemon's database directly.
type captureBridge struct {
	store *Store
	claim Claim
	fail  string
}

func (b captureBridge) Record(ctx context.Context, job sandbox.Job, ref evidence.Ref) error {
	if job.Digest != b.claim.Record.Receipt.Job.Digest {
		return evidence.ErrDenied
	}
	_, err := b.store.RecordCapture(ctx, job.ID, b.claim.Token, b.claim.Record.Revision, ref)
	if err == nil && b.fail == "record-ack" {
		return errors.New("record acknowledgement lost")
	}
	return err
}
func (b captureBridge) Confirm(ctx context.Context, job sandbox.Job, ref evidence.Ref) error {
	if job.Digest != b.claim.Record.Receipt.Job.Digest {
		return evidence.ErrDenied
	}
	_, err := b.store.ConfirmCapture(ctx, job.ID, b.claim.Token, b.claim.Record.Revision, ref)
	if err == nil && b.fail == "confirm-ack" {
		return errors.New("confirmation acknowledgement lost")
	}
	return err
}

type capturedRepository struct {
	t        *testing.T
	bridge   captureBridge
	writes   int
	artifact evidence.Artifact
}

func (r *capturedRepository) Put(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
	r.writes++
	records, err := r.bridge.store.Captures(ctx, r.bridge.claim.Record.Receipt.Job.ID)
	if err != nil || len(records) != 1 || records[0].State != CaptureIntent || !sameCaptureRef(records[0].Ref, a.Ref) {
		r.t.Fatal("repository write preceded durable intent", records, err)
	}
	r.artifact = a
	if r.bridge.fail == "upload-ack" {
		return evidence.Ref{}, errors.New("upload acknowledgement lost")
	}
	return a.Ref, nil
}
func (r *capturedRepository) Open(context.Context, evidence.Ref) (io.ReadCloser, error) {
	data := r.artifact.Data
	if r.bridge.fail == "readback" {
		data = []byte("changed bytes")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestCapturePipelineUsesDurableBindingsAcrossAcknowledgementLoss(t *testing.T) {
	for _, failure := range []string{"none", "record-ack", "upload-ack", "confirm-ack", "readback"} {
		t.Run(failure, func(t *testing.T) {
			s, cfg, claim, _ := captureFixture(t)
			job := claim.Record.Receipt.Job
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "outputs"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "outputs", "result"), []byte("durable candidate output"), 0o600); err != nil {
				t.Fatal(err)
			}
			bridge := captureBridge{store: s, claim: claim, fail: failure}
			repository := &capturedRepository{t: t, bridge: bridge}
			ctx := cortex.WithScope(t.Context(), job.Scope)
			refs, err := capture.Collect(ctx, capture.Config{Root: root, Repository: repository, Journal: bridge, Retention: time.Hour, Check: func(ctx context.Context) error { return ctx.Err() }, Authorize: func(ctx context.Context, _ sandbox.Job) error {
				_, authErr := s.VerifyClaim(ctx, job.ID, claim.Token, claim.Record.Revision)
				return authErr
			}, Policy: func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }}, job)
			if failure == "none" {
				if err != nil || len(refs) != 1 {
					t.Fatal(refs, err)
				}
			} else if err == nil || len(refs) != 0 {
				t.Fatal("uncertain capture reported", refs, err)
			}
			wantWrites := 1
			if failure == "record-ack" {
				wantWrites = 0
			}
			if repository.writes != wantWrites {
				t.Fatal("capture retried upload", repository.writes)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStore(t, cfg)
			records, err := reopened.Captures(ctx, job.ID)
			if err != nil || len(records) != 1 || records[0].Ref.ID != job.Outputs[0].ID || records[0].Ref.SHA256 != evidence.Digest([]byte("durable candidate output")) {
				t.Fatal("binding lost on restart", records, err)
			}
			wantState := CaptureIntent
			if failure == "none" || failure == "confirm-ack" {
				wantState = CaptureVerified
			}
			if records[0].State != wantState {
				t.Fatal("uncertainty changed durable evidence", records[0])
			}
			if _, err = reopened.VerifyClaim(ctx, job.ID, claim.Token, claim.Record.Revision); err != nil {
				t.Fatal("capture modified claim", err)
			}
			if failure == "none" {
				proposed, proposalErr := reopened.Propose(ctx, job.ID, claim.Token, claim.Record.Revision, sandbox.Result{JobDigest: job.Digest, State: sandbox.StateExited, Artifacts: refs})
				if proposalErr != nil {
					t.Fatal(proposalErr)
				}
				if _, err = reopened.Finalized(ctx, job.ID, proposed.Revision); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
