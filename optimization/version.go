package optimization

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
	"github.com/xraph/cortex/run"
)

// HarnessKind distinguishes execution inputs from the improvement controller.
type HarnessKind string

const (
	HarnessExecution   HarnessKind = "execution"
	HarnessImprovement HarnessKind = "improvement"
)

// HarnessVersion contains immutable execution content and separate display metadata.
// Persisted digests are checked before deriving or executing another version.
type HarnessVersion struct {
	ID             id.ID                 `json:"id"`
	HarnessID      id.ID                 `json:"harness_id"`
	ParentID       id.ID                 `json:"parent_id,omitempty"`
	SchemaVersion  int                   `json:"schema_version"`
	Kind           HarnessKind           `json:"kind"`
	Scope          cortex.Scope          `json:"scope"`
	AgentID        id.AgentID            `json:"agent_id"`
	Prepared       *engine.PreparedAgent `json:"prepared,omitempty"`
	Controller     *ControllerSpec       `json:"controller,omitempty"`
	RuntimeVersion string                `json:"runtime_version,omitempty"`
	Policy         EditPolicy            `json:"policy"`
	Sources        []run.Revision        `json:"sources"`
	Digest         string                `json:"digest"`
	Name           string                `json:"name,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	CreatedBy      string                `json:"created_by"`
}

// CanonicalVersion encodes behavior, policy and provenance without row identities,
// timestamps or display names. The prepared digest binds its original wire bytes.
func CanonicalVersion(version HarnessVersion) ([]byte, error) {
	if version.SchemaVersion != 1 {
		return nil, fmt.Errorf("%w: unsupported version schema", ErrInvalidSpec)
	}
	scope, err := ScopeKey(version.Scope)
	if err != nil {
		return nil, err
	}
	if _, err = ParseID(version.AgentID.String(), id.PrefixAgent); err != nil {
		return nil, err
	}
	if validationErr := version.Policy.validate(); validationErr != nil {
		return nil, validationErr
	}
	var preparedDigest string
	switch version.Kind {
	case HarnessExecution:
		if version.Prepared == nil || version.Controller != nil || version.RuntimeVersion == "" {
			return nil, fmt.Errorf("%w: execution version requires prepared runtime", ErrInvalidSpec)
		}
		view, inspectErr := version.Prepared.Inspect()
		if inspectErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrStaleVersion, inspectErr)
		}
		preparedScope, scopeErr := ScopeKey(view.Scope)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if view.AgentID != version.AgentID || preparedScope != scope {
			return nil, fmt.Errorf("%w: prepared identity mismatch", ErrInvalidSpec)
		}
		seen := map[string]bool{}
		for _, section := range view.Sections {
			if section.ID == "" || seen[section.ID] {
				return nil, fmt.Errorf("%w: duplicate or empty section ID", ErrInvalidSpec)
			}
			seen[section.ID] = true
		}
		preparedDigest = version.Prepared.Digest()
	case HarnessImprovement:
		if version.Controller == nil || version.Prepared != nil {
			return nil, fmt.Errorf("%w: improvement version requires controller", ErrInvalidSpec)
		}
		controller := version.Controller
		if controller.Tools == nil || controller.Model == "" || !finite(controller.Temperature) || controller.Temperature < 0 || controller.Temperature > 2 {
			return nil, fmt.Errorf("%w: invalid controller model or sampling", ErrInvalidSpec)
		}
		seen := map[string]bool{}
		for _, section := range controller.Sections {
			if section.ID == "" || seen[section.ID] {
				return nil, fmt.Errorf("%w: duplicate or empty section ID", ErrInvalidSpec)
			}
			seen[section.ID] = true
		}
		if validationErr := uniqueNames(controller.Tools); validationErr != nil {
			return nil, validationErr
		}
	default:
		return nil, fmt.Errorf("%w: invalid harness kind", ErrInvalidSpec)
	}
	content := struct {
		SchemaVersion  int             `json:"schema_version"`
		Kind           HarnessKind     `json:"kind"`
		Scope          string          `json:"scope"`
		AgentID        id.AgentID      `json:"agent_id"`
		PreparedDigest string          `json:"prepared_digest"`
		Controller     *ControllerSpec `json:"controller,omitempty"`
		RuntimeVersion string          `json:"runtime_version"`
		Policy         EditPolicy      `json:"policy"`
		Sources        []run.Revision  `json:"sources"`
	}{version.SchemaVersion, version.Kind, scope, version.AgentID, preparedDigest, version.Controller, version.RuntimeVersion, version.Policy, version.Sources}
	data, err := canonical.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return data, nil
}

// SealVersion validates and detaches all mutable content before assigning a digest.
func SealVersion(version HarnessVersion) (HarnessVersion, error) {
	data, err := CanonicalVersion(version)
	if err != nil {
		return HarnessVersion{}, err
	}
	raw, err := json.Marshal(version)
	if err != nil {
		return HarnessVersion{}, err
	}
	var detached HarnessVersion
	if validationErr := canonical.Decode(raw, &detached); validationErr != nil {
		return HarnessVersion{}, validationErr
	}
	sum := sha256.Sum256(data)
	detached.Digest = hex.EncodeToString(sum[:])
	return detached, nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
