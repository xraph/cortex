package optimization

import "github.com/xraph/cortex/prompt"

// ControllerSpec freezes the model, instructions and tool names of an improver.
type ControllerSpec struct {
	Model         string           `json:"model"`
	ModelRevision string           `json:"model_revision"`
	Sections      []prompt.Section `json:"sections"`
	Tools         []string         `json:"tools"`
	Temperature   float64          `json:"temperature"`
}
