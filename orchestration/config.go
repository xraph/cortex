package orchestration

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// OrchestrationConfig is a stored, named definition of a multi-agent
// orchestration: a strategy plus its participant agents and tunables.
type OrchestrationConfig struct {
	cortex.Entity
	ID           id.OrchestrationConfigID `json:"id"`
	Name         string                   `json:"name"`
	Description  string                   `json:"description,omitempty"`
	AppID        string                   `json:"app_id"`
	Strategy     string                   `json:"strategy"`
	Participants []Participant            `json:"participants"`
	Settings     Settings                 `json:"settings,omitempty"`
	Metadata     map[string]any           `json:"metadata,omitempty"`
}

// ConfigStore defines persistence for orchestration configs.
type ConfigStore interface {
	CreateOrchestration(ctx context.Context, c *OrchestrationConfig) error
	GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*OrchestrationConfig, error)
	GetOrchestrationByName(ctx context.Context, appID, name string) (*OrchestrationConfig, error)
	UpdateOrchestration(ctx context.Context, c *OrchestrationConfig) error
	DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error
	ListOrchestrations(ctx context.Context, filter *ConfigListFilter) ([]*OrchestrationConfig, error)
	CountOrchestrations(ctx context.Context, filter *ConfigListFilter) (int64, error)
}

// ConfigListFilter controls pagination and filtering for orchestration listing.
type ConfigListFilter struct {
	AppID  string
	Search string
	Limit  int
	Offset int
}
