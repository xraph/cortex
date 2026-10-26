package contract

import (
	"context"
	"net/http"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/optimization/operator"
)

func (s *service) bindOptimization(d *dispatcher.Dispatcher) error {
	reader := operator.Reader{Optimizer: s.deps.Optimization}
	binds := []func() error{
		func() error { return optimizationQuery(d, s, "optimization.catalog", reader.Catalog) },
		func() error { return s.bindOptimizationCapabilities(d, reader) },
		func() error { return optimizationQuery(d, s, "optimization.experiments.list", reader.Experiments) },
		func() error { return optimizationQuery(d, s, "optimization.experiments.detail", reader.Experiment) },
		func() error { return optimizationQuery(d, s, "optimization.candidates.list", reader.Candidates) },
		func() error { return optimizationQuery(d, s, "optimization.candidates.detail", reader.Candidate) },
		func() error { return optimizationQuery(d, s, "optimization.trials.list", reader.Trials) },
		func() error { return optimizationQuery(d, s, "optimization.trials.detail", reader.Trial) },
		func() error { return optimizationQuery(d, s, "optimization.comparison", reader.Comparison) },
		func() error { return optimizationQuery(d, s, "optimization.events.list", reader.Events) },
		func() error { return optimizationQuery(d, s, "optimization.versions.list", reader.Versions) },
		func() error { return optimizationQuery(d, s, "optimization.versions.detail", reader.Version) },
		func() error { return optimizationQuery(d, s, "optimization.lessons.list", reader.Lessons) },
		func() error { return optimizationQuery(d, s, "optimization.lessons.detail", reader.Lesson) },
		func() error { return optimizationQuery(d, s, "optimization.publications.list", reader.Publications) },
		func() error { return optimizationQuery(d, s, "optimization.activations.list", reader.Activations) },
	}
	for _, bind := range binds {
		if err := bind(); err != nil {
			return err
		}
	}
	return s.bindOptimizationCommands(d)
}

func optimizationQuery[I, O any](d *dispatcher.Dispatcher, s *service, name string, read func(context.Context, I) (O, error)) error {
	return queryWithPermission(d, s, name, "optimization.read", func(ctx context.Context, in I) (O, error) {
		out, err := read(ctx, in)
		return out, optimizationError(err)
	})
}

func optimizationError(err error) error {
	if err == nil {
		return nil
	}
	failure := operator.Failure(err)
	code := dc.CodeUnavailable
	switch failure.Status {
	case http.StatusBadRequest:
		code = dc.CodeBadRequest
	case http.StatusForbidden:
		code = dc.CodePermissionDenied
	case http.StatusNotFound:
		code = dc.CodeNotFound
	case http.StatusConflict:
		code = dc.CodeConflict
	}
	return &dc.Error{Code: code, Message: failure.Message, Details: map[string]any{"optimization_code": failure.Code}}
}
