package optimization

import (
	"context"

	"github.com/xraph/cortex/prompt"
)

// ControllerSpec freezes the model, instructions and tool names of an improver.
type ControllerSpec struct {
	Model         string           `json:"model"`
	ModelRevision string           `json:"model_revision"`
	Sections      []prompt.Section `json:"sections"`
	Tools         []string         `json:"tools"`
	Temperature   float64          `json:"temperature"`
}

// Controller proposes bounded edits and reflects only on eligible discovery data.
type Controller interface {
	Propose(context.Context, ProposalInput) ([]Proposal, error)
	Compose(context.Context, CompositionInput) (Proposal, error)
	Reflect(context.Context, ReflectionInput) ([]LessonDraft, error)
}

// DiscoveryEvidence deliberately omits frozen evaluator definitions and answers.
// Callers must also filter it by split before supplying it to a controller.
type DiscoveryEvidence struct {
	Trial      Trial      `json:"trial"`
	Evaluation Evaluation `json:"evaluation"`
}

// ProposalInput carries the current discovery baseline and frozen lesson set.
type ProposalInput struct {
	Discoveries []DiscoveryLessons  `json:"discoveries,omitempty"`
	Baseline    HarnessVersion      `json:"baseline"`
	Round       int                 `json:"round"`
	Evidence    []DiscoveryEvidence `json:"evidence"`
	Lessons     []Lesson            `json:"lessons"`
	Policy      EditPolicy          `json:"policy"`
}

// CompositionInput combines executed candidates without inheriting their scores.
type CompositionInput struct {
	ProposalInput
	Candidates []Candidate `json:"candidates"`
}

// ReflectionInput excludes held-out evaluations, even after a decision exists.
type ReflectionInput struct {
	Evidence   []DiscoveryEvidence `json:"evidence"`
	Candidates []Candidate         `json:"candidates"`
	Domain     string              `json:"domain"`
}
