package optimization

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/fabriq/core/registry"

	opt "github.com/xraph/cortex/optimization"
)

type readinessEmbedder struct{ fail bool }

func (*readinessEmbedder) Dims() int { return 3 }
func (e *readinessEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if e.fail {
		return nil, errors.New("embedding unavailable")
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func TestReadinessRequiresBoundPolicyAndWorkingRecall(t *testing.T) {
	adapter, _, fabric, ctx := adapterFixture(t)
	if err := adapter.CheckReady(ctx); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("unknown toolkit policy reported ready: %v", err)
	}
	reg := registry.New()
	reg.MustRegister(LessonSpec(adapter.config.Entity))
	embedder := &readinessEmbedder{}
	toolkit, err := NewLessonToolkit(fabric, reg, embedder, adapter.config.Entity)
	if err != nil {
		t.Fatal(err)
	}
	adapter.toolkit = toolkit
	if err = adapter.CheckReady(ctx); err != nil {
		t.Fatal(err)
	}
	embedder.fail = true
	if err = adapter.CheckReady(ctx); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("unavailable retrieval reported ready: %v", err)
	}
}
