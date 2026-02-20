// Package extension adapts the Cortex engine as a Forge extension.
package extension

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/xraph/forge"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/api"
	"github.com/xraph/cortex/engine"
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
	config     Config
	eng        *engine.Engine
	apiHandler *api.API
	logger     *slog.Logger
	engineOpts []engine.Option
}

// New creates a Cortex Forge extension with the given options.
func New(opts ...ExtOption) *Extension {
	e := &Extension{}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Name returns the extension name.
func (e *Extension) Name() string { return ExtensionName }

// Description returns the extension description.
func (e *Extension) Description() string { return ExtensionDescription }

// Version returns the extension version.
func (e *Extension) Version() string { return ExtensionVersion }

// Dependencies returns the list of extension names this extension depends on.
func (e *Extension) Dependencies() []string { return []string{} }

// Engine returns the underlying Cortex engine.
// This is nil until Register is called.
func (e *Extension) Engine() *engine.Engine { return e.eng }

// API returns the API handler.
func (e *Extension) API() *api.API { return e.apiHandler }

// Register implements [forge.Extension]. It initializes the engine,
// builds the API, and optionally registers HTTP routes.
func (e *Extension) Register(fapp forge.App) error {
	if err := e.init(fapp); err != nil {
		return err
	}

	if err := vessel.Provide(fapp.Container(), func() (*engine.Engine, error) {
		return e.eng, nil
	}); err != nil {
		return fmt.Errorf("cortex: register engine in container: %w", err)
	}

	return nil
}

// init builds the engine and API handler.
func (e *Extension) init(fapp forge.App) error {
	logger := e.logger
	if logger == nil {
		logger = slog.Default()
	}

	opts := make([]engine.Option, 0, len(e.engineOpts)+1)
	opts = append(opts, e.engineOpts...)
	opts = append(opts, engine.WithLogger(logger))

	eng, err := engine.New(opts...)
	if err != nil {
		return fmt.Errorf("cortex: create engine: %w", err)
	}
	e.eng = eng

	e.apiHandler = api.New(e.eng, fapp.Router())

	if !e.config.DisableRoutes {
		e.apiHandler.RegisterRoutes(fapp.Router())
	}

	return nil
}

// Start begins the Cortex engine and runs auto-migration if enabled.
func (e *Extension) Start(ctx context.Context) error {
	if e.eng == nil {
		return errors.New("cortex: extension not initialized")
	}

	if !e.config.DisableMigrate {
		store := e.eng.Store()
		if store != nil {
			if err := store.Migrate(ctx); err != nil {
				return fmt.Errorf("cortex: migration failed: %w", err)
			}
		}
	}

	return e.eng.Start(ctx)
}

// Stop gracefully shuts down the Cortex engine.
func (e *Extension) Stop(ctx context.Context) error {
	if e.eng == nil {
		return nil
	}
	return e.eng.Stop(ctx)
}

// Health implements [forge.Extension].
func (e *Extension) Health(ctx context.Context) error {
	if e.eng == nil {
		return errors.New("cortex: extension not initialized")
	}

	store := e.eng.Store()
	if store == nil {
		return errors.New("cortex: no store configured")
	}

	return store.Ping(ctx)
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
func (e *Extension) RegisterRoutes(router forge.Router) {
	if e.apiHandler != nil {
		e.apiHandler.RegisterRoutes(router)
	}
}
