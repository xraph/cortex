package cortex

import "time"

// Config holds configuration for the Cortex engine.
type Config struct {
	// DefaultModel is the LLM model used when an agent does not specify one.
	DefaultModel string

	// DefaultMaxSteps is the maximum reasoning steps per run.
	DefaultMaxSteps int

	// DefaultMaxTokens is the maximum output tokens per LLM call.
	DefaultMaxTokens int

	// DefaultTemperature is the LLM sampling temperature.
	DefaultTemperature float64

	// DefaultReasoningLoop is the reasoning strategy when none is set.
	DefaultReasoningLoop string

	// ShutdownTimeout is the maximum time to wait for graceful shutdown.
	// The Forge extension applies it, bounding Engine.Stop from its own
	// Stop; engine.New alone does not read it.
	ShutdownTimeout time.Duration

	// RunConcurrency is the maximum number of agent runs executing at once,
	// zero for no limit (the default). The Forge extension applies it with
	// engine.WithRunConcurrency; engine.New alone does not read it.
	RunConcurrency int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		DefaultModel:         "smart",
		DefaultMaxSteps:      25,
		DefaultMaxTokens:     4096,
		DefaultTemperature:   0.7,
		DefaultReasoningLoop: "react",
		ShutdownTimeout:      30 * time.Second,
		RunConcurrency:       0,
	}
}
