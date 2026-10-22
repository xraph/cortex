package service

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func declareCapture(job *sandbox.Job) {
	job.Limits.ArtifactBytes = 1024
	job.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/result", MediaType: "text/plain", MaxBytes: 1024}}
}
func captureAuthorizationFixture(t *testing.T) (*Controller, WorkerClaim, *fixtureWorkerPeer, evidence.Ref) {
	t.Helper()
	c, receipt, _ := controllerFixture(t, declareCapture)
	c.capturePolicy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }
	c, claim, peer, _ := authorizationControllerFixture(t, c, receipt)
	return c, claim, peer, captureRefForJob(t, receipt.Job)
}

func captureRefForJob(t *testing.T, job sandbox.Job) evidence.Ref {
	t.Helper()
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: job.Outputs[0].ID, Scope: job.Scope, MediaType: job.Outputs[0].MediaType, Version: job.Digest, Producer: sandbox.ArtifactProducer, SourceURI: job.OutputSource(job.Outputs[0]), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("candidate")})
	if err != nil {
		t.Fatal(err)
	}
	return a.Ref
}

func TestCaptureControllerTransportPersistsBeforeAcknowledging(t *testing.T) {
	c, claim, peer, ref := captureAuthorizationFixture(t)
	c.capturePolicy = func(ctx context.Context, job sandbox.Job, action evidence.Action, candidate evidence.Ref) error {
		if cortex.ScopeFromContext(ctx).Canonical() != ref.Scope.Canonical() || job.Principal != "alice" || (action != evidence.ActionRead && action != evidence.ActionPut) {
			t.Fatal("capture policy lost subject")
		}
		job.Outputs[0].Path = "changed"
		candidate.Scope.Levels[0].Value = "changed"
		return nil
	}
	session := &fixtureCaptureSession{fixtureReportSession: fixtureReportSession{claim: claim}, capture: func(ctx context.Context, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
		return c.captureWorker(ctx, peer, claim, ref, confirm)
	}}
	path, cancel, done := startReportControl(t, func(ctx context.Context, _ *net.UnixConn, got WorkerClaim) (reportSession, error) {
		if _, err := c.authorizeWorker(ctx, peer, got); err != nil {
			return nil, err
		}
		return session, nil
	}, ControlLimits{Connections: 2, Reports: 1, Timeout: time.Second})
	for _, confirm := range []bool{false, true} {
		ack, err := captureConnection(t.Context(), dialControl(t, path), claim, ref, confirm, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		records, err := c.launcher.store.Captures(t.Context(), claim.JobID)
		if err != nil || len(records) != 1 || records[0].Revision != ack.Revision || records[0].State != ack.State {
			t.Fatal("ack preceded persistence", records, ack, err)
		}
	}
	if _, err := c.authorizeWorker(t.Context(), peer, claim); err != nil {
		t.Fatal("capture invalidated claim", err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestCaptureControllerRejectsRevocationAndUnboundReferences(t *testing.T) {
	for _, mode := range []string{"no-policy", "policy", "peer", "claim", "reference", "cancel-during-policy", "peer-during-policy", "grant-during-policy"} {
		t.Run(mode, func(t *testing.T) {
			c, claim, peer, ref := captureAuthorizationFixture(t)
			switch mode {
			case "no-policy":
				c.capturePolicy = nil
			case "policy":
				c.capturePolicy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return evidence.ErrDenied }
			case "peer":
				peer.dieAfterFirst.Store(true)
			case "claim":
				claim.Revision++
			case "reference":
				ref.ID = id.New(evidence.PrefixArtifact)
			default:
				c.capturePolicy = func(ctx context.Context, _ sandbox.Job, _ evidence.Action, _ evidence.Ref) error {
					switch mode {
					case "cancel-during-policy":
						_, err := c.launcher.store.Cancel(ctx, claim.JobID)
						return err
					case "peer-during-policy":
						peer.dieAfterFirst.Store(true)
					case "grant-during-policy":
						c.scanner.revalidate = func(context.Context, sandbox.Job) error { return broker.ErrDenied }
					}
					return nil
				}
			}
			if ack, err := c.captureWorker(t.Context(), peer, claim, ref, false); err == nil || !ack.JobID.IsNil() {
				t.Fatal("unauthorized capture persisted", ack, err)
			}
			records, err := c.launcher.store.Captures(t.Context(), claim.JobID)
			if err != nil || len(records) != 0 {
				t.Fatal("denied capture changed journal", records, err)
			}
		})
	}
}

func TestCaptureControllerDeniesMissingPeer(t *testing.T) {
	c, claim, _, ref := captureAuthorizationFixture(t)
	if _, err := c.CaptureWorker(t.Context(), nil, claim, ref, false); !errors.Is(err, broker.ErrDenied) {
		t.Fatal(err)
	}
}

func TestCaptureControllerShutdownCancelsPolicyBeforePersistence(t *testing.T) {
	c, receipt, _ := controllerFixture(t, declareCapture)
	entered := make(chan struct{})
	c.capturePolicy = func(ctx context.Context, _ sandbox.Job, _ evidence.Action, _ evidence.Ref) error {
		close(entered)
		<-ctx.Done()
		return nil
	}
	c, claim, peer, stop := authorizationControllerFixture(t, c, receipt)
	ref := captureRefForJob(t, receipt.Job)
	done := make(chan error, 1)
	go func() { _, err := c.captureWorker(t.Context(), peer, claim, ref, false); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("capture policy did not start")
	}
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown acknowledged capture")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not join capture")
	}
	records, err := c.launcher.store.Captures(t.Context(), claim.JobID)
	if err != nil || len(records) != 0 {
		t.Fatal("shutdown persisted capture", records, err)
	}
}
