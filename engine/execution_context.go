package engine

import (
	"context"

	"github.com/xraph/cortex/run"
)

type executionContextKey struct{}

func withExecutionProvenance(ctx context.Context, provenance run.ExecutionProvenance) context.Context {
	return context.WithValue(ctx, executionContextKey{}, provenance)
}

// ExecutionProvenanceFromContext returns trusted execution identity for adapters
// and hooks. The copy prevents a consumer from changing another observer's view.
func ExecutionProvenanceFromContext(ctx context.Context) (run.ExecutionProvenance, bool) {
	provenance, ok := ctx.Value(executionContextKey{}).(run.ExecutionProvenance)
	if !ok {
		return run.ExecutionProvenance{}, false
	}
	provenance.Sources = append([]run.Revision(nil), provenance.Sources...)
	provenance.Tools = append([]run.Revision(nil), provenance.Tools...)
	provenance.Diagnostics = append([]string(nil), provenance.Diagnostics...)
	return provenance, true
}

func preparedBytes(p *PreparedAgent) []byte {
	if p == nil {
		return nil
	}
	// Construction and decoding already validated this payload.
	data, _ := p.MarshalJSON() //nolint:errcheck // immutable validated snapshot
	return data
}
