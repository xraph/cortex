package fabriqbrain

import (
	"testing"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/vessel"
)

func TestEngineOptions_NoFabriqFacadeIsNoop(t *testing.T) {
	c := vessel.New() // empty container: no *fabriq.Fabriq provided
	opts := EngineOptions(c)
	if opts != nil {
		t.Fatalf("EngineOptions with no facade = %v, want nil", opts)
	}
	// EngineOption must be a safe no-op that applies cleanly.
	e, err := engine.New(EngineOption(c))
	if err != nil {
		t.Fatalf("engine.New with no-op EngineOption: %v", err)
	}
	if e.Knowledge() != nil {
		t.Fatalf("knowledge should be nil when no facade present")
	}
}
