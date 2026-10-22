// Package planning validates immutable task graphs before a host schedules work.
// Graph validity does not establish authorization, verified task completion or
// reserved capacity. Those checks belong to the host and durable scheduler.
package planning

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// Entity prefixes distinguish plans from their task definitions.
const (
	PrefixPlan id.Prefix = "plan"
	PrefixTask id.Prefix = "ptask"
)

// ErrInvalid identifies an unsafe, unsupported or changed graph.
var ErrInvalid = errors.New("cortex: invalid plan")

// ErrConflict means the caller proposed a change against another revision.
var ErrConflict = errors.New("cortex: plan revision conflict")

// Access is a host-defined resource permission. Write includes read access.
type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

// ResourceGrant pins an allowed resource and its host revision.
type ResourceGrant struct {
	Resource string `json:"resource"`
	Revision string `json:"revision"`
	Access   Access `json:"access"`
}

// ArtifactRequirement declares the single output owned by a task. A later
// verifier must check its content, media type and byte bound before acceptance.
// The captured evidence Producer must come from ArtifactProducer.
type ArtifactRequirement struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	MaxBytes  int64  `json:"max_bytes"`
}

// Task is an immutable definition, separate from claims and execution state.
// Criteria reference the plan's host-authored acceptance contract.
type Task struct {
	ID          id.ID               `json:"id"`
	Instruction string              `json:"instruction"`
	Owner       string              `json:"owner"`
	DependsOn   []id.ID             `json:"depends_on,omitempty"`
	Criteria    []string            `json:"criteria"`
	Resources   []ResourceGrant     `json:"resources,omitempty"`
	Budget      execution.Budget    `json:"budget"`
	Output      ArtifactRequirement `json:"output"`
	Effects     []string            `json:"effects,omitempty"`
	Sources     []string            `json:"sources,omitempty"`
}

// Origin pins the execution configuration used to derive a graph. It is
// provenance, not authorization to load or execute the referenced configuration.
type Origin struct {
	Kind   string `json:"kind"`
	ID     id.ID  `json:"id"`
	Digest string `json:"digest"`
}

// Plan binds a graph to an exact scope, contract and authority ceiling. Digests
// establish integrity only; Actor is a host-authenticated audit reference.
type Plan struct {
	SchemaVersion  int                           `json:"schema_version"`
	ID             id.ID                         `json:"id"`
	Scope          cortex.Scope                  `json:"scope"`
	Revision       int64                         `json:"revision"`
	Digest         string                        `json:"digest"`
	PreviousDigest string                        `json:"previous_digest,omitempty"`
	Objective      string                        `json:"objective"`
	Contract       verification.Contract         `json:"contract"`
	Budget         execution.Budget              `json:"budget"`
	Owners         []string                      `json:"owners"`
	Resources      []ResourceGrant               `json:"resources,omitempty"`
	Tasks          []Task                        `json:"tasks"`
	Actor          string                        `json:"actor"`
	Reason         string                        `json:"reason"`
	AffectedTasks  []id.ID                       `json:"affected_tasks,omitempty"`
	Effects        []execution.EffectRequirement `json:"effects,omitempty"`
	Sources        []verification.Source         `json:"sources,omitempty"`
	Origin         Origin                        `json:"origin,omitzero"`
}

// Limits can narrow the hard bounds of 256 tasks, 64 levels and 4096 edges.
// Zero values use those bounds; negative or larger values are invalid.
type Limits struct {
	MaxTasks int
	MaxDepth int
	MaxEdges int
}

// Seal validates and detaches a definition before hashing its canonical JSON.
func Seal(p Plan, limits Limits) (Plan, error) {
	p.Digest = ""
	if err := validateFields(p, limits); err != nil {
		return Plan{}, err
	}
	data, err := encode(p)
	if err != nil {
		return Plan{}, err
	}
	var detached Plan
	if err = json.Unmarshal(data, &detached); err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	detached.Digest = evidence.Digest(data)
	return detached, nil
}

// Validate rejects modified definitions without resealing them.
func Validate(p Plan, limits Limits) error {
	if err := validateFields(p, limits); err != nil {
		return err
	}
	expected := p.Digest
	p.Digest = ""
	data, err := encode(p)
	if err != nil {
		return err
	}
	if expected != evidence.Digest(data) {
		return invalid("digest mismatch")
	}
	return nil
}

func encode(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(data) > 1<<20 {
		return nil, invalid("record exceeds one MiB")
	}
	return data, nil
}

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

func textValid(values ...string) bool {
	for _, value := range values {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return true
}
