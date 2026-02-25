// Package extension adapts the Cortex engine as a Forge extension.
//
// It implements the forge.Extension interface to integrate Cortex
// into a Forge application with automatic dependency discovery,
// route registration, and lifecycle management.
//
// Configuration can be provided programmatically via ExtOption functions
// or via YAML configuration files under "extensions.cortex" or "cortex" keys.
package extension

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/xraph/forge"
	"github.com/xraph/grove"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/api"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/store"
	mongostore "github.com/xraph/cortex/store/mongo"
	pgstore "github.com/xraph/cortex/store/postgres"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// ExtensionName is the name registered with Forge.
const ExtensionName = "cortex"

// ExtensionDescription is the human-readable description.
const ExtensionDescription = "Human-emulating AI agent orchestration"

// ExtensionVersion is the semantic version.
const ExtensionVersion = "0.1.0"

// Ensure Extension implements forge.Extension at compile time.
var _ forge.Extension = (*Extension)(nil)

// Extension adapts Cortex as a Forge extension.
type Extension struct {
	*forge.BaseExtension

	config     Config
	eng        *engine.Engine
	apiHandler *api.API
	engineOpts []engine.Option
	useGrove   bool
}

// New creates a Cortex Forge extension with the given options.
func New(opts ...ExtOption) *Extension {
	e := &Extension{
		BaseExtension: forge.NewBaseExtension(ExtensionName, ExtensionVersion, ExtensionDescription),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Engine returns the underlying Cortex engine.
// This is nil until Register is called.
func (e *Extension) Engine() *engine.Engine { return e.eng }

// API returns the API handler.
func (e *Extension) API() *api.API { return e.apiHandler }

// Register implements [forge.Extension]. It loads configuration,
// initializes the engine, and registers it in the DI container.
func (e *Extension) Register(fapp forge.App) error {
	if err := e.BaseExtension.Register(fapp); err != nil {
		return err
	}

	if err := e.loadConfiguration(); err != nil {
		return err
	}

	if err := e.init(fapp); err != nil {
		return err
	}

	return vessel.Provide(fapp.Container(), func() (*engine.Engine, error) {
		return e.eng, nil
	})
}

// init builds the engine and API handler.
func (e *Extension) init(fapp forge.App) error {
	// Resolve store from grove DI if configured.
	if e.useGrove {
		groveDB, err := e.resolveGroveDB(fapp)
		if err != nil {
			return fmt.Errorf("cortex: %w", err)
		}
		s, err := e.buildStoreFromGroveDB(groveDB)
		if err != nil {
			return err
		}
		e.engineOpts = append(e.engineOpts, engine.WithStore(s))
	}

	eng, err := engine.New(e.engineOpts...)
	if err != nil {
		return fmt.Errorf("cortex: create engine: %w", err)
	}
	e.eng = eng

	e.apiHandler = api.New(e.eng, fapp.Router())

	if !e.config.DisableRoutes {
		if err := e.apiHandler.RegisterRoutes(fapp.Router()); err != nil {
			return fmt.Errorf("cortex: register routes: %w", err)
		}
	}

	return nil
}

// Start begins the Cortex engine and runs auto-migration if enabled.
func (e *Extension) Start(ctx context.Context) error {
	if e.eng == nil {
		return errors.New("cortex: extension not initialized")
	}

	if !e.config.DisableMigrate {
		s := e.eng.Store()
		if s != nil {
			if err := s.Migrate(ctx); err != nil {
				return fmt.Errorf("cortex: migration failed: %w", err)
			}
		}
	}

	if err := e.eng.Start(ctx); err != nil {
		return err
	}

	e.MarkStarted()
	return nil
}

// Stop gracefully shuts down the Cortex engine.
func (e *Extension) Stop(ctx context.Context) error {
	if e.eng != nil {
		if err := e.eng.Stop(ctx); err != nil {
			e.MarkStopped()
			return err
		}
	}
	e.MarkStopped()
	return nil
}

// Health implements [forge.Extension].
func (e *Extension) Health(ctx context.Context) error {
	if e.eng == nil {
		return errors.New("cortex: extension not initialized")
	}

	s := e.eng.Store()
	if s == nil {
		return errors.New("cortex: no store configured")
	}

	return s.Ping(ctx)
}

// Handler returns the HTTP handler for all API routes.
// Convenience for standalone use outside Forge.
func (e *Extension) Handler() http.Handler {
	if e.apiHandler == nil {
		return http.NotFoundHandler()
	}
	return e.apiHandler.Handler()
}

// RegisterRoutes registers all Cortex API routes into a Forge router.
func (e *Extension) RegisterRoutes(router forge.Router) error {
	if e.apiHandler != nil {
		return e.apiHandler.RegisterRoutes(router)
	}
	return nil
}

// --- Config Loading (mirrors grove extension pattern) ---

// loadConfiguration loads config from YAML files or programmatic sources.
func (e *Extension) loadConfiguration() error {
	programmaticConfig := e.config

	fileConfig, configLoaded := e.tryLoadFromConfigFile()

	if !configLoaded {
		if programmaticConfig.RequireConfig {
			return errors.New("cortex: configuration is required but not found in config files; " +
				"ensure 'extensions.cortex' or 'cortex' key exists in your config")
		}

		e.config = e.mergeWithDefaults(programmaticConfig)
	} else {
		e.config = e.mergeConfigurations(fileConfig, programmaticConfig)
	}

	// Enable grove resolution if YAML config specifies a grove database.
	if e.config.GroveDatabase != "" {
		e.useGrove = true
	}

	e.Logger().Debug("cortex: configuration loaded",
		forge.F("disable_routes", e.config.DisableRoutes),
		forge.F("disable_migrate", e.config.DisableMigrate),
		forge.F("base_path", e.config.BasePath),
		forge.F("grove_database", e.config.GroveDatabase),
		forge.F("default_model", e.config.DefaultModel),
		forge.F("run_concurrency", e.config.RunConcurrency),
	)

	return nil
}

// tryLoadFromConfigFile attempts to load config from YAML files.
func (e *Extension) tryLoadFromConfigFile() (Config, bool) {
	cm := e.App().Config()
	var cfg Config

	// Try "extensions.cortex" first (namespaced pattern).
	if cm.IsSet("extensions.cortex") {
		if err := cm.Bind("extensions.cortex", &cfg); err == nil {
			e.Logger().Debug("cortex: loaded config from file",
				forge.F("key", "extensions.cortex"),
			)
			return cfg, true
		}
		e.Logger().Warn("cortex: failed to bind extensions.cortex config",
			forge.F("error", "bind failed"),
		)
	}

	// Try legacy "cortex" key.
	if cm.IsSet("cortex") {
		if err := cm.Bind("cortex", &cfg); err == nil {
			e.Logger().Debug("cortex: loaded config from file",
				forge.F("key", "cortex"),
			)
			return cfg, true
		}
		e.Logger().Warn("cortex: failed to bind cortex config",
			forge.F("error", "bind failed"),
		)
	}

	return Config{}, false
}

// mergeWithDefaults fills zero-valued fields with defaults.
func (e *Extension) mergeWithDefaults(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = defaults.DefaultModel
	}
	if cfg.DefaultMaxSteps == 0 {
		cfg.DefaultMaxSteps = defaults.DefaultMaxSteps
	}
	if cfg.DefaultMaxTokens == 0 {
		cfg.DefaultMaxTokens = defaults.DefaultMaxTokens
	}
	if cfg.DefaultTemperature == 0 {
		cfg.DefaultTemperature = defaults.DefaultTemperature
	}
	if cfg.DefaultReasoningLoop == "" {
		cfg.DefaultReasoningLoop = defaults.DefaultReasoningLoop
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = defaults.ShutdownTimeout
	}
	if cfg.RunConcurrency == 0 {
		cfg.RunConcurrency = defaults.RunConcurrency
	}
	return cfg
}

// mergeConfigurations merges YAML config with programmatic options.
// YAML config takes precedence for most fields; programmatic bool flags fill gaps.
func (e *Extension) mergeConfigurations(yamlConfig, programmaticConfig Config) Config {
	// Programmatic bool flags override when true.
	if programmaticConfig.DisableRoutes {
		yamlConfig.DisableRoutes = true
	}
	if programmaticConfig.DisableMigrate {
		yamlConfig.DisableMigrate = true
	}

	// String fields: YAML takes precedence.
	if yamlConfig.BasePath == "" && programmaticConfig.BasePath != "" {
		yamlConfig.BasePath = programmaticConfig.BasePath
	}
	if yamlConfig.GroveDatabase == "" && programmaticConfig.GroveDatabase != "" {
		yamlConfig.GroveDatabase = programmaticConfig.GroveDatabase
	}
	if yamlConfig.DefaultModel == "" && programmaticConfig.DefaultModel != "" {
		yamlConfig.DefaultModel = programmaticConfig.DefaultModel
	}
	if yamlConfig.DefaultReasoningLoop == "" && programmaticConfig.DefaultReasoningLoop != "" {
		yamlConfig.DefaultReasoningLoop = programmaticConfig.DefaultReasoningLoop
	}

	// Numeric fields: YAML takes precedence, programmatic fills gaps.
	if yamlConfig.DefaultMaxSteps == 0 && programmaticConfig.DefaultMaxSteps != 0 {
		yamlConfig.DefaultMaxSteps = programmaticConfig.DefaultMaxSteps
	}
	if yamlConfig.DefaultMaxTokens == 0 && programmaticConfig.DefaultMaxTokens != 0 {
		yamlConfig.DefaultMaxTokens = programmaticConfig.DefaultMaxTokens
	}
	if yamlConfig.DefaultTemperature == 0 && programmaticConfig.DefaultTemperature != 0 {
		yamlConfig.DefaultTemperature = programmaticConfig.DefaultTemperature
	}
	if yamlConfig.ShutdownTimeout == 0 && programmaticConfig.ShutdownTimeout != 0 {
		yamlConfig.ShutdownTimeout = programmaticConfig.ShutdownTimeout
	}
	if yamlConfig.RunConcurrency == 0 && programmaticConfig.RunConcurrency != 0 {
		yamlConfig.RunConcurrency = programmaticConfig.RunConcurrency
	}

	// Fill remaining zeros with defaults.
	return e.mergeWithDefaults(yamlConfig)
}

// resolveGroveDB resolves a *grove.DB from the DI container.
// If GroveDatabase is set, it looks up the named DB; otherwise it uses the default.
func (e *Extension) resolveGroveDB(fapp forge.App) (*grove.DB, error) {
	if e.config.GroveDatabase != "" {
		db, err := vessel.InjectNamed[*grove.DB](fapp.Container(), e.config.GroveDatabase)
		if err != nil {
			return nil, fmt.Errorf("grove database %q not found in container: %w", e.config.GroveDatabase, err)
		}
		return db, nil
	}
	db, err := vessel.Inject[*grove.DB](fapp.Container())
	if err != nil {
		return nil, fmt.Errorf("default grove database not found in container: %w", err)
	}
	return db, nil
}

// buildStoreFromGroveDB constructs the appropriate store backend
// based on the grove driver type (pg, sqlite, mongo).
func (e *Extension) buildStoreFromGroveDB(db *grove.DB) (store.Store, error) {
	driverName := db.Driver().Name()
	switch driverName {
	case "pg":
		return pgstore.New(db), nil
	case "sqlite":
		return sqlitestore.New(db), nil
	case "mongo":
		return mongostore.New(db), nil
	default:
		return nil, fmt.Errorf("cortex: unsupported grove driver %q", driverName)
	}
}
