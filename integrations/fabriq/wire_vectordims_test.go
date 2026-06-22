package fabriqbrain

import (
	"context"
	"testing"

	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"
	log "github.com/xraph/go-utils/log"
)

// stubFabric satisfies fabricFacade. Its embedded query.Fabric is nil: none of
// its methods are exercised because buildToolkit's dims check returns before the
// fabric is touched. Registry is non-nil so NewToolkit's nil-registry guard passes.
type stubFabric struct {
	query.Fabric
	reg *registry.Registry
}

func (s stubFabric) Registry() *registry.Registry { return s.reg }

// dimsEmbedder reports a fixed dimensionality; Embed is never called in these tests.
type dimsEmbedder struct{ dims int }

func (e dimsEmbedder) Dims() int { return e.dims }
func (dimsEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, nil
}

// captureLogger records Error-level messages so a test can assert a failure was
// surfaced rather than silently dropped. The embedded no-op Logger supplies the
// rest of the interface.
type captureLogger struct {
	log.Logger
	errors []string
}

func (c *captureLogger) Error(msg string, _ ...log.Field) { c.errors = append(c.errors, msg) }

func newStubFabric() stubFabric { return stubFabric{reg: registry.New()} }

// A configured vector-dims value is threaded into the toolkit: a 1536-dim
// embedder is accepted only because WithVectorDims(1536) overrides the 768 default.
func TestBuildToolkit_ThreadsVectorDims(t *testing.T) {
	c := applyOptions([]Option{
		WithEmbedder(dimsEmbedder{dims: 1536}),
		WithVectorDims(1536),
	})
	if _, err := buildToolkit(newStubFabric(), c); err != nil {
		t.Fatalf("buildToolkit with matching WithVectorDims = %v, want nil", err)
	}
}

// Without WithVectorDims, fabriq's 768 default applies, so a 1536-dim embedder
// is a mismatch and buildToolkit must report it.
func TestBuildToolkit_RejectsDimMismatchAgainstDefault(t *testing.T) {
	c := applyOptions([]Option{WithEmbedder(dimsEmbedder{dims: 1536})})
	if _, err := buildToolkit(newStubFabric(), c); err == nil {
		t.Fatal("buildToolkit with 1536-dim embedder vs 768 default = nil, want dims-mismatch error")
	}
}

// A dims mismatch in the wiring path must be logged (surfaced), not silently
// swallowed into a no-op brain.
func TestResolveToolkit_LogsDimsMismatch(t *testing.T) {
	cl := &captureLogger{Logger: log.NewNoopLogger()}
	c := applyOptions([]Option{
		WithEmbedder(dimsEmbedder{dims: 1536}),
		WithLogger(cl),
	})
	tk := resolveToolkit(newStubFabric(), c)
	if tk != nil {
		t.Fatalf("resolveToolkit on dims mismatch = %v, want nil", tk)
	}
	if len(cl.errors) == 0 {
		t.Fatal("dims mismatch was silently dropped; expected an error log")
	}
}
