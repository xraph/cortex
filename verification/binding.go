package verification

import (
	"fmt"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// Source records a captured resource revision and its host observation time.
type Source struct {
	Resource    string    `json:"resource"`
	Revision    string    `json:"revision"`
	Digest      string    `json:"digest"`
	RetrievedAt time.Time `json:"retrieved_at"`
}

// ToolRevision pins a tool implementation used by the captured execution.
type ToolRevision struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// ContextSnapshot contains host-attested identity and revision references.
// AuthorityRevision must change when grants are revoked. Verifiers cannot
// establish current authorization by inspecting this snapshot alone.
type ContextSnapshot struct {
	PrincipalRef        string         `json:"principal_ref"`
	AuthorityRevision   string         `json:"authority_revision"`
	PreparedDigest      string         `json:"prepared_digest"`
	PolicyRevision      string         `json:"policy_revision"`
	EnvironmentRevision string         `json:"environment_revision"`
	DatasetRevision     string         `json:"dataset_revision"`
	Sources             []Source       `json:"sources,omitempty"`
	Tools               []ToolRevision `json:"tools,omitempty"`
}

// Binding identifies the exact execution, candidate and context being checked.
// All evidence uses the exact scope; a parent scope cannot authorize a report.
type Binding struct {
	Scope          cortex.Scope    `json:"scope"`
	RunID          id.AgentRunID   `json:"run_id"`
	ContractDigest string          `json:"contract_digest"`
	Candidate      evidence.Ref    `json:"candidate"`
	Evidence       []evidence.Ref  `json:"evidence,omitempty"`
	Context        ContextSnapshot `json:"context"`
}

// Validate checks a binding structurally. A trusted host must still recheck
// resource access and compare current revisions before committing a result.
func (b Binding) Validate() error {
	if !validText(b.Scope.Canonical(), b.Context.PrincipalRef, b.Context.AuthorityRevision, b.Context.PolicyRevision, b.Context.EnvironmentRevision, b.Context.DatasetRevision) {
		return ErrInvalid
	}
	if err := cortex.ValidateScope(b.Scope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if b.RunID.IsNil() || b.RunID.Prefix() != id.PrefixAgentRun || !evidence.ValidDigest(b.ContractDigest) || !evidence.ValidDigest(b.Context.PreparedDigest) || strings.TrimSpace(b.Context.PrincipalRef) == "" || strings.TrimSpace(b.Context.AuthorityRevision) == "" || strings.TrimSpace(b.Context.PolicyRevision) == "" || strings.TrimSpace(b.Context.EnvironmentRevision) == "" || strings.TrimSpace(b.Context.DatasetRevision) == "" || len(b.Evidence) > 256 || len(b.Context.Sources) > 256 || len(b.Context.Tools) > 256 {
		return ErrInvalid
	}
	refs := append([]evidence.Ref{b.Candidate}, b.Evidence...)
	seen := make(map[id.ID]bool, len(refs))
	for _, ref := range refs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if ref.Scope.Canonical() != b.Scope.Canonical() || seen[ref.ID] {
			return ErrInvalid
		}
		seen[ref.ID] = true
	}
	sources := make(map[string]bool, len(b.Context.Sources))
	for _, source := range b.Context.Sources {
		if !validText(source.Resource, source.Revision) {
			return ErrInvalid
		}
		if strings.TrimSpace(source.Resource) == "" || strings.TrimSpace(source.Revision) == "" || !evidence.ValidDigest(source.Digest) || source.RetrievedAt.IsZero() || sources[source.Resource] {
			return ErrInvalid
		}
		sources[source.Resource] = true
	}
	tools := make(map[string]bool, len(b.Context.Tools))
	for _, tool := range b.Context.Tools {
		if !validText(tool.Name, tool.Revision) {
			return ErrInvalid
		}
		if strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Revision) == "" || tools[tool.Name] {
			return ErrInvalid
		}
		tools[tool.Name] = true
	}
	return nil
}

// Digest binds every reference in order, including identity and source timing.
func (b Binding) Digest() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	return recordDigest(b)
}
