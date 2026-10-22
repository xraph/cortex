package workerstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

func captureFixture(t *testing.T) (*Store, Config, Claim, evidence.Ref) {
	t.Helper()
	cfg, receipt, key := fixtureWithKey(t)
	receipt.Job.Limits.ArtifactBytes = 1024
	receipt.Capabilities.Limits.ArtifactBytes = 1024
	receipt.Job.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/result", MediaType: "text/plain", MaxBytes: 1024}}
	var err error
	receipt.Job, err = sandbox.Seal(receipt.Job)
	if err != nil {
		t.Fatal(err)
	}
	receipt = sign(t, receipt, key)
	s := openStore(t, cfg)
	if _, err = s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: receipt.Job.Outputs[0].ID, Scope: receipt.Job.Scope, MediaType: "text/plain", Version: receipt.Job.Digest, Producer: sandbox.ArtifactProducer, SourceURI: receipt.Job.OutputSource(receipt.Job.Outputs[0]), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("untrusted output")})
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg, claim, a.Ref
}

func TestCaptureIntentSurvivesRestartWithoutChangingClaim(t *testing.T) {
	s, cfg, claim, ref := captureFixture(t)
	record, err := s.RecordCapture(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision, ref)
	if err != nil || record.State != CaptureIntent || record.Revision != 1 {
		t.Fatal(record, err)
	}
	if _, err = s.VerifyClaim(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision); err != nil {
		t.Fatal("capture changed claim", err)
	}
	ref.Scope.Levels[0].Value = "changed"
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, cfg)
	records, err := reopened.Captures(t.Context(), claim.Record.Receipt.Job.ID)
	if err != nil || len(records) != 1 || records[0].Ref.Scope.Canonical() != claim.Record.Receipt.Job.Scope.Canonical() {
		t.Fatal(records, err)
	}
	records[0].Ref.Scope.Levels[0].Value = "detached"
	loaded, err := reopened.Captures(t.Context(), claim.Record.Receipt.Job.ID)
	if err != nil || loaded[0].Ref.Scope.Canonical() != claim.Record.Receipt.Job.Scope.Canonical() {
		t.Fatal("load changed durable state", err)
	}
	if _, err = reopened.Claim(t.Context(), claim.Record.Receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("capture restart reissued execution", err)
	}
}

func TestCaptureReportRequiresConfirmedMatchingIntent(t *testing.T) {
	s, _, claim, ref := captureFixture(t)
	job := claim.Record.Receipt.Job
	result := sandbox.Result{JobDigest: job.Digest, State: sandbox.StateExited, Artifacts: []evidence.Ref{ref}}
	if _, err := s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, result); err == nil {
		t.Fatal("unrecorded artifact reported")
	}
	if _, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, result); err == nil {
		t.Fatal("unconfirmed artifact reported")
	}
	verified, err := s.ConfirmCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref)
	if err != nil || verified.State != CaptureVerified || verified.Revision != 2 {
		t.Fatal(verified, err)
	}
	changed := ref
	changed.SHA256 = strings.Repeat("b", 64)
	result.Artifacts = []evidence.Ref{changed}
	if _, err = s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, result); err == nil {
		t.Fatal("substituted capture reported")
	}
	result.Artifacts = []evidence.Ref{ref}
	proposed, err := s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Finalized(t.Context(), job.ID, proposed.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureRejectsChangedRevokedExpiredAndForeignClaims(t *testing.T) {
	for _, mode := range []string{"token", "revision", "scope", "identity", "digest", "expired", "cancelled", "future-created", "expired-retention", "long-retention"} {
		t.Run(mode, func(t *testing.T) {
			s, _, claim, ref := captureFixture(t)
			job := claim.Record.Receipt.Job
			if mode == "digest" {
				if _, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "token":
				claim.Token = strings.Repeat("0", 64)
			case "revision":
				claim.Record.Revision++
			case "scope":
				ref.Scope.Levels[0].Value = "other"
			case "identity":
				ref.ID = id.New(evidence.PrefixArtifact)
			case "digest":
				ref.SHA256 = strings.Repeat("b", 64)
			case "expired":
				s.cfg.Now = func() time.Time { return claim.Record.Receipt.AcceptedAt.Add(job.Limits.WallTime) }
			case "cancelled":
				if _, err := s.Cancel(t.Context(), job.ID); err != nil {
					t.Fatal(err)
				}
			case "future-created":
				ref.CreatedAt = time.Now().Add(time.Hour)
				ref.RetainUntil = ref.CreatedAt.Add(time.Hour)
			case "expired-retention":
				ref.RetainUntil = time.Now().Add(-time.Second)
			case "long-retention":
				ref.RetainUntil = ref.CreatedAt.Add(25 * time.Hour)
			}
			if _, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err == nil {
				t.Fatal("invalid capture recorded")
			}
		})
	}
}

func TestCaptureReplayHasOneRevisionAndNoNewAuthority(t *testing.T) {
	s, _, claim, ref := captureFixture(t)
	job := claim.Record.Receipt.Job
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			record, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref)
			if err != nil || record.Revision != 1 {
				t.Errorf("record replay: %+v %v", record, err)
			}
		})
	}
	wg.Wait()
	for range 12 {
		wg.Go(func() {
			record, err := s.ConfirmCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref)
			if err != nil || record.Revision != 2 {
				t.Errorf("confirm replay: %+v %v", record, err)
			}
		})
	}
	wg.Wait()
	if _, err := s.VerifyClaim(t.Context(), job.ID, claim.Token, claim.Record.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err == nil {
		t.Fatal("revoked intent replay accepted")
	}
	if _, err := s.ConfirmCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err == nil {
		t.Fatal("revoked confirmation replay accepted")
	}
}

func TestCaptureRecoveryIncludesTerminalJobsAndPreservesTombstone(t *testing.T) {
	s, _, claim, ref := captureFixture(t)
	job := claim.Record.Receipt.Job
	intent, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DiscardCapture(t.Context(), job.ID, ref.ID, intent.Revision, "retention"); err == nil {
		t.Fatal("live worker capture discarded")
	}
	reaping, err := s.Cancel(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reaped(t.Context(), job.ID, reaping.Revision); err != nil {
		t.Fatal(err)
	}
	pending, err := s.CaptureJobs(t.Context(), "", 1)
	if err != nil || len(pending) != 1 || pending[0] != job.ID {
		t.Fatal("terminal capture lost", pending, err)
	}
	if _, err = s.DiscardCapture(t.Context(), job.ID, ref.ID, intent.Revision+1, "retention"); err == nil {
		t.Fatal("stale discard accepted")
	}
	discarded, err := s.DiscardCapture(t.Context(), job.ID, ref.ID, intent.Revision, "retention")
	if err != nil || discarded.State != CaptureDiscarded || discarded.Revision != 2 {
		t.Fatal(discarded, err)
	}
	replayed, err := s.DiscardCapture(t.Context(), job.ID, ref.ID, intent.Revision, "retention")
	if err != nil || !replayed.DiscardedAt.Equal(discarded.DiscardedAt) {
		t.Fatal("discard replay changed tombstone", err)
	}
	pending, err = s.CaptureJobs(t.Context(), "", 1)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	retained, err := s.Captures(t.Context(), job.ID)
	if err != nil || len(retained) != 1 || retained[0].Ref.SHA256 != ref.SHA256 || retained[0].State != CaptureDiscarded {
		t.Fatal("capture binding forgotten", retained, err)
	}
}

func TestCaptureOperationsHonorCancelledContext(t *testing.T) {
	s, _, claim, ref := captureFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.RecordCapture(ctx, claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision, ref); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestExpiredCaptureCannotBeReplayedProposedOrFinalized(t *testing.T) {
	for _, mode := range []string{"record", "proposal", "finalize"} {
		t.Run(mode, func(t *testing.T) {
			s, _, claim, ref := captureFixture(t)
			job := claim.Record.Receipt.Job
			ref.RetainUntil = ref.CreatedAt.Add(10 * time.Second)
			if _, err := s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ConfirmCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
				t.Fatal(err)
			}
			result := sandbox.Result{JobDigest: job.Digest, State: sandbox.StateExited, Artifacts: []evidence.Ref{ref}}
			var proposed Record
			var err error
			if mode == "finalize" {
				proposed, err = s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, result)
				if err != nil {
					t.Fatal(err)
				}
			}
			s.cfg.Now = func() time.Time { return ref.RetainUntil }
			switch mode {
			case "record":
				_, err = s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref)
			case "proposal":
				_, err = s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, result)
			case "finalize":
				_, err = s.Finalized(t.Context(), job.ID, proposed.Revision)
			}
			if err == nil {
				t.Fatal("expired capture authorized")
			}
		})
	}
}

func TestCaptureJournalRejectsMalformedSchema(t *testing.T) {
	s, cfg, _, _ := captureFixture(t)
	if _, err := s.journal.Conn.ExecContext(t.Context(), "DROP TABLE worker_captures"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.Conn.ExecContext(t.Context(), "CREATE TABLE worker_captures(job_id TEXT,artifact_id TEXT,state TEXT,revision INTEGER,body BLOB)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), cfg)
	if reopened != nil {
		_ = reopened.Close()
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("unconstrained capture schema accepted", err)
	}
}

func TestCaptureJournalUpgradesVersionTwoWithoutReclaimingWork(t *testing.T) {
	s, cfg, claim, ref := captureFixture(t)
	if _, err := s.journal.Conn.ExecContext(t.Context(), "DROP TABLE worker_captures"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.journal.Conn.ExecContext(t.Context(), "PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, cfg)
	if _, err := reopened.VerifyClaim(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision); err != nil {
		t.Fatal("migration changed current claim", err)
	}
	if _, err := reopened.RecordCapture(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Claim(t.Context(), claim.Record.Receipt.Job.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("migration renewed execution", err)
	}
}

func TestCaptureJournalRejectsCorruptBindings(t *testing.T) {
	for _, mode := range []string{"state", "revision", "body", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			s, _, claim, ref := captureFixture(t)
			if _, err := s.RecordCapture(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
				t.Fatal(err)
			}
			query := "UPDATE worker_captures SET state='verified'"
			switch mode {
			case "revision":
				query = "UPDATE worker_captures SET revision=2"
			case "body":
				query = "UPDATE worker_captures SET body='{}'"
			case "oversized":
				query = "UPDATE worker_captures SET body=zeroblob(65537)"
			}
			if _, err := s.journal.Conn.ExecContext(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Captures(t.Context(), claim.Record.Receipt.Job.ID); !errors.Is(err, ErrCorrupt) {
				t.Fatal("corrupt capture loaded", err)
			}
			if _, err := s.ConfirmCapture(t.Context(), claim.Record.Receipt.Job.ID, claim.Token, claim.Record.Revision, ref); !errors.Is(err, ErrCorrupt) {
				t.Fatal("corrupt capture confirmed", err)
			}
		})
	}
}
