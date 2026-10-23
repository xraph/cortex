package verification_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestJobRequestBoundsAndOwnership(t *testing.T) {
	contract, report, now := fixture(t)
	req := verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: contract.ID, Binding: report.Binding, Deadline: now.Add(time.Minute), MaxAttempts: 2}
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	req.Binding.Scope.Levels[0].Value = "mutated"
	_, again, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, again)
	for _, mutate := range []func(*verification.JobRequest){
		func(r *verification.JobRequest) { r.SchemaVersion = 2 },
		func(r *verification.JobRequest) { r.ID = id.New(verification.PrefixReport) },
		func(r *verification.JobRequest) { r.ExpectedRunRevision = math.MaxInt64 - 1 },
		func(r *verification.JobRequest) { r.MaxAttempts = 0 },
		func(r *verification.JobRequest) { r.MaxAttempts = 257 },
		func(r *verification.JobRequest) { r.Deadline = time.Time{} },
	} {
		changed, _, copyErr := frozen.Freeze()
		require.NoError(t, copyErr)
		mutate(&changed)
		_, _, copyErr = changed.Freeze()
		require.ErrorIs(t, copyErr, verification.ErrInvalid)
	}
}

func TestJobSnapshotIntegrityAndClaimHistory(t *testing.T) {
	contract, report, now := fixture(t)
	req := verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: contract.ID, Binding: report.Binding, Deadline: now.Add(time.Minute), MaxAttempts: 2}
	req, digest, err := req.Freeze()
	require.NoError(t, err)
	claimID := id.New(verification.PrefixJobClaim)
	job, err := verification.SealJob(verification.Job{SchemaVersion: 1, Request: req, RequestDigest: digest, RunRevision: 2, Revision: 2, Generation: 1, Attempts: 1, ClaimIDs: []id.ID{claimID}, State: verification.JobRunning, Lease: &verification.JobLease{JobID: req.ID, ClaimID: claimID, Owner: "worker", Generation: 1, ExpiresAt: now.Add(time.Second)}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	require.NoError(t, job.Validate())
	detached, err := verification.SealJob(job)
	require.NoError(t, err)
	job.ClaimIDs[0] = id.New(verification.PrefixJobClaim)
	job.Lease.Owner = "mutated"
	require.NoError(t, detached.Validate())
	require.Error(t, job.Validate())
	for _, mutate := range []func(*verification.Job){
		func(j *verification.Job) { j.ClaimIDs = nil },
		func(j *verification.Job) {
			j.ClaimIDs = append(j.ClaimIDs, j.ClaimIDs[0])
			j.Attempts = 2
			j.Generation = 2
			j.Lease.Generation = 2
		},
		func(j *verification.Job) { j.Lease.ExpiresAt = j.Request.Deadline.Add(time.Second) },
		func(j *verification.Job) { j.State = verification.JobCommitted },
		func(j *verification.Job) { j.State = verification.JobReady },
		func(j *verification.Job) { j.RunRevision++ },
		func(j *verification.Job) { j.ReportIDs = []id.ID{report.ID, report.ID} },
	} {
		changed, copyErr := verification.SealJob(detached)
		require.NoError(t, copyErr)
		mutate(&changed)
		_, copyErr = verification.SealJob(changed)
		require.ErrorIs(t, copyErr, verification.ErrInvalid)
	}
}
