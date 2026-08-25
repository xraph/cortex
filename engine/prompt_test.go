package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/trait"
)

// The overlay tests all share one agent id across every scope, on
// purpose. An overlay is a per-scope delta on ONE agent, so a fixture
// that gave each scope its own agent would pass whether the ancestor
// walk worked or not: the rows would already be separated by the agent
// id rather than by the scope under test.
//
// They also share one workspace, ws_x, with projects p1 and p2 beneath
// it, so the only thing that differs between a layered case and a
// sibling case is which project the run happens in.
const (
	overlayWorkspace = "ws_x"
	overlayProject   = "p1"
	siblingProject   = "p2"
)

func overlayScope(levels ...string) cortex.Scope {
	keys := []string{"workspace", "project"}
	out := cortex.Scope{}
	for i, v := range levels {
		out.Levels = append(out.Levels, cortex.Level{Key: keys[i], Value: v})
	}

	return out
}

func overlayCtx(levels ...string) context.Context {
	return cortex.WithScope(context.Background(), overlayScope(levels...))
}

// capturingLogger records Warn calls so a test can assert that a
// declined patch was actually surfaced. Everything else falls through to
// the embedded noop logger.
type capturingLogger struct {
	log.Logger

	mu    sync.Mutex
	warns []capturedWarn
}

type capturedWarn struct {
	msg    string
	fields map[string]any
}

func newCapturingLogger() *capturingLogger {
	return &capturingLogger{Logger: log.NewNoopLogger()}
}

func (c *capturingLogger) Warn(msg string, fields ...log.Field) {
	c.mu.Lock()
	defer c.mu.Unlock()

	w := capturedWarn{msg: msg, fields: make(map[string]any, len(fields))}
	for _, f := range fields {
		w.fields[f.Key()] = f.Value()
	}
	c.warns = append(c.warns, w)
}

func (c *capturingLogger) Warns() []capturedWarn {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]capturedWarn, len(c.warns))
	copy(out, c.warns)

	return out
}

// newPromptEngine builds an engine over a real migrated sqlite store.
// Overlay layering is a question about which rows a store returns for a
// given scope, so a hand-written fake that answers from a map would
// assert only that the fake agrees with itself.
func newPromptEngine(t *testing.T, opts ...engine.Option) (*engine.Engine, store.Store) {
	t.Helper()

	s := newSQLiteStoreForTest(t)
	e, err := engine.New(append([]engine.Option{engine.WithStore(s)}, opts...)...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	return e, s
}

//nolint:revive // ctx placement matches the other test helpers here; test-only, not a public API
func mustCreateOverlayAt(t *testing.T, s store.Store, ctx context.Context, agentID id.AgentID, patches ...prompt.Patch) {
	t.Helper()

	o := &prompt.Overlay{
		Entity:  cortex.NewEntity(),
		ID:      id.NewOverlayID(),
		AgentID: agentID,
		Patches: patches,
	}
	if err := s.CreateOverlay(ctx, o); err != nil {
		t.Fatalf("create overlay: %v", err)
	}
}

// sectionedAgent is the agent every overlay test patches: one host
// section it may rewrite and one it may not.
func sectionedAgent(agentID id.AgentID) *agent.Config {
	return &agent.Config{
		ID:   agentID,
		Name: "layered",
		Sections: []prompt.Section{
			{ID: "role", Source: prompt.SourceHost, Body: "You answer questions.", Order: prompt.OrderRole},
			{ID: "tone", Source: prompt.SourceHost, Body: "Be plain.", Order: prompt.OrderRole + 1},
		},
	}
}

// TestBuildSystemPrompt_NarrowestScopeWinsOnASharedSection is the
// layering promise. Two overlays on the same agent patch the same
// section, one at the workspace and one at the project inside it. The
// project's text is what the run ends up with, which only holds if the
// overlays applied broadest first.
func TestBuildSystemPrompt_NarrowestScopeWinsOnASharedSection(t *testing.T) {
	e, s := newPromptEngine(t)
	agentID := id.NewAgentID()

	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace), agentID,
		prompt.Patch{ID: "role", Body: "You answer workspace questions."})
	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace, overlayProject), agentID,
		prompt.Patch{ID: "role", Body: "You answer project questions."})

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace, overlayProject), sectionedAgent(agentID), nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if !strings.Contains(got, "You answer project questions.") {
		t.Errorf("prompt did not end with the project overlay's text; the narrower scope must patch last\ngot: %q", got)
	}
	if strings.Contains(got, "You answer workspace questions.") {
		t.Errorf("prompt still carries the workspace overlay's text for a section the project overlay replaced\ngot: %q", got)
	}
}

// TestBuildSystemPrompt_OverlaysAtBothScopesApplyToDifferentSections is
// the other half of the same promise. "Most specific wins" must not
// become "only the most specific applies": an ancestor's patch against a
// section the descendant never touched still has to land.
func TestBuildSystemPrompt_OverlaysAtBothScopesApplyToDifferentSections(t *testing.T) {
	e, s := newPromptEngine(t)
	agentID := id.NewAgentID()

	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace), agentID,
		prompt.Patch{ID: "tone", Body: "Be formal."})
	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace, overlayProject), agentID,
		prompt.Patch{ID: "role", Body: "You answer project questions."})

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace, overlayProject), sectionedAgent(agentID), nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if !strings.Contains(got, "Be formal.") {
		t.Errorf("the workspace overlay's patch was dropped once a project overlay existed\ngot: %q", got)
	}
	if !strings.Contains(got, "You answer project questions.") {
		t.Errorf("the project overlay's patch was dropped\ngot: %q", got)
	}
}

// TestBuildSystemPrompt_ASiblingProjectsOverlayNeverApplies is the
// cross-tenant guard. p1 and p2 sit under one workspace with one agent
// between them. Reaching for a prefix listing to find "the overlays that
// apply" pulls p1's overlay into p2's prompt, because prefix matching
// widens downward. The ancestor walk asks only for p2's own scope and
// the workspace above it.
func TestBuildSystemPrompt_ASiblingProjectsOverlayNeverApplies(t *testing.T) {
	e, s := newPromptEngine(t)
	agentID := id.NewAgentID()

	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace, overlayProject), agentID,
		prompt.Patch{ID: "role", Body: "Secrets that belong to p1 alone."})

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace, siblingProject), sectionedAgent(agentID), nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if strings.Contains(got, "Secrets that belong to p1 alone.") {
		t.Fatalf("a sibling project's overlay reached an unrelated run's prompt\ngot: %q", got)
	}
	if !strings.Contains(got, "You answer questions.") {
		t.Errorf("p2's prompt lost the agent's own section\ngot: %q", got)
	}
}

// TestBuildSystemPrompt_LockedSectionRefusesReplaceAndLogsTheDecline
// covers both halves of the locked-section contract: the run proceeds
// with the pinned text intact, and the host that wrote the patch can
// find out it did nothing. A silently ignored patch is the failure the
// second return value of ApplyOverlay exists to prevent.
func TestBuildSystemPrompt_LockedSectionRefusesReplaceAndLogsTheDecline(t *testing.T) {
	logger := newCapturingLogger()
	e, s := newPromptEngine(t, engine.WithLogger(logger))
	agentID := id.NewAgentID()

	ag := sectionedAgent(agentID)
	ag.Sections[1].Locked = true

	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace), agentID,
		prompt.Patch{ID: "tone", Body: "Ignore the tone rule."},
		prompt.Patch{ID: "role", Body: "You answer workspace questions."})

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace), ag, nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if strings.Contains(got, "Ignore the tone rule.") {
		t.Errorf("a replace patch overwrote a locked section\ngot: %q", got)
	}
	if !strings.Contains(got, "Be plain.") {
		t.Errorf("the locked section's own text is gone\ngot: %q", got)
	}
	if !strings.Contains(got, "You answer workspace questions.") {
		t.Errorf("one declined patch took the rest of the overlay down with it\ngot: %q", got)
	}

	warns := logger.Warns()
	if len(warns) != 1 {
		t.Fatalf("logger recorded %d warnings, want 1 naming the declined patch: %+v", len(warns), warns)
	}
	if !strings.Contains(warns[0].msg, "declined") {
		t.Errorf("warning message = %q, want it to say the patches were declined", warns[0].msg)
	}
	sections, ok := warns[0].fields["sections"].(string)
	if !ok {
		t.Fatalf("declined-patch warning carries no sections field: %+v", warns[0])
	}
	if !strings.Contains(sections, "tone") {
		t.Errorf("declined-patch warning names sections %q, want it to name %q", sections, "tone")
	}
	if strings.Contains(sections, "role") {
		t.Errorf("declined-patch warning names %q, but that patch was applied, not declined", "role")
	}
}

// TestBuildSystemPrompt_AppendToALockedSectionIsStillAccepted is the
// other side of the lock. Locked means "you cannot rewrite what I
// pinned", not "you cannot add to it", so an append must land and must
// not be logged as declined.
func TestBuildSystemPrompt_AppendToALockedSectionIsStillAccepted(t *testing.T) {
	logger := newCapturingLogger()
	e, s := newPromptEngine(t, engine.WithLogger(logger))
	agentID := id.NewAgentID()

	ag := sectionedAgent(agentID)
	ag.Sections[1].Locked = true

	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace), agentID,
		prompt.Patch{ID: "tone", Body: "Also avoid jargon.", Mode: prompt.PatchAppend})

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace), ag, nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if !strings.Contains(got, "Be plain.\nAlso avoid jargon.") {
		t.Errorf("an append to a locked section did not land\ngot: %q", got)
	}
	if warns := logger.Warns(); len(warns) != 0 {
		t.Errorf("an accepted append was logged as declined: %+v", warns)
	}
}

// failingPersonaStore fails persona resolution and delegates nothing
// else, so a lookup failure is the only thing under test.
type failingPersonaStore struct {
	store.Store
}

func (failingPersonaStore) GetPersonaByName(_ context.Context, _ string) (*persona.Persona, error) {
	return nil, cortex.ErrPersonaNotFound
}

// TestBuildSystemPrompt_PersonaLookupFailureAborts guards the fail-loud
// rule against the section pipeline. Collecting sections is a chain of
// appends, and the easy version of it drops a producer that returned an
// error instead of returning that error. An agent that silently loses
// its identity is exactly what this behavior was added to prevent.
func TestBuildSystemPrompt_PersonaLookupFailureAborts(t *testing.T) {
	e, err := engine.New(engine.WithStore(&failingPersonaStore{Store: scopespy.New()}))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ag := &agent.Config{
		ID:           id.NewAgentID(),
		Name:         "identified",
		SystemPrompt: "You answer questions.",
		PersonaRef:   "ghost",
	}

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace), ag, nil)
	if err == nil {
		t.Fatalf("BuildSystemPrompt succeeded with an unresolvable persona, returning %q; it must abort rather than assemble a prompt with the Identity section quietly missing", got)
	}
	if !errors.Is(err, cortex.ErrPersonaNotFound) {
		t.Errorf("err = %v, want it to wrap cortex.ErrPersonaNotFound", err)
	}
	if got != "" {
		t.Errorf("BuildSystemPrompt returned prompt %q alongside its error; an aborted build must return nothing usable", got)
	}
}

// legacyKnowledgePrompt is what v1.11.0 produced for the knowledge
// fixture below. The old knowledge chunk terminated every bullet with a
// newline, the last one included, and the old join then added its own
// separator on top, so a knowledge block was followed by three newlines
// where every other part had two.
const legacyKnowledgePrompt = "You answer questions about the deploy pipeline." +
	"\n\n## Skill: docs\nCite the source line." +
	"\n\n## Knowledge: runbook\n- restart the worker\n- roll the deploy back" +
	"\n\n\n## Trait: brief\nKeep it under three sentences."

// wantKnowledgePrompt is what the section pipeline produces for the same
// fixture: identical except that the stray blank line after the
// knowledge block is gone. The test below derives it from the legacy
// string rather than restating it, so the ONLY tolerated difference is
// that one newline.
var wantKnowledgePrompt = strings.Replace(legacyKnowledgePrompt, "back\n\n\n## Trait", "back\n\n## Trait", 1)

// staticKnowledge returns the same two chunks for any query.
type staticKnowledge struct{}

func (staticKnowledge) Retrieve(_ context.Context, _ string, _ *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	return []knowledge.ScoredChunk{
		{Content: "restart the worker"},
		{Content: "roll the deploy back"},
	}, nil
}

func (staticKnowledge) ListCollections(_ context.Context) ([]knowledge.CollectionInfo, error) {
	return nil, nil
}

// TestBuildSystemPrompt_KnowledgeBlockLosesOnlyItsTrailingNewline pins
// the one deliberate byte-level change in this release. Every other
// producer assembles byte-identically to v1.11.0, which
// TestBuildSystemPrompt_MatchesTheLegacyPrompt covers; that fixture has
// no knowledge in it, so this test is where the knowledge block's shape
// is recorded.
func TestBuildSystemPrompt_KnowledgeBlockLosesOnlyItsTrailingNewline(t *testing.T) {
	e, s := newPromptEngine(t, engine.WithKnowledge(staticKnowledge{}))
	ctx := overlayCtx(overlayWorkspace)

	sk := &skill.Skill{
		Entity:               cortex.NewEntity(),
		ID:                   id.NewSkillID(),
		Name:                 "docs",
		SystemPromptFragment: "Cite the source line.",
		Knowledge:            []skill.KnowledgeRef{{Source: "runbook"}},
	}
	if err := s.CreateSkill(ctx, sk); err != nil {
		t.Fatalf("create skill: %v", err)
	}

	tr := &trait.Trait{
		Entity: cortex.NewEntity(),
		ID:     id.NewTraitID(),
		Name:   "brief",
		Influences: []trait.Influence{
			{Target: trait.TargetPromptInjection, Value: "Keep it under three sentences."},
		},
	}
	if err := s.CreateTrait(ctx, tr); err != nil {
		t.Fatalf("create trait: %v", err)
	}

	got, err := e.BuildSystemPrompt(ctx, &agent.Config{
		ID:           id.NewAgentID(),
		Name:         "knowing",
		SystemPrompt: "You answer questions about the deploy pipeline.",
		InlineSkills: []string{"docs"},
		InlineTraits: []string{"brief"},
	}, nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if got != wantKnowledgePrompt {
		t.Errorf("assembled prompt drifted\n got: %q\nwant: %q", got, wantKnowledgePrompt)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("assembled prompt still carries a triple newline: %q", got)
	}
}

// TestCreateAgent_SyncsTheStoredSystemPromptFromSections wires the
// derived column to the writes that change it. agent.Config carries both
// Sections and the SystemPrompt they assemble to, and every reader that
// predates sections still serves the column, so a create or update that
// skipped the sync would hand those readers a prompt the agent no longer
// has. Both writes go through the engine, which is the one place every
// caller reaches the store from.
func TestCreateAgent_SyncsTheStoredSystemPromptFromSections(t *testing.T) {
	e, s := newPromptEngine(t)
	ctx := overlayCtx(overlayWorkspace)

	ag := sectionedAgent(id.NewAgentID())
	ag.Entity = cortex.NewEntity()
	if err := e.CreateAgent(ctx, ag); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	created, err := s.Get(ctx, ag.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	want := prompt.Assemble(ag.Sections)
	if created.SystemPrompt != want {
		t.Errorf("stored SystemPrompt after create = %q, want %q", created.SystemPrompt, want)
	}

	created.Sections[0].Body = "You answer only about deploys."
	if updErr := e.UpdateAgent(ctx, created); updErr != nil {
		t.Fatalf("UpdateAgent: %v", updErr)
	}

	updated, err := s.Get(ctx, ag.ID)
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	wantUpdated := prompt.Assemble(created.Sections)
	if updated.SystemPrompt != wantUpdated {
		t.Errorf("stored SystemPrompt after update = %q, want %q", updated.SystemPrompt, wantUpdated)
	}
	if !strings.Contains(updated.SystemPrompt, "You answer only about deploys.") {
		t.Errorf("the edited section never reached the stored prompt: %q", updated.SystemPrompt)
	}
}

// TestCloneAgent_SyncsTheClonesSystemPrompt covers the third agent-write
// path. A clone inherits its source's sections, so it inherits whatever
// the source's derived prompt was, stale included. Writing the clone
// without re-deriving is how a derived field starts drifting: one write
// path that skips the derivation is enough. The source here is stored
// straight through the store, which is the only way to produce the stale
// row the engine's own writes prevent.
func TestCloneAgent_SyncsTheClonesSystemPrompt(t *testing.T) {
	e, s := newPromptEngine(t)
	ctx := overlayCtx(overlayWorkspace)

	src := sectionedAgent(id.NewAgentID())
	src.Entity = cortex.NewEntity()
	src.SystemPrompt = "a prompt these sections never produced"
	if err := s.Create(ctx, src); err != nil {
		t.Fatalf("create source agent: %v", err)
	}

	clone, err := e.CloneAgent(ctx, src.Name, "layered-copy")
	if err != nil {
		t.Fatalf("CloneAgent: %v", err)
	}

	stored, err := s.Get(ctx, clone.ID)
	if err != nil {
		t.Fatalf("get clone: %v", err)
	}
	want := prompt.Assemble(src.Sections)
	if stored.SystemPrompt != want {
		t.Errorf("cloned SystemPrompt = %q, want %q re-derived from the sections it copied", stored.SystemPrompt, want)
	}
}

// legacyAssembledPrompt is the exact output of the pre-sections
// BuildSystemPrompt for the fixture built below. It was captured by
// running that engine method at the commit before the producers grew
// Sections, printed with %q, and pasted here verbatim. It is therefore a
// record of the old behavior and not a restatement of the new code: if
// assembly drifts by a single byte, this test fails.
//
// The skills are listed zeta-then-alpha and the traits curious-then-brief
// on purpose. Both lists are in the reverse of alphabetical order, so an
// implementation that lets sections fall back to sorting by ID produces a
// visibly different string instead of accidentally matching.
const legacyAssembledPrompt = "You answer questions about the deploy pipeline.\n" +
	"\n## Identity\nYou are a patient guide." +
	"\n\n## Skill: zeta\nCite the source line." +
	"\n\n## Skill: alpha\nPrefer the shortest answer." +
	"\n\n## Trait: curious\nAsk one clarifying question." +
	"\n\n## Trait: brief\nKeep it under three sentences."

// TestBuildSystemPrompt_MatchesTheLegacyPrompt is the compatibility
// promise for the whole release. An agent that only ever set
// SystemPrompt, with a persona, skills and traits behind it, must
// assemble to the byte-identical string it did before sections existed.
//
// It runs through the engine rather than through a test-local walk of
// the producers. The literal pins what a host actually receives, so the
// assertion has to sit on the real pipeline: a reordering inside
// collectSections is precisely the regression this exists to catch, and
// a re-implementation of the traversal would agree with itself while the
// engine drifted underneath it.
func TestBuildSystemPrompt_MatchesTheLegacyPrompt(t *testing.T) {
	e, s := newPromptEngine(t)
	ctx := overlayCtx(overlayWorkspace)

	if err := s.CreatePersona(ctx, &persona.Persona{
		Entity:   cortex.NewEntity(),
		ID:       id.NewPersonaID(),
		Name:     "guide",
		Identity: "You are a patient guide.",
	}); err != nil {
		t.Fatalf("create persona: %v", err)
	}

	for _, sk := range []*skill.Skill{
		{Name: "zeta", SystemPromptFragment: "Cite the source line."},
		{Name: "alpha", SystemPromptFragment: "Prefer the shortest answer."},
	} {
		sk.Entity = cortex.NewEntity()
		sk.ID = id.NewSkillID()
		if err := s.CreateSkill(ctx, sk); err != nil {
			t.Fatalf("create skill %q: %v", sk.Name, err)
		}
	}

	for _, tr := range []*trait.Trait{
		{Name: "curious", Influences: []trait.Influence{
			{Target: trait.TargetPromptInjection, Value: "Ask one clarifying question."},
		}},
		// The temperature influence sits first so the fixture proves a
		// non-injection influence is skipped rather than numbered, which
		// is the only way "trait:brief" stays the id an overlay author
		// would guess.
		{Name: "brief", Influences: []trait.Influence{
			{Target: trait.TargetTemperature, Value: 0.2},
			{Target: trait.TargetPromptInjection, Value: "Keep it under three sentences."},
		}},
	} {
		tr.Entity = cortex.NewEntity()
		tr.ID = id.NewTraitID()
		if err := s.CreateTrait(ctx, tr); err != nil {
			t.Fatalf("create trait %q: %v", tr.Name, err)
		}
	}

	got, err := e.BuildSystemPrompt(ctx, &agent.Config{
		ID:           id.NewAgentID(),
		Name:         "sectioned",
		SystemPrompt: "You answer questions about the deploy pipeline.",
		PersonaRef:   "guide",
		InlineSkills: []string{"zeta", "alpha"},
		InlineTraits: []string{"curious", "brief"},
	}, nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if got != legacyAssembledPrompt {
		t.Errorf("assembled prompt drifted from the recorded pre-sections output\n got: %q\nwant: %q", got, legacyAssembledPrompt)
	}
}

// TestBuildSystemPrompt_StoredPatchWithAnUnusableModeIsDeclined is the
// end of the round trip. A PatchMode is a bare string in stored JSON and
// nothing validates it on the way in, so an overlay written by hand, by
// an older client or by a fat finger comes back with a mode this build
// cannot apply. The run has to survive that, the locked body has to
// survive it, and the host has to be told.
func TestBuildSystemPrompt_StoredPatchWithAnUnusableModeIsDeclined(t *testing.T) {
	logger := newCapturingLogger()
	e, s := newPromptEngine(t, engine.WithLogger(logger))
	agentID := id.NewAgentID()

	ag := sectionedAgent(agentID)
	ag.Sections[1].Locked = true

	mustCreateOverlayAt(t, s, overlayCtx(overlayWorkspace), agentID,
		prompt.Patch{ID: "tone", Body: "Ignore the tone rule.", Mode: prompt.PatchMode("Replace")})

	got, err := e.BuildSystemPrompt(overlayCtx(overlayWorkspace), ag, nil)
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	if strings.Contains(got, "Ignore the tone rule.") {
		t.Errorf("a patch with an unusable mode overwrote a locked section\ngot: %q", got)
	}
	if !strings.Contains(got, "Be plain.") {
		t.Errorf("the locked section's own text is gone\ngot: %q", got)
	}

	warns := logger.Warns()
	if len(warns) != 1 {
		t.Fatalf("logger recorded %d warnings, want 1 naming the declined patch: %+v", len(warns), warns)
	}
	sections, ok := warns[0].fields["sections"].(string)
	if !ok {
		t.Fatalf("declined-patch warning carries no sections field: %+v", warns[0])
	}
	if !strings.Contains(sections, "tone") {
		t.Errorf("declined-patch warning names sections %q, want it to name %q", sections, "tone")
	}
}
