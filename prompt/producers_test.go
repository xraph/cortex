package prompt_test

import (
	"strings"
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

// legacyAssembledPrompt is the exact output of the pre-sections
// BuildSystemPrompt for the fixture built by newFixture below. It was
// captured by running that engine method at the commit before the
// producers grew Sections, printed with %q, and pasted here verbatim.
// It is therefore a record of the old behavior and not a restatement of
// the new code: if section assembly drifts by a single byte, this test
// fails.
//
// The skills are listed zeta-then-alpha and the traits curious-then-brief
// on purpose. Both lists are in the reverse of alphabetical order, so an
// implementation that lets sections fall back to sorting by ID produces
// a visibly different string instead of accidentally matching.
const legacyAssembledPrompt = "You answer questions about the deploy pipeline.\n" +
	"\n## Identity\nYou are a patient guide." +
	"\n\n## Skill: zeta\nCite the source line." +
	"\n\n## Skill: alpha\nPrefer the shortest answer." +
	"\n\n## Trait: curious\nAsk one clarifying question." +
	"\n\n## Trait: brief\nKeep it under three sentences."

// fixture is the representative agent: a system prompt, a persona, two
// skills and two traits, which between them cover every producer.
type fixture struct {
	agentCfg *agent.Config
	person   *persona.Persona
	skills   []*skill.Skill
	traits   []*trait.Trait
}

func newFixture() fixture {
	return fixture{
		agentCfg: &agent.Config{
			Name:         "sectioned",
			SystemPrompt: "You answer questions about the deploy pipeline.",
			PersonaRef:   "guide",
			InlineSkills: []string{"zeta", "alpha"},
			InlineTraits: []string{"curious", "brief"},
		},
		person: &persona.Persona{Name: "guide", Identity: "You are a patient guide."},
		skills: []*skill.Skill{
			{Name: "zeta", SystemPromptFragment: "Cite the source line."},
			{Name: "alpha", SystemPromptFragment: "Prefer the shortest answer."},
		},
		traits: []*trait.Trait{
			{Name: "curious", Influences: []trait.Influence{
				{Target: trait.TargetPromptInjection, Value: "Ask one clarifying question."},
			}},
			// The temperature influence sits first so the fixture proves
			// that a non-injection influence is skipped rather than
			// numbered, which is the only way "trait:brief" stays the id
			// an overlay author would guess.
			{Name: "brief", Influences: []trait.Influence{
				{Target: trait.TargetTemperature, Value: 0.2},
				{Target: trait.TargetPromptInjection, Value: "Keep it under three sentences."},
			}},
		},
	}
}

// collect walks the fixture the way an assembler does: the agent's own
// sections first, then the persona, then each skill in listed order,
// then each trait in listed order.
func (f fixture) collect() []prompt.Section {
	out := f.agentCfg.PromptSections()
	out = append(out, f.person.Sections(prompt.OrderPersona)...)

	order := prompt.OrderSkill
	for _, sk := range f.skills {
		secs := sk.Sections(order)
		order += len(secs)
		out = append(out, secs...)
	}

	order = prompt.OrderTrait
	for _, tr := range f.traits {
		secs := tr.Sections(order)
		order += len(secs)
		out = append(out, secs...)
	}

	return out
}

// TestAssemble_ProducerSectionsMatchTheLegacyPrompt is the compatibility
// promise for the whole release. An agent that only ever set
// SystemPrompt, with a persona, skills and traits, must assemble to the
// byte-identical string it did before sections existed.
func TestAssemble_ProducerSectionsMatchTheLegacyPrompt(t *testing.T) {
	got := prompt.Assemble(newFixture().collect())

	if got != legacyAssembledPrompt {
		t.Errorf("assembled prompt drifted from the recorded pre-sections output\n got: %q\nwant: %q", got, legacyAssembledPrompt)
	}
}

// TestProducerSections_LeaveTitleEmpty guards the subtler half of the
// same promise. Assemble prefixes a non-empty Title to its section, so a
// producer that sets one silently rewrites every existing agent's
// prompt. Titles are for host-authored sections only.
func TestProducerSections_LeaveTitleEmpty(t *testing.T) {
	for _, s := range newFixture().collect() {
		if s.Title != "" {
			t.Errorf("section %q has Title %q; producer sections must carry their heading in Body, since a Title is prefixed by Assemble and shifts the prompt", s.ID, s.Title)
		}
	}
}

func TestProducerSections_OrderFollowsTheProducerBands(t *testing.T) {
	tests := []struct {
		name      string
		sectionID string
		wantOrder int
	}{
		{name: "agent role", sectionID: "role", wantOrder: prompt.OrderRole},
		{name: "persona identity", sectionID: "persona:identity", wantOrder: prompt.OrderPersona},
		{name: "first listed skill", sectionID: "skill:zeta", wantOrder: prompt.OrderSkill},
		{name: "second listed skill", sectionID: "skill:alpha", wantOrder: prompt.OrderSkill + 1},
		{name: "first listed trait", sectionID: "trait:curious", wantOrder: prompt.OrderTrait},
		{name: "second listed trait", sectionID: "trait:brief", wantOrder: prompt.OrderTrait + 1},
	}

	got := newFixture().collect()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, s := range got {
				if s.ID != tt.sectionID {
					continue
				}
				if s.Order != tt.wantOrder {
					t.Errorf("section %q Order = %d, want %d", s.ID, s.Order, tt.wantOrder)
				}

				return
			}
			t.Fatalf("no section with id %q was produced; got %d sections", tt.sectionID, len(got))
		})
	}
}

func TestProducerSections_CarryTheirSource(t *testing.T) {
	want := map[string]prompt.Source{
		"role":             prompt.SourceHost,
		"persona:identity": prompt.SourcePersona,
		"skill:zeta":       prompt.SourceSkill,
		"skill:alpha":      prompt.SourceSkill,
		"trait:curious":    prompt.SourceTrait,
		"trait:brief":      prompt.SourceTrait,
	}

	for _, s := range newFixture().collect() {
		if w, ok := want[s.ID]; !ok {
			t.Errorf("unexpected section id %q", s.ID)
		} else if s.Source != w {
			t.Errorf("section %q Source = %q, want %q", s.ID, s.Source, w)
		}
	}
}

func TestPersonaSections_EmptyIdentityProducesNothing(t *testing.T) {
	p := &persona.Persona{Name: "guide"}

	if got := p.Sections(prompt.OrderPersona); len(got) != 0 {
		t.Errorf("Sections on a persona with no identity = %+v, want none", got)
	}
}

func TestSkillSections_EmptyFragmentProducesNothing(t *testing.T) {
	s := &skill.Skill{Name: "alpha"}

	if got := s.Sections(prompt.OrderSkill); len(got) != 0 {
		t.Errorf("Sections on a skill with no fragment = %+v, want none", got)
	}
}

func TestTraitSections_SkipsInfluencesThatAreNotPromptInjections(t *testing.T) {
	tr := &trait.Trait{Name: "warm", Influences: []trait.Influence{
		{Target: trait.TargetTemperature, Value: 0.9},
		{Target: trait.TargetMaxSteps, Value: 4},
		// A prompt injection whose value is not a string is unusable and
		// has always been dropped rather than rendered as a Go value.
		{Target: trait.TargetPromptInjection, Value: 42},
		{Target: trait.TargetPromptInjection, Value: ""},
	}}

	if got := tr.Sections(prompt.OrderTrait); len(got) != 0 {
		t.Errorf("Sections = %+v, want none: only non-empty string prompt injections produce a section", got)
	}
}

func TestTraitSections_NumbersTheSecondInjectionOnwards(t *testing.T) {
	tr := &trait.Trait{Name: "warm", Influences: []trait.Influence{
		{Target: trait.TargetPromptInjection, Value: "first"},
		{Target: trait.TargetPromptInjection, Value: "second"},
		{Target: trait.TargetPromptInjection, Value: "third"},
	}}

	got := tr.Sections(prompt.OrderTrait)
	if len(got) != 3 {
		t.Fatalf("Sections returned %d sections, want 3", len(got))
	}

	wantIDs := []string{"trait:warm", "trait:warm:2", "trait:warm:3"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("section %d id = %q, want %q", i, got[i].ID, want)
		}
		if got[i].Order != prompt.OrderTrait+i {
			t.Errorf("section %d Order = %d, want %d", i, got[i].Order, prompt.OrderTrait+i)
		}
	}

	if assembled := prompt.Assemble(got); !strings.Contains(assembled, "first\n\n## Trait: warm\nsecond") {
		t.Errorf("a trait's injections lost their declared order when assembled: %q", assembled)
	}
}
