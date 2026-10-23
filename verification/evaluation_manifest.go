package verification

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// EvaluationManifest pins host-selected grading material to one captured check.
// Sealing is not registration, execution authorization or proof of isolation.
type EvaluationManifest struct {
	SchemaVersion      int          `json:"schema_version"`
	Revision           string       `json:"revision"`
	Binding            Binding      `json:"binding"`
	Criterion          Criterion    `json:"criterion"`
	TestBundle         evidence.Ref `json:"test_bundle"`
	FixtureDataset     evidence.Ref `json:"fixture_dataset"`
	ResultSchema       evidence.Ref `json:"result_schema"`
	SupervisorRevision string       `json:"supervisor_revision"`
	RequiredChecks     []string     `json:"required_checks"`
	StateProbes        []Source     `json:"state_probes,omitempty"`
	Digest             string       `json:"digest"`
}

// SealEvaluationManifest detaches and seals a host-selected grading manifest.
func SealEvaluationManifest(m EvaluationManifest) (EvaluationManifest, error) {
	m.Digest = ""
	if err := m.validateFields(); err != nil {
		return EvaluationManifest{}, err
	}
	digest, err := recordDigest(m)
	if err != nil {
		return EvaluationManifest{}, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return EvaluationManifest{}, ErrInvalid
	}
	var detached EvaluationManifest
	if err = json.Unmarshal(body, &detached); err != nil {
		return EvaluationManifest{}, ErrInvalid
	}
	detached.Digest = digest
	return detached, nil
}

// Validate detects unsupported or changed grading manifests.
func (m EvaluationManifest) Validate() error {
	if err := m.validateFields(); err != nil {
		return err
	}
	expected := m.Digest
	m.Digest = ""
	actual, err := recordDigest(m)
	if err != nil || actual != expected {
		return ErrInvalid
	}
	return nil
}

func (m EvaluationManifest) validateFields() error {
	if m.SchemaVersion != 1 || !evaluationName(m.Revision, 256) || m.Binding.Validate() != nil || m.Criterion.validate() != nil || m.Criterion.Kind != KindTest || m.Revision != m.Criterion.Parameters.ManifestRevision || !evidence.ValidDigest(m.SupervisorRevision) || len(m.RequiredChecks) == 0 || len(m.RequiredChecks) > 256 {
		return ErrInvalid
	}
	checks := make(map[string]bool, len(m.RequiredChecks))
	for _, name := range m.RequiredChecks {
		if !evaluationName(name, 1024) || checks[name] {
			return ErrInvalid
		}
		checks[name] = true
	}
	supporting := make(map[id.ID]string, len(m.Binding.Evidence))
	for _, ref := range m.Binding.Evidence {
		digest, err := ref.BindingDigest()
		if err != nil {
			return ErrInvalid
		}
		supporting[ref.ID] = digest
	}
	for _, ref := range []evidence.Ref{m.TestBundle, m.FixtureDataset, m.ResultSchema} {
		digest, err := ref.BindingDigest()
		if err != nil || ref.Sensitivity != evidence.SensitivityRestricted || supporting[ref.ID] != digest {
			return ErrInvalid
		}
		delete(supporting, ref.ID)
	}
	if ValidateSources(m.StateProbes) != nil {
		return ErrInvalid
	}
	for _, probe := range m.StateProbes {
		found := false
		for _, source := range m.Binding.Context.Sources {
			if probe.Equal(source) {
				found = true
				break
			}
		}
		if !found {
			return ErrInvalid
		}
	}
	return nil
}

func evaluationName(value string, limit int) bool {
	return validText(value) && strings.TrimSpace(value) == value && value != "" && len(value) <= limit && !strings.ContainsRune(value, 0)
}

// ValidateRequest binds a test request and trusted observation time to this manifest.
// The deadline must fit inside every artifact's retention interval. Callers must
// independently recheck current authority, source state and runtime qualification.
func (m EvaluationManifest) ValidateRequest(req TestRequest, now time.Time) error {
	if m.Validate() != nil || req.Criterion != m.Criterion || now.IsZero() || !req.Deadline.After(now) {
		return ErrInvalid
	}
	expected, err := m.Binding.Digest()
	if err != nil {
		return ErrInvalid
	}
	actual, err := req.Binding.Digest()
	if err != nil || actual != expected {
		return ErrInvalid
	}
	refs := append([]evidence.Ref{m.Binding.Candidate}, m.Binding.Evidence...)
	for _, ref := range refs {
		if ref.CreatedAt.After(now) || !ref.RetainUntil.After(req.Deadline) {
			return ErrInvalid
		}
	}
	for _, source := range m.Binding.Context.Sources {
		if source.RetrievedAt.After(now) || (m.Criterion.MaxSourceAge > 0 && now.Sub(source.RetrievedAt) > m.Criterion.MaxSourceAge) {
			return ErrInvalid
		}
	}
	return nil
}
