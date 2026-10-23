package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func testVerificationRunDeadline(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, path := range []string{"enqueue", "capture", "intent"} {
		for _, bound := range []string{"shortened", "equal", "later", "unbounded"} {
			t.Run(path+"/"+bound, func(t *testing.T) {
				s := newStore(t)
				ctx, intent, output := captureIntentStoreFixture(t, s)
				intents := resultCapability[verification.CaptureIntentStore](t, s)
				if path == "intent" {
					require.NoError(t, intents.CreateCaptureIntent(ctx, intent))
				}
				finishCaptureIntent(ctx, t, s, intent, output)
				current, err := s.GetRun(ctx, intent.Template.Binding.RunID)
				require.NoError(t, err)
				switch bound {
				case "shortened":
					current.Budget.Deadline = intent.Template.Deadline.Add(-time.Second)
				case "equal":
					current.Budget.Deadline = intent.Template.Deadline
				case "later":
					current.Budget.Deadline = intent.Template.Deadline.Add(time.Second)
				}
				require.NoError(t, s.UpdateRun(ctx, current))
				req, err := intent.JobFor(current.Revision, output)
				require.NoError(t, err)
				jobs := resultCapability[verification.JobStore](t, s)
				artifacts := resultCapability[evidence.Storage](t, s)
				var job verification.Job
				switch path {
				case "enqueue":
					require.NoError(t, artifacts.SaveEvidence(ctx, evidence.Artifact{Ref: req.Binding.Candidate, Data: []byte(output)}))
					job, err = jobs.EnqueueVerificationJob(ctx, req)
				case "capture":
					job, err = resultCapability[verification.CaptureStore](t, s).CaptureVerificationJob(ctx, req)
				case "intent":
					job, err = intents.CaptureFromIntent(ctx, intent.ID)
				}
				if bound != "shortened" {
					require.NoError(t, err)
					require.Equal(t, verification.JobQueued, job.State)
					return
				}
				require.ErrorIs(t, err, verification.ErrJobExpired)
				require.Empty(t, job)
				saved, err := s.GetRun(ctx, current.ID)
				require.NoError(t, err)
				require.Equal(t, current.Revision, saved.Revision)
				require.Equal(t, verification.StatusUnchecked, saved.VerificationStatus.Normalized())
				_, err = jobs.GetVerificationJob(ctx, req.ID)
				require.ErrorIs(t, err, verification.ErrJobNotFound)
				if path != "enqueue" {
					_, err = artifacts.LoadEvidence(ctx, req.Binding.Candidate)
					require.ErrorIs(t, err, evidence.ErrNotFound)
				}
			})
		}
	}
}
