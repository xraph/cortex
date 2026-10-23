package verification

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const (
	PrefixContract id.Prefix = "vcontract"
	PrefixReport   id.Prefix = "vreport"
)

// Kind identifies the host-registered check required by a criterion.
type Kind string

const (
	KindSchema   Kind = "schema"
	KindTest     Kind = "test"
	KindState    Kind = "state"
	KindSemantic Kind = "semantic"
	KindHuman    Kind = "human"
	KindPolicy   Kind = "policy"
)

// Parameters references a host-registered, immutable check manifest. The
// manifest defines the kind-specific schema, test bundle, state probe or rubric.
// It never carries commands or callbacks supplied by candidate content.
type Parameters struct {
	ManifestRevision string `json:"manifest_revision"`
}

// Criterion is one ordered acceptance check. Every required check must pass.
type Criterion struct {
	ID               string        `json:"id"`
	Kind             Kind          `json:"kind"`
	Required         bool          `json:"required"`
	Verifier         string        `json:"verifier"`
	VerifierRevision string        `json:"verifier_revision"`
	Parameters       Parameters    `json:"parameters"`
	MaxAge           time.Duration `json:"max_age_ns"`
	// MaxSourceAge limits source observation age; zero leaves that policy to the host.
	MaxSourceAge time.Duration `json:"max_source_age_ns"`
	MinEvidence  int           `json:"min_evidence"`
}

// Contract binds an objective and ordered criteria to a scope and revision.
// A new revision requires a new ID. Digest excludes only its own field.
type Contract struct {
	SchemaVersion int          `json:"schema_version"`
	ID            id.ID        `json:"id"`
	Scope         cortex.Scope `json:"scope"`
	Revision      string       `json:"revision"`
	Digest        string       `json:"digest"`
	Objective     string       `json:"objective"`
	Constraints   []string     `json:"constraints,omitempty"`
	Criteria      []Criterion  `json:"criteria"`
}

// SealContract detaches a host-authored contract and seals its canonical JSON.
// Criterion order is significant and is never sorted away.
func SealContract(c Contract) (Contract, error) {
	c.Scope.Levels = append([]cortex.Level(nil), c.Scope.Levels...)
	c.Constraints = append([]string(nil), c.Constraints...)
	c.Criteria = append([]Criterion(nil), c.Criteria...)
	c.Digest = ""
	if err := c.validateFields(); err != nil {
		return Contract{}, err
	}
	digest, err := recordDigest(c)
	if err != nil {
		return Contract{}, err
	}
	c.Digest = digest
	return c, nil
}

func (c Contract) validateFields() error {
	if !validText(c.Revision, c.Objective, c.Scope.Canonical()) || !validText(c.Constraints...) {
		return ErrInvalid
	}
	if c.SchemaVersion != 1 || c.ID.IsNil() || c.ID.Prefix() != PrefixContract || strings.TrimSpace(c.Revision) == "" || strings.TrimSpace(c.Objective) == "" || len(c.Criteria) == 0 || len(c.Criteria) > 256 {
		return ErrInvalid
	}
	if err := cortex.ValidateScope(c.Scope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	seen := make(map[string]bool, len(c.Criteria))
	required := false
	for _, criterion := range c.Criteria {
		if criterion.validate() != nil || seen[criterion.ID] {
			return ErrInvalid
		}
		seen[criterion.ID] = true
		required = required || criterion.Required
	}
	if !required {
		return ErrInvalid
	}
	return nil
}

func (c Criterion) validate() error {
	if !validText(c.ID, c.Verifier, c.VerifierRevision, c.Parameters.ManifestRevision) || c.MaxSourceAge < 0 {
		return ErrInvalid
	}
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Verifier) == "" || strings.TrimSpace(c.VerifierRevision) == "" || strings.TrimSpace(c.Parameters.ManifestRevision) == "" || c.MaxAge <= 0 || c.MinEvidence < 0 || c.MinEvidence > 256 {
		return ErrInvalid
	}
	switch c.Kind {
	case KindSchema, KindTest, KindState, KindSemantic, KindHuman, KindPolicy:
		return nil
	default:
		return ErrInvalid
	}
}

// Validate rejects a changed or unsupported contract rather than resealing it.
func (c Contract) Validate() error {
	if err := c.validateFields(); err != nil {
		return err
	}
	expected := c.Digest
	c.Digest = ""
	actual, err := recordDigest(c)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("%w: contract digest", ErrInvalid)
	}
	return nil
}

func recordDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(data) > 1<<20 {
		return "", fmt.Errorf("%w: record exceeds one MiB", ErrInvalid)
	}
	return evidence.Digest(data), nil
}

func validText(values ...string) bool {
	for _, value := range values {
		if !utf8.ValidString(value) {
			return false
		}
	}
	return true
}
