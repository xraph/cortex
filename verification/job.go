package verification

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

const (
	PrefixJob      id.Prefix = "vjob"
	PrefixJobClaim id.Prefix = "vclaim"
	MaxJobLease              = time.Hour
)

var (
	ErrJobNotFound = cortex.ErrVerificationJobNotFound
	ErrJobConflict = cortex.ErrVerificationJobConflict
	ErrJobExpired  = cortex.ErrVerificationJobExpired
)

// JobState separates verification work from generation and public delivery.
type JobState string

const (
	JobQueued       JobState = "queued"
	JobRunning      JobState = "running"
	JobReady        JobState = "ready"
	JobCommitted    JobState = "committed"
	JobFailed       JobState = "failed"
	JobInconclusive JobState = "inconclusive"
	JobCancelled    JobState = "cancelled"
)

// JobRequest pins the captured candidate and contract. Scheduling never invokes
// an agent or tool. A new verification attempt uses a new ID and run revision.
type JobRequest struct {
	SchemaVersion       int               `json:"schema_version"`
	ID                  id.ID             `json:"id"`
	ExpectedRunRevision int64             `json:"expected_run_revision"`
	ContractID          id.ID             `json:"contract_id"`
	Binding             Binding           `json:"binding"`
	Deadline            time.Time         `json:"deadline"`
	MaxAttempts         int               `json:"max_attempts"`
	EvaluationBudget    *EvaluationBudget `json:"evaluation_budget,omitempty"`
}

// Freeze validates and detaches an immutable job request.
func (r JobRequest) Freeze() (JobRequest, string, error) {
	if r.EvaluationBudget != nil && r.EvaluationBudget.Validate() != nil {
		return JobRequest{}, "", ErrInvalid
	}
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixJob || r.ContractID.IsNil() || r.ContractID.Prefix() != PrefixContract || r.ExpectedRunRevision <= 0 || r.ExpectedRunRevision >= math.MaxInt64-1 || r.Deadline.IsZero() || r.MaxAttempts < 1 || r.MaxAttempts > 256 {
		return JobRequest{}, "", ErrInvalid
	}
	if err := r.Binding.Validate(); err != nil {
		return JobRequest{}, "", err
	}
	digest, err := recordDigest(r)
	if err != nil {
		return JobRequest{}, "", err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return JobRequest{}, "", err
	}
	var frozen JobRequest
	if decodeErr := json.Unmarshal(data, &frozen); decodeErr != nil {
		return JobRequest{}, "", decodeErr
	}
	return frozen, digest, nil
}

// JobLease is an ownership token. Expiry comes from storage, never the worker.
// ClaimID makes a lost claim acknowledgement recoverable without another attempt.
type JobLease struct {
	JobID      id.ID     `json:"job_id"`
	ClaimID    id.ID     `json:"claim_id"`
	Owner      string    `json:"owner"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Validate checks a token's shape; storage must check live ownership and expiry.
func (l JobLease) Validate() error {
	if l.JobID.IsNil() || l.JobID.Prefix() != PrefixJob || l.ClaimID.IsNil() || l.ClaimID.Prefix() != PrefixJobClaim || !validText(l.Owner) || strings.TrimSpace(l.Owner) == "" || len(l.Owner) > 256 || l.Generation <= 0 || l.ExpiresAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

// Job persists accepted reports across worker restarts. A digest detects stored
// corruption, not authenticity; this is trusted storage metadata.
type Job struct {
	SchemaVersion int              `json:"schema_version"`
	Request       JobRequest       `json:"request"`
	RequestDigest string           `json:"request_digest"`
	RunRevision   int64            `json:"run_revision"`
	Revision      int64            `json:"revision"`
	Generation    int64            `json:"generation"`
	Attempts      int              `json:"attempts"`
	ClaimIDs      []id.ID          `json:"claim_ids,omitempty"`
	State         JobState         `json:"state"`
	Lease         *JobLease        `json:"lease,omitempty"`
	ReportIDs     []id.ID          `json:"report_ids,omitempty"`
	Calls         []EvaluationCall `json:"calls,omitempty"`
	ResultID      id.ID            `json:"result_id,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Digest        string           `json:"digest"`
}

// SealJob validates and detaches a storage-produced job snapshot.
func SealJob(j Job) (Job, error) {
	req, digest, err := j.Request.Freeze()
	if err != nil {
		return Job{}, err
	}
	if j.SchemaVersion != 1 || j.RequestDigest != digest || j.RunRevision != req.ExpectedRunRevision+1 || j.Revision < 1 || j.Generation < 0 || j.Attempts < 0 || j.Attempts > req.MaxAttempts || j.Generation != int64(j.Attempts) || len(j.ClaimIDs) != j.Attempts || j.CreatedAt.IsZero() || !req.Deadline.After(j.CreatedAt) || j.UpdatedAt.Before(j.CreatedAt) || len(j.ReportIDs) > 256 {
		return Job{}, ErrInvalid
	}
	switch j.State {
	case JobQueued:
		if j.Lease != nil || j.Attempts != 0 || len(j.ReportIDs) != 0 {
			return Job{}, ErrInvalid
		}
	case JobRunning, JobReady:
		if j.Lease == nil || (j.State == JobReady && len(j.ReportIDs) == 0) {
			return Job{}, ErrInvalid
		}
	case JobCommitted, JobFailed, JobInconclusive, JobCancelled:
		if j.Lease != nil {
			return Job{}, ErrInvalid
		}
	default:
		return Job{}, ErrInvalid
	}
	claims := make(map[id.ID]bool, len(j.ClaimIDs))
	for _, claimID := range j.ClaimIDs {
		if claimID.IsNil() || claimID.Prefix() != PrefixJobClaim || claims[claimID] {
			return Job{}, ErrInvalid
		}
		claims[claimID] = true
	}
	if j.Lease != nil {
		lease := *j.Lease
		if lease.Validate() != nil || lease.JobID != req.ID || lease.Generation != j.Generation || len(j.ClaimIDs) == 0 || lease.ClaimID != j.ClaimIDs[len(j.ClaimIDs)-1] || lease.ExpiresAt.After(req.Deadline) || !lease.ExpiresAt.After(j.UpdatedAt) {
			return Job{}, ErrInvalid
		}
		j.Lease = &lease
	}
	if j.State == JobCommitted {
		if j.ResultID.IsNil() || j.ResultID.Prefix() != PrefixResult || len(j.ReportIDs) == 0 {
			return Job{}, ErrInvalid
		}
	} else if !j.ResultID.IsNil() {
		return Job{}, ErrInvalid
	}
	seen := make(map[id.ID]bool, len(j.ReportIDs))
	for _, reportID := range j.ReportIDs {
		if reportID.IsNil() || reportID.Prefix() != PrefixReport || seen[reportID] {
			return Job{}, ErrInvalid
		}
		seen[reportID] = true
	}
	j.ClaimIDs = append([]id.ID(nil), j.ClaimIDs...)
	j.Request = req
	j.ReportIDs = append([]id.ID(nil), j.ReportIDs...)
	if validationErr := j.validateCalls(); validationErr != nil {
		return Job{}, validationErr
	}
	j.Calls = append([]EvaluationCall(nil), j.Calls...)
	for i := range j.Calls {
		j.Calls[i].CriterionIDs = append([]string(nil), j.Calls[i].CriterionIDs...)
		if j.Calls[i].Completion != nil {
			completion := *j.Calls[i].Completion
			j.Calls[i].Completion = &completion
		}
	}
	j.Digest = ""
	j.Digest, err = recordDigest(j)
	if err != nil {
		return Job{}, err
	}
	data, err := json.Marshal(j)
	if err != nil || len(data) > 1<<20 {
		return Job{}, ErrInvalid
	}
	return j, nil
}

// Validate rejects changed snapshots without resealing them.
func (j Job) Validate() error {
	sealed, err := SealJob(j)
	if err != nil {
		return err
	}
	if sealed.Digest != j.Digest {
		return ErrInvalid
	}
	return nil
}

// JobClaim requests one bounded ownership generation.
type JobClaim struct {
	JobID    id.ID
	ClaimID  id.ID
	Owner    string
	Duration time.Duration
}

// JobListRequest pages exact-scope jobs by ID, optionally restricted to a run.
type JobListRequest struct {
	RunID id.AgentRunID
	After id.ID
	Limit int
}

// JobStore persists evaluation work without rerunning generation or tools.
// These are trusted host operations. Workers must use the lease-aware report
// method and include their lease when committing a result.
type JobStore interface {
	EnqueueVerificationJob(context.Context, JobRequest) (Job, error)
	GetVerificationJob(context.Context, id.ID) (Job, error)
	ListVerificationJobs(context.Context, JobListRequest) ([]Job, error)
	ClaimVerificationJob(context.Context, JobClaim) (Job, error)
	RenewVerificationJob(context.Context, JobLease, time.Duration) (Job, error)
	SaveVerificationJobReport(context.Context, JobLease, Report) (Job, error)
	ExpireVerificationJob(context.Context, id.ID) (Job, error)
	CancelVerificationJob(context.Context, id.ID) (Job, error)
}
