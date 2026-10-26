package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// CalibrationRegistration is host-owned routing to one immutable measured
// profile. Task class and deployment pins cannot be selected by the model.
type CalibrationRegistration struct {
	AgentID  id.AgentID
	Model    string
	Binding  cognitive.CalibrationBinding
	Profile  evidence.Ref
	Producer string
}

// CalibrationAuthorizer checks the current run's authority, deployment pins,
// comparable deadline constraints and current cohort revocation/contamination.
// It runs before and after evidence reads. Returning nil attests those facts.
type CalibrationAuthorizer interface {
	AuthorizeCalibration(context.Context, engine.OperationalCalibrationRequest, CalibrationRegistration) error
}

// EvidenceCalibrationProvider hydrates and recomputes registered cohort evidence.
// It has no dynamic discovery or score-based cohort selection.
type EvidenceCalibrationProvider struct {
	repository    evidence.Repository
	authority     CalibrationAuthorizer
	registrations []CalibrationRegistration
}

var _ engine.OperationalCalibrationProvider = (*EvidenceCalibrationProvider)(nil)

// NewEvidenceCalibrationProvider freezes a bounded host roster. Registering two
// cohorts for the same runtime pins is rejected instead of choosing the best score.
func NewEvidenceCalibrationProvider(repository evidence.Repository, authority CalibrationAuthorizer, registrations []CalibrationRegistration) (*EvidenceCalibrationProvider, error) {
	if nilCalibrationPort(repository) || nilCalibrationPort(authority) || len(registrations) == 0 || len(registrations) > 256 {
		return nil, cognitive.ErrInvalidCalibration
	}
	p := &EvidenceCalibrationProvider{repository: repository, authority: authority}
	for _, r := range registrations {
		if r.AgentID.IsNil() || r.AgentID.Prefix() != id.PrefixAgent || r.Model == "" || len(r.Model) > 256 || r.Binding.Validate() != nil || r.Profile.Validate() != nil || r.Profile.Bytes > 1<<20 || r.Profile.MediaType != "application/json" || r.Producer == "" || r.Producer != r.Profile.Producer || r.Profile.Scope.Canonical() != r.Binding.Scope.Canonical() {
			return nil, cognitive.ErrInvalidCalibration
		}
		for _, old := range p.registrations {
			if sameCalibrationRuntime(old, r) {
				return nil, cognitive.ErrInvalidCalibration
			}
		}
		p.registrations = append(p.registrations, cloneCalibrationRegistration(r))
	}
	return p, nil
}

// ObserveCalibration returns measured origin only after current authorization,
// canonical artifact validation and exact reproduction of the stored statistics.
func (p *EvidenceCalibrationProvider) ObserveCalibration(ctx context.Context, request engine.OperationalCalibrationRequest) (engine.OperationalCalibrationObservation, error) {
	var zero engine.OperationalCalibrationObservation
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return zero, err
	}
	if scope.Canonical() != request.Scope.Canonical() || request.RunID.IsNil() || request.RunID.Prefix() != id.PrefixAgentRun || !evidence.ValidDigest(request.PreparedDigest) {
		return zero, evidence.ErrDenied
	}
	probe := CalibrationRegistration{AgentID: request.AgentID, Model: request.Model, Binding: cognitive.CalibrationBinding{Scope: request.Scope, ModelRevision: request.ModelRevision, PromptRevision: request.PromptDigest, PolicyRevision: request.PolicyDigest, AcceptanceDigest: request.AcceptanceDigest, Tools: request.Tools}}
	var selected *CalibrationRegistration
	for _, r := range p.registrations {
		if sameCalibrationRuntime(r, probe) {
			selected = &r
			break
		}
	}
	if selected == nil {
		return zero, nil
	}
	r := *selected
	authorize := func() error {
		detached := request
		detached.Scope.Levels = slices.Clone(request.Scope.Levels)
		detached.Tools = slices.Clone(request.Tools)
		if authErr := p.authority.AuthorizeCalibration(ctx, detached, cloneCalibrationRegistration(r)); authErr != nil {
			return authErr
		}
		return ctx.Err()
	}
	if authErr := authorize(); authErr != nil {
		return zero, authErr
	}
	profileBody, err := p.read(ctx, r.Profile)
	if err != nil {
		return zero, err
	}
	var stored CalibrationMeasurement
	if decodeCalibration(profileBody, &stored) != nil || stored.Profile.Validate() != nil || !reflect.DeepEqual(stored.Profile.Binding, r.Binding) || stored.Profile.Evidence.Producer != r.Producer {
		return zero, cognitive.ErrInvalidCalibration
	}
	source := stored.Profile.Evidence
	cohortBody, err := p.read(ctx, source)
	if err != nil {
		return zero, err
	}
	var cohort CalibrationCohort
	if decodeCalibration(cohortBody, &cohort) != nil {
		return zero, cognitive.ErrInvalidCalibration
	}
	measured, err := MeasureCalibration(cohort, source)
	if err != nil || !reflect.DeepEqual(measured, stored) {
		return zero, cognitive.ErrInvalidCalibration
	}
	now := time.Now().UTC()
	if now.Before(stored.Profile.WindowEnd) || !now.Before(stored.Profile.ExpiresAt) || r.Profile.CreatedAt.Before(source.CreatedAt) {
		return zero, cognitive.ErrInvalidCalibration
	}
	// Reopen both artifacts after hydration so deletion or revocation during
	// decoding cannot leave an apparently current observation.
	for _, ref := range []evidence.Ref{r.Profile, source} {
		if _, err = p.read(ctx, ref); err != nil {
			return zero, err
		}
	}
	if authErr := authorize(); authErr != nil {
		return zero, authErr
	}
	if !time.Now().Before(stored.Profile.ExpiresAt) || !time.Now().Before(r.Profile.RetainUntil) || !time.Now().Before(source.RetainUntil) {
		return zero, cognitive.ErrInvalidCalibration
	}
	return engine.OperationalCalibrationObservation{Binding: stored.Profile.Binding, Observation: &cognitive.CalibrationObservation{Origin: cognitive.CalibrationMeasured, ObservedAt: time.Now().UTC(), Profile: stored.Profile}}, nil
}

func (p *EvidenceCalibrationProvider) read(ctx context.Context, ref evidence.Ref) ([]byte, error) {
	if ref.Validate() != nil || ref.Bytes > 1<<20 || ref.MediaType != "application/json" || ref.CreatedAt.After(time.Now()) || !time.Now().Before(ref.RetainUntil) {
		return nil, cognitive.ErrInvalidCalibration
	}
	reader, err := p.repository.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if validateErr := (evidence.Artifact{Ref: ref, Data: body}).Validate(); validateErr != nil {
		return nil, validateErr
	}
	return body, ctx.Err()
}

func decodeCalibration(body []byte, target any) error {
	if err := json.Unmarshal(body, target); err != nil {
		return err
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, body) {
		return cognitive.ErrInvalidCalibration
	}
	return nil
}

func sameCalibrationRuntime(a, b CalibrationRegistration) bool {
	if a.AgentID != b.AgentID || a.Model != b.Model || a.Binding.Scope.Canonical() != b.Binding.Scope.Canonical() || a.Binding.ModelRevision != b.Binding.ModelRevision || a.Binding.PromptRevision != b.Binding.PromptRevision || a.Binding.PolicyRevision != b.Binding.PolicyRevision || a.Binding.AcceptanceDigest != b.Binding.AcceptanceDigest || len(a.Binding.Tools) != len(b.Binding.Tools) {
		return false
	}
	tools := map[string]string{}
	for _, t := range a.Binding.Tools {
		tools[t.Name] = t.Revision
	}
	for _, t := range b.Binding.Tools {
		if tools[t.Name] != t.Revision {
			return false
		}
		delete(tools, t.Name)
	}
	return len(tools) == 0
}

func cloneCalibrationRegistration(r CalibrationRegistration) CalibrationRegistration {
	r.Binding.Scope.Levels = slices.Clone(r.Binding.Scope.Levels)
	r.Binding.Tools = slices.Clone(r.Binding.Tools)
	r.Profile.Scope.Levels = slices.Clone(r.Profile.Scope.Levels)
	return r
}

func nilCalibrationPort(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
