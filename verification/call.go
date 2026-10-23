package verification

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// PrefixCall identifies a durable evaluator dispatch.
const PrefixCall id.Prefix = "vcall"

var (
	ErrJobBudgetExceeded = cortex.ErrVerificationJobBudget
	ErrCallUncertain     = cortex.ErrVerificationCallUncertain
)

// EvaluationBudget caps dispatch charges within one job. Zero token allowance
// permits only calls with a zero token bound. TokenOnly explicitly disables a
// money ceiling; otherwise the host must supply a pinned pricing revision.
type EvaluationBudget struct {
	MaxCalls        int    `json:"max_calls"`
	MaxTokens       int64  `json:"max_tokens"`
	MaxUSDMicros    int64  `json:"max_usd_micros"`
	TokenOnly       bool   `json:"token_only"`
	PricingRevision string `json:"pricing_revision,omitempty"`
}

// Validate rejects missing or contradictory accounting policy.
func (b EvaluationBudget) Validate() error {
	if b.MaxCalls < 1 || b.MaxCalls > 256 || b.MaxTokens < 0 || b.MaxUSDMicros < 0 {
		return ErrInvalid
	}
	if b.TokenOnly {
		if b.MaxUSDMicros != 0 || b.PricingRevision != "" {
			return ErrInvalid
		}
	} else if b.MaxUSDMicros == 0 || !callText(b.PricingRevision, 256) {
		return ErrInvalid
	}
	return nil
}

// EvaluationDebit includes all dispatched calls, including unresolved outcomes.
type EvaluationDebit struct {
	Calls     int   `json:"calls"`
	Tokens    int64 `json:"tokens"`
	USDMicros int64 `json:"usd_micros"`
}

// Admit checks a host-computed upper bound before an external call starts.
// Actual usage above the bound is retained and prevents later overspending;
// the adapter, not this ledger, must enforce provider limits on the active call.
func (b EvaluationBudget) Admit(used EvaluationDebit, req CallRequest) error {
	if b.Validate() != nil || req.Validate() != nil || used.Calls < 0 || used.Tokens < 0 || used.USDMicros < 0 {
		return ErrInvalid
	}
	if b.TokenOnly && req.MaxUSDMicros != 0 {
		return ErrInvalid
	}
	if used.Calls >= b.MaxCalls || used.Tokens > b.MaxTokens || req.MaxTokens > b.MaxTokens-used.Tokens || (!b.TokenOnly && (used.USDMicros > b.MaxUSDMicros || req.MaxUSDMicros > b.MaxUSDMicros-used.USDMicros)) {
		return ErrJobBudgetExceeded
	}
	return nil
}

// CallRequest pins one verifier group and its conservative usage allowance.
// The job fixes its binding, pricing revision and full acceptance contract.
type CallRequest struct {
	ID               id.ID  `json:"id"`
	Verifier         string `json:"verifier"`
	VerifierRevision string `json:"verifier_revision"`
	MaxTokens        int64  `json:"max_tokens"`
	MaxUSDMicros     int64  `json:"max_usd_micros"`
}

// Validate checks dispatch identity and bounds, not host price provenance.
func (r CallRequest) Validate() error {
	if r.ID.IsNil() || r.ID.Prefix() != PrefixCall || !callText(r.Verifier, 256) || !callText(r.VerifierRevision, 256) || r.MaxTokens < 0 || r.MaxUSDMicros < 0 {
		return ErrInvalid
	}
	return nil
}

// EvaluationUsage is a host-attributed usage observation. Unknown dimensions
// retain their full dispatch allowance. Known zero is valid with a receipt.
// Receipt identifies trusted adapter evidence; model text is not a receipt.
type EvaluationUsage struct {
	Tokens      int64  `json:"tokens"`
	USDMicros   int64  `json:"usd_micros"`
	TokensKnown bool   `json:"tokens_known"`
	MoneyKnown  bool   `json:"money_known"`
	Receipt     string `json:"receipt,omitempty"`
}

func (u EvaluationUsage) validate() error {
	if u.Tokens < 0 || u.USDMicros < 0 || (!u.TokensKnown && u.Tokens != 0) || (!u.MoneyKnown && u.USDMicros != 0) || !validText(u.Receipt) || len(u.Receipt) > 4096 {
		return ErrInvalid
	}
	if (u.TokensKnown || u.MoneyKnown) && !callText(u.Receipt, 4096) {
		return ErrInvalid
	}
	return nil
}

// CallCompletion retains usage and report identity without duplicating report bytes.
type CallCompletion struct {
	CallID       id.ID           `json:"call_id"`
	Usage        EvaluationUsage `json:"usage"`
	ReportID     id.ID           `json:"report_id,omitempty"`
	ReportDigest string          `json:"report_digest,omitempty"`
	Failure      string          `json:"failure,omitempty"`
}

// CallResult supplies an authenticated evaluator result or an explicit failure.
// Only a current job owner may settle, including recovery with a retained receipt.
type CallResult struct {
	CallID  id.ID
	Usage   EvaluationUsage
	Report  *Report
	Failure string
}

// Freeze validates and detaches completion input before storage transactions.
func (r CallResult) Freeze() (CallResult, CallCompletion, error) {
	c := CallCompletion{CallID: r.CallID, Usage: r.Usage, Failure: r.Failure}
	if r.Report != nil {
		if r.Report.Validate() != nil {
			return CallResult{}, CallCompletion{}, ErrInvalid
		}
		frozen, err := SealReport(*r.Report)
		if err != nil {
			return CallResult{}, CallCompletion{}, err
		}
		r.Report = &frozen
		c.ReportID, c.ReportDigest = frozen.ID, frozen.Digest
	}
	if err := c.validate(); err != nil {
		return CallResult{}, CallCompletion{}, err
	}
	return r, c, nil
}

func (c CallCompletion) validate() error {
	if c.CallID.IsNil() || c.CallID.Prefix() != PrefixCall || c.Usage.validate() != nil {
		return ErrInvalid
	}
	if c.ReportID.IsNil() {
		if c.ReportDigest != "" || !callText(c.Failure, 1024) {
			return ErrInvalid
		}
	} else if c.ReportID.Prefix() != PrefixReport || !evidence.ValidDigest(c.ReportDigest) || c.Failure != "" {
		return ErrInvalid
	}
	return nil
}

// EvaluationCall is recorded before dispatch. No completion means the provider
// may have executed; another worker must reconcile rather than issue the call again.
type EvaluationCall struct {
	Request      CallRequest     `json:"request"`
	Generation   int64           `json:"generation"`
	CriterionIDs []string        `json:"criterion_ids"`
	StartedAt    time.Time       `json:"started_at"`
	CompletedAt  time.Time       `json:"completed_at,omitempty"`
	Completion   *CallCompletion `json:"completion,omitempty"`
}

// DebitCalls conservatively charges unresolved usage and preserves overruns.
func DebitCalls(calls []EvaluationCall) EvaluationDebit {
	used := EvaluationDebit{Calls: len(calls)}
	for _, call := range calls {
		tokens, money := call.Request.MaxTokens, call.Request.MaxUSDMicros
		if call.Completion != nil {
			if call.Completion.Usage.TokensKnown {
				tokens = call.Completion.Usage.Tokens
			}
			if call.Completion.Usage.MoneyKnown {
				money = call.Completion.Usage.USDMicros
			}
		}
		used.Tokens = addDebit(used.Tokens, tokens)
		used.USDMicros = addDebit(used.USDMicros, money)
	}
	return used
}

func addDebit(total, value int64) int64 {
	if value > math.MaxInt64-total {
		return math.MaxInt64
	}
	return total + value
}

func callText(value string, limit int) bool {
	return validText(value) && strings.TrimSpace(value) != "" && len(value) <= limit
}

func (j Job) validateCalls() error {
	budget := j.Request.EvaluationBudget
	if budget == nil {
		if len(j.Calls) != 0 {
			return ErrInvalid
		}
		return nil
	}
	if len(j.Calls) > budget.MaxCalls || (j.State == JobQueued && len(j.Calls) != 0) {
		return ErrInvalid
	}
	ids, groups, criteria, reports := make(map[id.ID]bool), make(map[[2]string]bool), make(map[string]bool), make(map[id.ID]bool)
	for _, call := range j.Calls {
		group := [2]string{call.Request.Verifier, call.Request.VerifierRevision}
		if call.Request.Validate() != nil || ids[call.Request.ID] || groups[group] || call.Generation < 1 || call.Generation > j.Generation || call.StartedAt.Before(j.CreatedAt) || call.StartedAt.After(j.UpdatedAt) || len(call.CriterionIDs) == 0 || len(call.CriterionIDs) > 256 || call.Request.MaxTokens > budget.MaxTokens || call.Request.MaxUSDMicros > budget.MaxUSDMicros {
			return ErrInvalid
		}
		ids[call.Request.ID], groups[group] = true, true
		for _, criterion := range call.CriterionIDs {
			if !callText(criterion, 4096) || criteria[criterion] {
				return ErrInvalid
			}
			criteria[criterion] = true
		}
		if call.Completion == nil {
			if !call.CompletedAt.IsZero() || j.State == JobReady || j.State == JobCommitted {
				return ErrInvalid
			}
			continue
		}
		c := call.Completion
		if c.validate() != nil || c.CallID != call.Request.ID || call.CompletedAt.Before(call.StartedAt) || call.CompletedAt.After(j.UpdatedAt) {
			return ErrInvalid
		}
		if c.Failure != "" && (j.State == JobRunning || j.State == JobReady || j.State == JobCommitted) {
			return ErrInvalid
		}
		if !c.ReportID.IsNil() {
			if reports[c.ReportID] || !slices.Contains(j.ReportIDs, c.ReportID) {
				return ErrInvalid
			}
			reports[c.ReportID] = true
		}
	}
	if len(reports) != len(j.ReportIDs) {
		return ErrInvalid
	}
	return nil
}

// CallStore atomically records dispatch and settles reports for budgeted jobs.
// A successful Begin is permission for one invocation only. Errors and repeated
// requests grant no dispatch permission, even when the prior write may have committed.
// Hosts must verify allowance and receipt provenance before using these trusted APIs.
type CallStore interface {
	BeginVerificationCall(context.Context, JobLease, CallRequest) (Job, error)
	CompleteVerificationCall(context.Context, JobLease, CallResult) (Job, error)
}
