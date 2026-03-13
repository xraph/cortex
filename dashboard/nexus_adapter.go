package dashboard

import (
	"context"

	"github.com/xraph/nexus"
)

// nexusAdapter wraps a *nexus.Gateway to satisfy the ModelSource interface.
type nexusAdapter struct {
	gw *nexus.Gateway
}

// NewNexusModelSource creates a ModelSource backed by a nexus Gateway.
func NewNexusModelSource(gw *nexus.Gateway) ModelSource {
	return &nexusAdapter{gw: gw}
}

func (a *nexusAdapter) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := a.gw.Models().ListModels(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		result = append(result, ModelInfo{
			ID:            m.ID,
			Provider:      m.Provider,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			MaxOutput:     m.MaxOutput,
			InputPricing:  m.Pricing.InputPerMillion,
			OutputPricing: m.Pricing.OutputPerMillion,
			Capabilities: ModelCapabilities{
				Chat:       m.Capabilities.Chat,
				Streaming:  m.Capabilities.Streaming,
				Embeddings: m.Capabilities.Embeddings,
				Images:     m.Capabilities.Images,
				Vision:     m.Capabilities.Vision,
				Tools:      m.Capabilities.Tools,
				JSON:       m.Capabilities.JSON,
				Audio:      m.Capabilities.Audio,
				Thinking:   m.Capabilities.Thinking,
				Batch:      m.Capabilities.Batch,
			},
		})
	}
	return result, nil
}

func (a *nexusAdapter) ListProviders(ctx context.Context) ([]ProviderInfo, error) {
	all := a.gw.Providers().All()
	healthy := a.gw.Providers().Healthy(ctx)

	healthySet := make(map[string]bool, len(healthy))
	for _, p := range healthy {
		healthySet[p.Name()] = true
	}

	result := make([]ProviderInfo, 0, len(all))
	for _, p := range all {
		models, _ := p.Models(ctx) //nolint:errcheck // best-effort UI data
		result = append(result, ProviderInfo{
			Name:       p.Name(),
			ModelCount: len(models),
			Healthy:    healthySet[p.Name()],
		})
	}
	return result, nil
}
