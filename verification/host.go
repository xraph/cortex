package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Clock supplies trusted observation time. Hosts normally return time.Now;
// tests can inject a deterministic clock without trusting model timestamps.
type Clock interface{ Now() time.Time }

// BindingAuthorizer checks current principal, source and artifact grants and
// every revision in the supplied binding. A changed revision must deny access.
type BindingAuthorizer interface {
	AuthorizeVerification(context.Context, Binding) error
}

type registeredVerifier struct {
	registrations map[[3]string]Registration
	authorizer    BindingAuthorizer
	clock         Clock
}

// NewVerifier freezes typed registrations and enforces host identity and timing.
// Semantic and policy adapters may implement Verifier directly, subject to the
// same report validation and current-binding checks at the commit boundary.
func NewVerifier(registrations []Registration, authorizer BindingAuthorizer, clock Clock) (Verifier, error) {
	if len(registrations) == 0 || authorizer == nil || clock == nil {
		return nil, ErrInvalid
	}
	v := &registeredVerifier{registrations: make(map[[3]string]Registration, len(registrations)), authorizer: authorizer, clock: clock}
	for _, r := range registrations {
		key := [3]string{r.name, r.revision, r.manifest}
		if strings.TrimSpace(r.name) == "" || strings.TrimSpace(r.revision) == "" || strings.TrimSpace(r.manifest) == "" || r.check == nil {
			return nil, ErrInvalid
		}
		if _, exists := v.registrations[key]; exists {
			return nil, ErrConflict
		}
		v.registrations[key] = r
	}
	return v, nil
}

func (v *registeredVerifier) authorize(ctx context.Context, binding Binding) error {
	detached, err := cloneBinding(binding)
	if err != nil {
		return err
	}
	if err = v.authorizer.AuthorizeVerification(ctx, detached); err != nil {
		return fmt.Errorf("cortex: authorize verification: %w", err)
	}
	return ctx.Err()
}

func cloneBinding(binding Binding) (Binding, error) {
	data, err := json.Marshal(binding)
	if err != nil {
		return Binding{}, err
	}
	var detached Binding
	if err := json.Unmarshal(data, &detached); err != nil {
		return Binding{}, err
	}
	return detached, nil
}

func (v *registeredVerifier) Verify(ctx context.Context, req Request) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if err := req.Contract.Validate(); err != nil {
		return Report{}, err
	}
	if err := req.Binding.Validate(); err != nil {
		return Report{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return Report{}, err
	}
	if scope.Canonical() != req.Binding.Scope.Canonical() || scope.Canonical() != req.Contract.Scope.Canonical() || req.Binding.ContractDigest != req.Contract.Digest {
		return Report{}, ErrInvalid
	}
	// Freeze all caller-owned slices before invoking host callbacks.
	req.Contract, err = SealContract(req.Contract)
	if err != nil {
		return Report{}, err
	}
	req.Binding, err = cloneBinding(req.Binding)
	if err != nil {
		return Report{}, err
	}
	started := v.clock.Now().UTC()
	if started.IsZero() || !req.Deadline.After(started) {
		return Report{}, ErrInvalid
	}
	ctx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()
	selected := make([]Criterion, 0, len(req.Contract.Criteria))
	for _, criterion := range req.Contract.Criteria {
		if criterion.Verifier != req.Verifier || criterion.VerifierRevision != req.VerifierRevision {
			continue
		}
		registration, exists := v.registrations[[3]string{criterion.Verifier, criterion.VerifierRevision, criterion.Parameters.ManifestRevision}]
		if !exists || registration.kind != criterion.Kind {
			return Report{}, ErrInvalid
		}
		selected = append(selected, criterion)
	}
	if len(selected) == 0 {
		return Report{}, ErrInvalid
	}
	report := Report{SchemaVersion: 1, ID: id.New(PrefixReport), Binding: req.Binding, CheckedAt: started, ExpiresAt: req.Deadline}
	for _, criterion := range selected {
		if authorizeErr := v.authorize(ctx, req.Binding); authorizeErr != nil {
			return Report{}, authorizeErr
		}
		registration := v.registrations[[3]string{criterion.Verifier, criterion.VerifierRevision, criterion.Parameters.ManifestRevision}]
		detached, cloneErr := cloneBinding(req.Binding)
		if cloneErr != nil {
			return Report{}, cloneErr
		}
		result, checkErr := registration.check(ctx, CheckRequest{Criterion: criterion, Binding: detached, Deadline: req.Deadline})
		if checkErr != nil {
			return Report{}, fmt.Errorf("cortex: verify criterion %s: %w", criterion.ID, checkErr)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return Report{}, contextErr
		}
		report.Outcomes = append(report.Outcomes, Outcome{CriterionID: criterion.ID, Verifier: criterion.Verifier, VerifierRevision: criterion.VerifierRevision, Status: result.Status, Score: result.Score, EvidenceIDs: result.EvidenceIDs, Human: result.Human, Reason: result.Reason})
		expiry := started.Add(criterion.MaxAge)
		if expiry.Before(report.ExpiresAt) {
			report.ExpiresAt = expiry
		}
	}
	if authorizeErr := v.authorize(ctx, req.Binding); authorizeErr != nil {
		return Report{}, authorizeErr
	}
	finished := v.clock.Now().UTC()
	if finished.Before(started) || !report.ExpiresAt.After(finished) {
		return Report{}, ErrInvalid
	}
	sealed, err := SealReport(report)
	if err != nil {
		return Report{}, err
	}
	if CheckFreshness(sealed, req.Binding, finished) != StatusPassed {
		return Report{}, ErrInvalid
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return Report{}, contextErr
	}
	return sealed, nil
}
