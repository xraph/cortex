package coordination

import (
	"context"
	"encoding/json"
	"math"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/verification"
)

const (
	MaxTaskUsageJobs  = 256
	MaxTaskUsageBytes = 16 << 20
)

var ErrInvalidTaskUsage = cortex.ErrInvalidTaskUsage

// TaskUsageRequest pins ownership and the journal observation to account for.
// The store reads all verifier jobs for that run, including earlier attempts.
type TaskUsageRequest struct {
	Claim          ClaimToken            `json:"claim"`
	Seal           execution.RunCallSeal `json:"seal"`
	SnapshotDigest string                `json:"snapshot_digest"`
}

// Freeze detaches the request without authenticating evidence or granting access.
func (r TaskUsageRequest) Freeze() (TaskUsageRequest, error) {
	if r.Claim.Validate() != nil || r.Seal.Validate() != nil || !evidence.ValidDigest(r.SnapshotDigest) {
		return TaskUsageRequest{}, ErrInvalidTaskUsage
	}
	seal, _, err := r.Seal.Request.Freeze()
	if err != nil {
		return TaskUsageRequest{}, err
	}
	r.Seal.Request = seal
	return r, nil
}

// TaskUsageSnapshot retains a single transactional view of ownership, provider
// observations and every verifier job. It does not close verifier admission,
// authenticate usage, authorize settlement or guarantee future billing finality.
type TaskUsageSnapshot struct {
	SchemaVersion int                           `json:"schema_version"`
	Claim         Claim                         `json:"claim"`
	Journal       execution.RunRecoverySnapshot `json:"journal"`
	Jobs          []verification.Job            `json:"jobs,omitempty"`
	Digest        string                        `json:"digest"`
}

// SealTaskUsageSnapshot validates membership, ordering and bounds, then detaches
// the observation. Only trusted complete storage reads establish completeness.
func SealTaskUsageSnapshot(s TaskUsageSnapshot) (TaskUsageSnapshot, error) {
	if s.SchemaVersion != 1 || s.Claim.Validate() != nil || (s.Claim.State != ClaimRecovering && s.Claim.State != ClaimCompleted) || s.Journal.Validate() != nil || s.Journal.Seal.Request.RunID != s.Claim.RunID || s.Journal.Seal.Request.Scope.Canonical() != s.Claim.Scope.Canonical() || len(s.Jobs) > MaxTaskUsageJobs {
		return TaskUsageSnapshot{}, ErrInvalidTaskUsage
	}
	previous := ""
	used := 0
	for _, job := range s.Jobs {
		if job.Validate() != nil || job.Request.ID.String() <= previous || job.Request.Binding.RunID != s.Claim.RunID || job.Request.Binding.Scope.Canonical() != s.Claim.Scope.Canonical() {
			return TaskUsageSnapshot{}, ErrInvalidTaskUsage
		}
		previous = job.Request.ID.String()
		data, err := json.Marshal(job)
		if err != nil {
			return TaskUsageSnapshot{}, err
		}
		used += len(data)
		if used > MaxTaskUsageBytes {
			return TaskUsageSnapshot{}, ErrInvalidTaskUsage
		}
	}
	s.Digest = ""
	data, err := json.Marshal(s)
	if err != nil || len(data) > MaxTaskUsageBytes {
		return TaskUsageSnapshot{}, ErrInvalidTaskUsage
	}
	var frozen TaskUsageSnapshot
	if json.Unmarshal(data, &frozen) != nil {
		return TaskUsageSnapshot{}, ErrInvalidTaskUsage
	}
	frozen.Digest = evidence.Digest(data)
	return frozen, nil
}

// Validate checks the complete retained digest, not provider authenticity.
func (s TaskUsageSnapshot) Validate() error {
	frozen, err := SealTaskUsageSnapshot(s)
	if err != nil || s.Digest != frozen.Digest {
		return ErrInvalidTaskUsage
	}
	return nil
}

// TaskUsageSummary distinguishes observed lower bounds from exact retained
// usage. Known covers only the currently retained calls. OpenJobs and future job
// admission still prevent using this observation as final accounting authority.
type TaskUsageSummary struct {
	Observed      execution.Resources      `json:"observed"`
	Known         execution.KnownResources `json:"known"`
	Overflow      execution.KnownResources `json:"overflow"`
	ProviderCalls int                      `json:"provider_calls"`
	VerifierCalls int                      `json:"verifier_calls"`
	OpenJobs      int                      `json:"open_jobs"`
}

// SummarizeTaskUsage includes failed work and all verifier attempts. Missing
// receipts never become known zero; partial observations survive as lower bounds.
// Tool/effect costs and local token estimates cannot establish billable usage.
func SummarizeTaskUsage(s TaskUsageSnapshot) (TaskUsageSummary, error) {
	if s.Validate() != nil {
		return TaskUsageSummary{}, ErrInvalidTaskUsage
	}
	u := TaskUsageSummary{Known: execution.KnownTokens | execution.KnownMoney}
	add := func(tokens, money int64, known execution.KnownResources) {
		u.Known &= known
		for _, dimension := range []struct {
			amount int64
			total  *int64
			flag   execution.KnownResources
		}{{tokens, &u.Observed.Tokens, execution.KnownTokens}, {money, &u.Observed.USDMicros, execution.KnownMoney}} {
			if known&dimension.flag == 0 {
				continue
			}
			if dimension.amount > math.MaxInt64-*dimension.total {
				*dimension.total = math.MaxInt64
				u.Overflow |= dimension.flag
			} else {
				*dimension.total += dimension.amount
			}
		}
	}
	for _, entry := range s.Journal.Calls {
		if entry.Call.Request.Kind == execution.RunCallTool {
			continue
		}
		u.ProviderCalls++
		if entry.Provider == nil {
			u.Known = 0
			continue
		}
		if err := entry.Provider.ValidateObservation(entry.Call); err != nil {
			return TaskUsageSummary{}, err
		}
		usage := entry.Provider.Usage
		add(usage.Actual.Tokens, usage.Actual.USDMicros, usage.Known)
	}
	for _, job := range s.Jobs {
		if job.State == verification.JobQueued || job.State == verification.JobRunning || job.State == verification.JobReady {
			u.OpenJobs++
		}
		if job.Request.EvaluationBudget == nil {
			u.Known = 0
		}
		u.VerifierCalls += len(job.Calls)
		for _, call := range job.Calls {
			if call.Completion == nil {
				u.Known = 0
				continue
			}
			usage := call.Completion.Usage
			var known execution.KnownResources
			if usage.TokensKnown {
				known |= execution.KnownTokens
			}
			if usage.MoneyKnown {
				known |= execution.KnownMoney
			}
			add(usage.Tokens, usage.USDMicros, known)
		}
	}
	u.Known &^= u.Overflow
	return u, nil
}

// TaskUsageSnapshotStore reads complete evidence under the plan, run and journal
// guards. Hosts must authorize this read; the result grants no spending authority.
type TaskUsageSnapshotStore interface {
	SnapshotTaskUsage(context.Context, TaskUsageRequest) (TaskUsageSnapshot, error)
}
