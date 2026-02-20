// Package store defines the composite store interface for Cortex.
package store

import (
	"context"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

// Store is the composite persistence interface for all Cortex subsystems.
type Store interface {
	agent.Store
	run.Store
	memory.Store
	checkpoint.Store
	skill.Store
	trait.Store
	behavior.Store
	persona.Store

	Migrate(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}
