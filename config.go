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
	ShutdownTimeout time.Duration

	// RunConcurrency is the maximum number of agent runs processed concurrently.
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
		RunConcurrency:       4,
	}
}
