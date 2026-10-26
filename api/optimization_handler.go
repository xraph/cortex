package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/xraph/forge"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func (a *API) registerOptimizationRoutes(router forge.Router) error {
	reader := operator.Reader{Optimizer: a.optimization}
	group := router.Group("/v1/optimization", forge.WithGroupTags("optimization"))
	routes := []struct {
		path, name             string
		handler, input, output any
	}{
		{"/capabilities", "optimizationCapabilities", optimizationRead(reader.Capabilities), operator.CapabilitiesInput{}, opt.CapabilitySnapshot{}},
		{"/experiments", "optimizationExperiments", optimizationRead(reader.Experiments), operator.ExperimentsInput{}, opt.ResourcePage[opt.Experiment]{}},
		{"/experiments/:id", "optimizationExperiment", optimizationRead(reader.Experiment), operator.IDInput{}, opt.Experiment{}},
		{"/experiments/:id/candidates", "optimizationCandidates", optimizationRead(reader.Candidates), operator.ExperimentPageInput{}, opt.ResourcePage[opt.Candidate]{}},
		{"/experiments/:experiment_id/candidates/:id", "optimizationCandidate", optimizationRead(reader.Candidate), operator.ChildInput{}, opt.Candidate{}},
		{"/experiments/:id/trials", "optimizationTrials", optimizationRead(reader.Trials), operator.TrialsInput{}, opt.ResourcePage[opt.Trial]{}},
		{"/experiments/:experiment_id/trials/:id", "optimizationTrial", optimizationRead(reader.Trial), operator.ChildInput{}, opt.Trial{}},
		{"/experiments/:id/comparison", "optimizationComparison", optimizationRead(reader.Comparison), operator.ExperimentPageInput{}, opt.ComparisonDetail{}},
		{"/experiments/:id/events", "optimizationEvents", optimizationRead(reader.Events), operator.ExperimentPageInput{}, opt.ResourcePage[opt.Event]{}},
		{"/versions", "optimizationVersions", optimizationRead(reader.Versions), operator.VersionsInput{}, opt.ResourcePage[opt.HarnessVersion]{}},
		{"/versions/:id", "optimizationVersion", optimizationRead(reader.Version), operator.IDInput{}, opt.HarnessVersion{}},
		{"/lessons", "optimizationLessons", optimizationRead(reader.Lessons), operator.LessonsInput{}, opt.ResourcePage[opt.Lesson]{}},
		{"/lessons/:id", "optimizationLesson", optimizationRead(reader.Lesson), operator.IDInput{}, opt.Lesson{}},
		{"/publications", "optimizationPublications", optimizationRead(reader.Publications), operator.PublicationsInput{}, opt.ResourcePage[opt.Publication]{}},
		{"/activations", "optimizationActivations", optimizationRead(reader.Activations), operator.ActivationsInput{}, opt.ResourcePage[opt.Activation]{}},
	}
	for _, route := range routes {
		if err := group.GET(route.path, route.handler, forge.WithOperationID(route.name), forge.WithRequestSchema(route.input), forge.WithResponseSchema(http.StatusOK, "Retained optimization records", route.output), forge.WithErrorResponses()); err != nil {
			return fmt.Errorf("register optimization route %s: %w", route.name, err)
		}
	}
	return a.registerOptimizationCommands(router)
}

func optimizationRead[I, O any](read func(context.Context, I) (O, error)) func(forge.Context, *I) (*O, error) {
	return func(ctx forge.Context, in *I) (*O, error) {
		ctx.SetHeader("Cache-Control", "no-store")
		values, err := url.ParseQuery(ctx.Request().URL.RawQuery)
		if err == nil {
			for _, value := range values {
				if len(value) != 1 {
					err = opt.ErrInvalidSpec
					break
				}
			}
		}
		if err != nil {
			return nil, replyOptimizationFailure(ctx, opt.ErrInvalidSpec)
		}
		out, err := read(ctx.Context(), *in)
		if err != nil {
			return nil, replyOptimizationFailure(ctx, err)
		}
		return &out, nil
	}
}
