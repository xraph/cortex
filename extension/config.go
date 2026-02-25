package extension

import "time"

// Config holds the Cortex extension configuration.
// Fields can be set programmatically via ExtOption functions or loaded from
// YAML configuration files (under "extensions.cortex" or "cortex" keys).
type Config struct {
	// DisableRoutes prevents HTTP route registration.
	DisableRoutes bool `json:"disable_routes" mapstructure:"disable_routes" yaml:"disable_routes"`

	// DisableMigrate prevents auto-migration on start.
	DisableMigrate bool `json:"disable_migrate" mapstructure:"disable_migrate" yaml:"disable_migrate"`

	// BasePath is the URL prefix for cortex routes (default: "/cortex").
	BasePath string `json:"base_path" mapstructure:"base_path" yaml:"base_path"`

	// DefaultModel is the LLM model to use when none is specified.
	DefaultModel string `json:"default_model" mapstructure:"default_model" yaml:"default_model"`

	// DefaultMaxSteps is the maximum reasoning steps per run.
	DefaultMaxSteps int `json:"default_max_steps" mapstructure:"default_max_steps" yaml:"default_max_steps"`

	// DefaultMaxTokens is the maximum tokens per LLM call.
	DefaultMaxTokens int `json:"default_max_tokens" mapstructure:"default_max_tokens" yaml:"default_max_tokens"`

	// DefaultTemperature is the LLM sampling temperature.
	DefaultTemperature float64 `json:"default_temperature" mapstructure:"default_temperature" yaml:"default_temperature"`

	// DefaultReasoningLoop is the reasoning loop strategy (e.g., "react").
	DefaultReasoningLoop string `json:"default_reasoning_loop" mapstructure:"default_reasoning_loop" yaml:"default_reasoning_loop"`

	// ShutdownTimeout is the maximum time to wait for graceful shutdown.
	ShutdownTimeout time.Duration `json:"shutdown_timeout" mapstructure:"shutdown_timeout" yaml:"shutdown_timeout"`

	// RunConcurrency controls how many agent runs can execute in parallel.
	RunConcurrency int `json:"run_concurrency" mapstructure:"run_concurrency" yaml:"run_concurrency"`

	// GroveDatabase is the name of a grove.DB registered in the DI container.
	// When set, the extension resolves this named database and auto-constructs
	// the appropriate store based on the driver type (pg/sqlite/mongo).
	// When empty and WithGroveDatabase was called, the default (unnamed) DB is used.
	GroveDatabase string `json:"grove_database" mapstructure:"grove_database" yaml:"grove_database"`

	// RequireConfig requires config to be present in YAML files.
	// If true and no config is found, Register returns an error.
	RequireConfig bool `json:"-" yaml:"-"`
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
