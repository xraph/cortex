package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
)

const preparedSchemaVersion = 2

// PreparedAgent holds an immutable, versioned execution snapshot. MarshalJSON
// returns a copy; no accessor exposes mutable prompt or tool data.
type PreparedAgent struct {
	payload json.RawMessage
	digest  string
}

type preparedSnapshot struct {
	SchemaVersion int                     `json:"schema_version"`
	Scope         cortex.Scope            `json:"scope"`
	Agent         agent.Config            `json:"agent"`
	Config        resolvedConfig          `json:"config"`
	Sections      []prompt.Section        `json:"sections"`
	Tools         []llm.Tool              `json:"tools"`
	Provenance    run.ExecutionProvenance `json:"provenance"`
}

type preparedWire struct {
	Digest   string          `json:"digest"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// Digest is the SHA-256 identity of the immutable snapshot.
func (p *PreparedAgent) Digest() string {
	if p == nil {
		return ""
	}
	return p.digest
}

// MarshalJSON serializes the frozen snapshot with its integrity digest.
func (p PreparedAgent) MarshalJSON() ([]byte, error) {
	if len(p.payload) == 0 {
		return nil, fmt.Errorf("cortex: empty prepared snapshot: %w", cortex.ErrInvalidPrepared)
	}
	return json.Marshal(preparedWire{Digest: p.digest, Snapshot: p.payload})
}

// UnmarshalJSON rejects unsupported versions and payloads whose digest changed.
func (p *PreparedAgent) UnmarshalJSON(data []byte) error {
	var wire preparedWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("cortex: decode prepared snapshot: %w", err)
	}
	var snap preparedSnapshot
	if err := decodePreparedJSON(wire.Snapshot, &snap); err != nil {
		return fmt.Errorf("cortex: decode prepared payload: %w", err)
	}
	if snap.SchemaVersion != preparedSchemaVersion {
		return fmt.Errorf("cortex: prepared schema %d: %w", snap.SchemaVersion, cortex.ErrInvalidPrepared)
	}
	// Canonicalize JSON whitespace without changing semantic numeric values.
	var canonical json.RawMessage
	canonical, err := json.Marshal(wire.Snapshot)
	if err != nil {
		return err
	}
	if contentDigest(canonical) != wire.Digest {
		return fmt.Errorf("cortex: prepared digest mismatch: %w", cortex.ErrInvalidPrepared)
	}
	if cortex.ValidateScope(snap.Scope) != nil || cortex.ValidateScope(snap.Agent.Scope) != nil || snap.Scope.Canonical() != snap.Agent.Scope.Canonical() || snap.Agent.ID.IsNil() || snap.Config.MaxSteps <= 0 {
		return fmt.Errorf("cortex: incomplete prepared snapshot: %w", cortex.ErrInvalidPrepared)
	}
	if err := snap.Config.Budget.Validate(); err != nil {
		return err
	}
	policy, policyErr := snap.Config.Publication.Resolve()
	if policyErr != nil || policy != snap.Config.Publication {
		return cortex.ErrInvalidPrepared
	}
	p.payload = append([]byte(nil), canonical...)
	p.digest = wire.Digest
	return nil
}

func (p *PreparedAgent) snapshot() (*preparedSnapshot, error) {
	if p == nil || len(p.payload) == 0 {
		return nil, fmt.Errorf("cortex: missing prepared snapshot: %w", cortex.ErrInvalidPrepared)
	}
	if contentDigest(p.payload) != p.digest {
		return nil, cortex.ErrInvalidPrepared
	}
	var snap preparedSnapshot
	if err := decodePreparedJSON(p.payload, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// PrepareAgent resolves an agent and freezes its effective configuration and
// sources under the caller's exact scope. Tool authorization remains live.
func (e *Engine) PrepareAgent(ctx context.Context, agentID id.AgentID, overrides *RunOverrides) (*PreparedAgent, error) {
	ctx, span := e.startPrepare(ctx, agentID.String())
	p, err := e.prepareAgentByID(ctx, agentID, overrides)
	endPrepare(span, err)
	return p, err
}

func (e *Engine) prepareAgentByID(ctx context.Context, agentID id.AgentID, overrides *RunOverrides) (*PreparedAgent, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if _, scopeErr := cortex.RequireScope(ctx); scopeErr != nil {
		return nil, scopeErr
	}
	if e.atomicPreparation {
		return e.prepareAtomicAgent(ctx, agentID, overrides)
	}
	ag, err := e.store.Get(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("cortex: prepare agent: %w", err)
	}
	return e.buildPrepared(ctx, ag, overrides)
}

func (e *Engine) collectPreparedAgent(ctx context.Context, ag *agent.Config, overrides *RunOverrides) (*preparedSnapshot, error) {
	if ag == nil || e.store == nil {
		return nil, cortex.ErrInvalidPrepared
	}
	if scopeErr := requireAgentScope(ctx, ag); scopeErr != nil {
		return nil, scopeErr
	}
	readSet := &sourceReadSet{}
	ctx = context.WithValue(ctx, sourceReadSetKey{}, readSet)
	agentID := ag.ID
	ag, err := recordPreparedSource(ctx, "agent:"+agentID.String(), ag, func(readCtx context.Context) (*agent.Config, error) {
		return e.preparationSources(readCtx).Get(readCtx, agentID)
	})
	if err != nil {
		return nil, err
	}
	overlays, err := e.loadScopeOverlays(ctx, ag.ID)
	if err != nil {
		return nil, fmt.Errorf("cortex: prepare overlays: %w", err)
	}
	cfg := e.effectiveConfig(ag, overrides, overlays)
	cfg.Publication = e.frozenPublication()
	if budgetErr := cfg.Budget.Validate(); budgetErr != nil {
		return nil, budgetErr
	}
	if cfg.MaxSteps <= 0 {
		return nil, fmt.Errorf("cortex: preparation needs a positive step limit: %w", cortex.ErrInvalidPrepared)
	}
	sections, diagnostics, err := e.collectPreparedSections(ctx, ag, overrides)
	if err != nil {
		return nil, fmt.Errorf("cortex: prepare prompt: %w", err)
	}
	sections = e.applyOverlaySections(sections, overlays)
	defs := e.toolDefinitions(cfg.Tools, cfg.ToolsRestricted)
	if cfg.AllToolsRestricted {
		defs = filterByName(defs, cfg.Tools)
	}
	for _, name := range cfg.Tools {
		if len(filterByName(defs, []string{name})) == 0 {
			return nil, fmt.Errorf("cortex: required tool %q unavailable: %w", name, cortex.ErrInvalidPrepared)
		}
	}
	provenance := run.ExecutionProvenance{Diagnostics: diagnostics, SchemaVersion: 1, Purpose: run.PurposeNormal, ModelRevision: e.modelRevisions[cfg.Model]}
	if provenance.ModelRevision == "" {
		provenance.Diagnostics = append(provenance.Diagnostics, "model revision is not pinned")
	}
	for _, def := range defs {
		revision := e.toolRevisions[def.Name]
		provenance.Tools = append(provenance.Tools, run.Revision{Name: def.Name, Revision: revision})
		if revision == "" {
			provenance.Diagnostics = append(provenance.Diagnostics, "tool implementation revision unavailable: "+def.Name)
		}
	}
	if e.knowledge == nil {
		provenance.Diagnostics = append(provenance.Diagnostics, "optional knowledge provider unavailable")
	}
	snap := preparedSnapshot{SchemaVersion: preparedSchemaVersion, Scope: cortex.ScopeFromContext(ctx), Agent: *ag, Config: cfg, Sections: sections, Tools: defs, Provenance: provenance}
	if !isAtomicPreparation(ctx) {
		if publicationErr := e.checkPreparedSources(ctx, ag, &snap); publicationErr != nil {
			return nil, publicationErr
		}
	}
	snap.Provenance.SourceReads, err = readSet.validate(ctx)
	if err != nil {
		return nil, err
	}
	if !isAtomicPreparation(ctx) {
		snap.Provenance.SourceValidation = "read_set_checked"
		snap.Provenance.Diagnostics = append(snap.Provenance.Diagnostics, "source read set checked; atomic source snapshot unavailable")
	}
	return &snap, nil
}

func freezePrepared(snap *preparedSnapshot) (*PreparedAgent, error) {
	snap.Provenance.Sources = nil
	for _, section := range snap.Sections {
		data, err := json.Marshal(section)
		if err != nil {
			return nil, err
		}
		snap.Provenance.Sources = append(snap.Provenance.Sources, run.Revision{Name: string(section.Source) + ":" + section.ID, Digest: contentDigest(data)})
	}
	revisions := make(map[string]string, len(snap.Provenance.Tools))
	for _, tool := range snap.Provenance.Tools {
		revisions[tool.Name] = tool.Revision
	}
	snap.Provenance.Tools = nil
	for _, tool := range snap.Tools {
		data, err := json.Marshal(tool)
		if err != nil {
			return nil, fmt.Errorf("cortex: snapshot tool %s: %w", tool.Name, err)
		}
		snap.Provenance.Tools = append(snap.Provenance.Tools, run.Revision{Name: tool.Name, Digest: contentDigest(data), Revision: revisions[tool.Name]})
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("cortex: snapshot preparation: %w", err)
	}
	return &PreparedAgent{payload: data, digest: contentDigest(data)}, nil
}

func contentDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// JSON schemas may contain integers larger than float64 can represent exactly.
func decodePreparedJSON(data []byte, dest any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(dest)
}
