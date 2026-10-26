//go:build linux || darwin

package grading

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
	"github.com/xraph/cortex/verification"
)

// EvaluationConfig installs a bounded roster and a protected local phase journal.
// Allowance is the host's conservative price bound for both exact jobs, whose
// resource ceilings are enforced by the qualified runner. Unknown money remains
// charged at this bound. The host must persist the roster across restarts.
type EvaluationConfig struct {
	Tests            verification.SandboxTestConfig
	Manifests        []verification.EvaluationManifest
	Allowance        verification.EvaluationAllowance
	JournalDirectory string
	Owner            uint32
	Interval         time.Duration
}

// EvaluationAdapter retains evaluator admissions and results by stable call ID.
// Start and Stop belong to the installed host lifecycle. Reconcile never starts
// an evaluator or submits a sandbox job.
type EvaluationAdapter struct {
	config   EvaluationConfig
	journal  evaluationJournal
	entries  []*evaluationEntry
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	lock     *os.File
	failures uint64
}
type evaluationEntry struct {
	manifest verification.EvaluationManifest
	verifier verification.Verifier
	mu       sync.Mutex
}
type evaluationAdmission struct {
	Dispatch verification.EvaluationDispatch `json:"dispatch"`
	Digest   string                          `json:"digest"`
}
type evaluationCallIdentity struct {
	Manifest string
	Digest   string
}

type evaluationOutcome struct {
	AdmissionDigest string                  `json:"admission_digest"`
	Result          verification.CallResult `json:"result"`
}

// NewEvaluationAdapter freezes registrations without starting work or creating files.
func NewEvaluationAdapter(c EvaluationConfig) (*EvaluationAdapter, error) {
	if len(c.Manifests) == 0 || len(c.Manifests) > 256 || c.Interval < 10*time.Millisecond || c.Interval > time.Minute || c.Allowance.MaxTokens != 0 || c.Allowance.MaxUSDMicros < 0 || (c.Allowance.MaxUSDMicros > 0 && !fileExportName(c.Allowance.PricingRevision, 256)) || (c.Allowance.MaxUSDMicros == 0 && c.Allowance.PricingRevision != "") {
		return nil, verification.ErrInvalid
	}
	journal, err := newEvaluationJournal(c.JournalDirectory, c.Owner)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(c.Manifests)
	if err != nil || len(body) > 16<<20 {
		return nil, verification.ErrInvalid
	}
	var manifests []verification.EvaluationManifest
	if json.Unmarshal(body, &manifests) != nil {
		return nil, verification.ErrInvalid
	}
	// The existing verifier validates and freezes every plan and runner dependency.
	a := &EvaluationAdapter{config: c, journal: journal}
	a.config.Manifests = nil
	runner := &journalRunner{Runner: c.Tests.Runner, journal: journal}
	tc := c.Tests
	tc.Runner = runner
	checker, checkErr := verification.NewSandboxTestVerifier(tc)
	if checkErr != nil {
		return nil, checkErr
	}
	seen := map[string]bool{}
	for _, m := range manifests {
		if m.Validate() != nil || seen[m.Digest] {
			return nil, verification.ErrInvalid
		}
		seen[m.Digest] = true
		planFound := false
		for _, plan := range c.Tests.Plans {
			if plan.ManifestDigest == m.Digest {
				planFound = true
			}
		}
		if !planFound {
			return nil, verification.ErrInvalid
		}
		verifier, verifyErr := verification.NewVerifier([]verification.Registration{verification.RegisterTests(m.Criterion.Verifier, m.Criterion.VerifierRevision, m.Criterion.Parameters.ManifestRevision, checker)}, tc.Authorizer, tc.Clock)
		if verifyErr != nil {
			return nil, verifyErr
		}
		a.entries = append(a.entries, &evaluationEntry{manifest: m, verifier: verifier})
	}
	if len(seen) != len(c.Tests.Plans) {
		return nil, verification.ErrInvalid
	}
	return a, nil
}

func (a *EvaluationAdapter) selectEntry(ctx context.Context, input verification.EvaluationInput) (*evaluationEntry, error) {
	if a == nil || input.JobID.IsNil() || input.JobID.Prefix() != verification.PrefixJob || input.Budget.Validate() != nil || input.Request.Contract.Validate() != nil || input.Request.Binding.Validate() != nil || input.Request.Contract.Digest != input.Request.Binding.ContractDigest {
		return nil, verification.ErrInvalid
	}
	r := input.Request
	if cortex.ScopeFromContext(ctx).Canonical() != r.Binding.Scope.Canonical() || r.Contract.Scope.Canonical() != r.Binding.Scope.Canonical() {
		return nil, verification.ErrInvalid
	}
	var selected []verification.Criterion
	for _, c := range r.Contract.Criteria {
		if c.Verifier == r.Verifier && c.VerifierRevision == r.VerifierRevision {
			selected = append(selected, c)
		}
	}
	if len(selected) != 1 || selected[0].Kind != verification.KindTest {
		return nil, verification.ErrInvalid
	}
	req := verification.TestRequest{CheckRequest: verification.CheckRequest{Criterion: selected[0], Binding: r.Binding, Deadline: r.Deadline}}
	for _, entry := range a.entries {
		if entry.manifest.ValidateRequest(req, a.config.Tests.Clock.Now()) != nil {
			continue
		}
		// Registry resolution also prevents a caller supplying another registered
		// binding under a matching verifier name.
		resolved, err := a.config.Tests.Registry.Resolve(ctx, req, a.config.Tests.Clock.Now())
		if err != nil || resolved.Digest != entry.manifest.Digest {
			return nil, verification.ErrInvalid
		}
		body, marshalErr := json.Marshal(entry.manifest.Binding)
		if marshalErr != nil {
			return nil, verification.ErrInvalid
		}
		var detached verification.Binding
		if json.Unmarshal(body, &detached) != nil {
			return nil, verification.ErrInvalid
		}
		if authErr := a.config.Tests.Authorizer.AuthorizeVerification(ctx, detached); authErr != nil {
			return nil, authErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return entry, nil
	}
	return nil, verification.ErrInvalid
}

// Estimate returns only the installed host price bound for the exact registered jobs.
func (a *EvaluationAdapter) Estimate(ctx context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
	if _, err := a.selectEntry(ctx, input); err != nil {
		return verification.EvaluationAllowance{}, err
	}
	allowance := a.config.Allowance
	if input.Budget.PricingRevision != allowance.PricingRevision || input.Budget.TokenOnly != (allowance.MaxUSDMicros == 0) || allowance.MaxUSDMicros > input.Budget.MaxUSDMicros {
		return verification.EvaluationAllowance{}, verification.ErrJobBudgetExceeded
	}
	return allowance, nil
}
func (a *EvaluationAdapter) admission(ctx context.Context, d verification.EvaluationDispatch) (*evaluationEntry, evaluationAdmission, error) {
	e, err := a.selectEntry(ctx, d.Input)
	if err != nil {
		return nil, evaluationAdmission{}, err
	}
	allowance, err := a.Estimate(ctx, d.Input)
	if err != nil {
		return nil, evaluationAdmission{}, err
	}
	call := d.Call
	if call.Request.Validate() != nil || call.Completion != nil || call.Generation < 1 || call.StartedAt.IsZero() || call.StartedAt.After(a.config.Tests.Clock.Now()) || call.Request.Verifier != d.Input.Request.Verifier || call.Request.VerifierRevision != d.Input.Request.VerifierRevision || len(call.CriterionIDs) != 1 || call.CriterionIDs[0] != e.manifest.Criterion.ID || call.Request.MaxTokens != allowance.MaxTokens || call.Request.MaxUSDMicros != allowance.MaxUSDMicros {
		return nil, evaluationAdmission{}, verification.ErrInvalid
	}
	if budgetErr := d.Input.Budget.Admit(verification.EvaluationDebit{}, call.Request); budgetErr != nil {
		return nil, evaluationAdmission{}, budgetErr
	}
	body, err := json.Marshal(d)
	if err != nil || len(body) > 512<<10 {
		return nil, evaluationAdmission{}, verification.ErrInvalid
	}
	var frozen verification.EvaluationDispatch
	if strictJSON(body, &frozen) != nil {
		return nil, evaluationAdmission{}, verification.ErrInvalid
	}
	return e, evaluationAdmission{Dispatch: frozen, Digest: evidence.Digest(body)}, nil
}

// Evaluate durably admits the original call. The explicit service advances its
// phases independently; pending admission is uncertain and keeps its full charge.
func (a *EvaluationAdapter) Evaluate(ctx context.Context, d verification.EvaluationDispatch) (verification.CallResult, error) {
	entry, admission, err := a.admission(ctx, d)
	if err != nil {
		return verification.CallResult{}, err
	}
	a.mu.Lock()
	if a.cancel == nil {
		a.mu.Unlock()
		return verification.CallResult{}, cortex.ErrVerificationUnavailable
	}
	err = a.journal.write(ctx, d.Call.Request.ID.String()+".call", evaluationCallIdentity{entry.manifest.Digest, admission.Digest})
	if err == nil {
		err = a.journal.write(ctx, entry.manifest.Digest+".admission", admission)
	}
	a.mu.Unlock()
	if err != nil {
		return verification.CallResult{}, err
	}
	return a.Reconcile(ctx, d)
}

// Reconcile observes only retained output, rechecking current authority and
// input/output evidence. It does not submit, cancel, collect or rerun evaluation.
func (a *EvaluationAdapter) Reconcile(ctx context.Context, d verification.EvaluationDispatch) (verification.CallResult, error) {
	entry, expected, err := a.admission(ctx, d)
	if err != nil {
		return verification.CallResult{}, err
	}
	if err = a.checkCallIdentity(ctx, entry.manifest.Digest, expected); err != nil {
		return verification.CallResult{}, errors.Join(verification.ErrCallUncertain, err)
	}
	var admission evaluationAdmission
	if err = a.journal.read(ctx, entry.manifest.Digest+".admission", &admission); err != nil {
		return verification.CallResult{}, errors.Join(verification.ErrCallUncertain, err)
	}
	if admission.Digest != expected.Digest {
		return verification.CallResult{}, verification.ErrConflict
	}
	var outcome evaluationOutcome
	if err = a.journal.read(ctx, entry.manifest.Digest+".outcome", &outcome); err != nil {
		return verification.CallResult{}, errors.Join(verification.ErrCallUncertain, err)
	}
	if outcome.AdmissionDigest != admission.Digest || outcome.Result.CallID != d.Call.Request.ID || outcome.Result.Report == nil {
		return verification.CallResult{}, verification.ErrInvalid
	}
	result, _, err := outcome.Result.Freeze()
	if err != nil {
		return verification.CallResult{}, err
	}
	report := *result.Report
	if verification.ValidateReportForContract(d.Input.Request.Contract, report) != nil {
		return verification.CallResult{}, verification.ErrInvalid
	}
	current, wantErr := d.Input.Request.Binding.Digest()
	bound, gotErr := report.Binding.Digest()
	if wantErr != nil || gotErr != nil || current != bound {
		return verification.CallResult{}, verification.ErrInvalid
	}
	if materialErr := a.validateMaterial(ctx, entry, d.Input, report); materialErr != nil {
		return verification.CallResult{}, materialErr
	}
	return result, nil
}
func (a *EvaluationAdapter) validateMaterial(ctx context.Context, entry *evaluationEntry, input verification.EvaluationInput, report verification.Report) error {
	refs, err := verification.ReportObservationRefs(report)
	if err != nil {
		return err
	}
	refs = append(refs, entry.manifest.Binding.Candidate)
	refs = append(refs, entry.manifest.Binding.Evidence...)
	remaining := int64(32 << 20)
	for _, ref := range refs {
		if _, err = a.selectEntry(ctx, input); err != nil {
			return err
		}
		if ref.Bytes > remaining || !freshRef(ref, a.config.Tests.Clock.Now()) {
			return verification.ErrInvalid
		}
		remaining -= ref.Bytes
		reader, openErr := a.config.Tests.Repository.Open(ctx, ref)
		if openErr != nil {
			if reader != nil {
				openErr = errors.Join(openErr, reader.Close())
			}
			return openErr
		}
		if reader == nil {
			return verification.ErrInvalid
		}
		_, readErr := readEvaluationArtifact(reader, ref)
		if readErr != nil {
			return readErr
		}
	}
	if _, err = a.selectEntry(ctx, input); err != nil {
		return err
	}
	if verification.CheckFreshness(report, input.Request.Binding, a.config.Tests.Clock.Now()) != verification.StatusPassed {
		return verification.ErrInvalid
	}
	for _, ref := range refs {
		if !freshRef(ref, a.config.Tests.Clock.Now()) {
			return verification.ErrInvalid
		}
	}
	return ctx.Err()
}

var _ verification.EvaluationAdapter = (*EvaluationAdapter)(nil)
var _ sandbox.Runner = (*journalRunner)(nil)

func (a *EvaluationAdapter) checkCallIdentity(ctx context.Context, manifest string, admission evaluationAdmission) error {
	var identity evaluationCallIdentity
	if err := a.journal.read(ctx, admission.Dispatch.Call.Request.ID.String()+".call", &identity); err != nil {
		return err
	}
	if identity.Manifest != manifest || identity.Digest != admission.Digest {
		return verification.ErrConflict
	}
	return nil
}
