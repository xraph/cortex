package optimization

import (
	"context"

	"github.com/xraph/cortex/id"
)

// Repository owns exactly scoped persistence. Transaction implementations must
// publish all changes together and return detached resource values.
type Repository interface {
	Transact(context.Context, func(Transaction) error) error
	GetExperiment(context.Context, id.ID) (Experiment, error)
	GetVersion(context.Context, id.ID) (HarnessVersion, error)
	Migrate(context.Context) error
	Ping(context.Context) error
	Close() error
}

// Transaction is valid only during its synchronous callback. Callers must not
// retain it or use it from concurrent goroutines.
type Transaction interface {
	CreateExperiment(ExperimentBundle) error
}

// ExperimentBundle atomically creates a draft and its frozen version content.
type ExperimentBundle struct {
	Experiment Experiment       `json:"experiment"`
	Versions   []HarnessVersion `json:"versions"`
}
