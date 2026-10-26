package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type authority struct{ denied bool }

func (a *authority) Authorize(context.Context, opt.Action, opt.Resource) error {
	if a.denied {
		return opt.ErrPolicyDenied
	}
	return nil
}
func (*authority) Resolve(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}
func (*authority) Current(context.Context) (opt.IdentityRef, error) {
	return opt.IdentityRef{Subject: "fixture"}, nil
}
func (*authority) Execute(context.Context, opt.WorkItem) (opt.Transition, error) {
	return opt.Transition{}, opt.ErrUnavailable
}

func TestReadAdapters(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	auth := &authority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Authorizer: auth, Caller: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "transport")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if _, err = service.Create(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	reader := operator.Reader{Optimizer: service}
	key := bundle.Experiment.ID.String()
	cases := []struct {
		name string
		read func(operator.Reader, context.Context) error
	}{
		{"experiments", func(r operator.Reader, c context.Context) error {
			p, e := r.Experiments(c, operator.ExperimentsInput{})
			if e == nil && (len(p.Items) != 1 || !p.Complete) {
				t.Fatalf("experiment page: %+v", p)
			}
			return e
		}},
		{"experiment", func(r operator.Reader, c context.Context) error {
			_, e := r.Experiment(c, operator.IDInput{ID: key})
			return e
		}},
		{"candidates", func(r operator.Reader, c context.Context) error {
			_, e := r.Candidates(c, operator.ExperimentPageInput{ID: key})
			return e
		}},
		{"trials", func(r operator.Reader, c context.Context) error {
			_, e := r.Trials(c, operator.TrialsInput{ID: key})
			return e
		}},
		{"comparison", func(r operator.Reader, c context.Context) error {
			_, e := r.Comparison(c, operator.ExperimentPageInput{ID: key})
			return e
		}},
		{"versions", func(r operator.Reader, c context.Context) error {
			_, e := r.Versions(c, operator.VersionsInput{AgentID: bundle.Experiment.Spec.AgentID.String()})
			return e
		}},
		{"version", func(r operator.Reader, c context.Context) error {
			_, e := r.Version(c, operator.IDInput{ID: bundle.Versions[0].ID.String()})
			return e
		}},
		{"lessons", func(r operator.Reader, c context.Context) error {
			_, e := r.Lessons(c, operator.LessonsInput{})
			return e
		}},
		{"publications", func(r operator.Reader, c context.Context) error {
			_, e := r.Publications(c, operator.PublicationsInput{ExperimentID: key})
			return e
		}},
		{"activations", func(r operator.Reader, c context.Context) error {
			_, e := r.Activations(c, operator.ActivationsInput{AgentID: bundle.Experiment.Spec.AgentID.String()})
			return e
		}},
		{"events", func(r operator.Reader, c context.Context) error {
			_, e := r.Events(c, operator.ExperimentPageInput{ID: key})
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if e := tc.read(reader, ctx); e != nil {
				t.Fatal(e)
			}
			if e := tc.read(operator.Reader{}, ctx); !errors.Is(e, opt.ErrUnavailable) {
				t.Fatalf("unconfigured: %v", e)
			}
			if e := tc.read(reader, context.Background()); !errors.Is(e, cortex.ErrNoScope) {
				t.Fatalf("scope: %v", e)
			}
			auth.denied = true
			if e := tc.read(reader, ctx); !errors.Is(e, opt.ErrPolicyDenied) {
				t.Fatalf("denied: %v", e)
			}
			auth.denied = false
		})
	}
	for _, raw := range []string{"", "malformed", id.New(opt.PrefixTrial).String()} {
		if _, err = reader.Experiment(ctx, operator.IDInput{ID: raw}); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("invalid experiment %q: %v", raw, err)
		}
	}
	if _, err = reader.Trials(ctx, operator.TrialsInput{ID: key, VersionID: key}); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("wrong version kind: %v", err)
	}
	if _, err = reader.Versions(ctx, operator.VersionsInput{AgentID: key}); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("wrong agent kind: %v", err)
	}
	if _, err = reader.Experiments(ctx, operator.ExperimentsInput{PageInput: operator.PageInput{Limit: 101}}); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("page cap: %v", err)
	}
	if _, err = reader.Candidate(ctx, operator.ChildInput{ExperimentID: key, ID: id.New(opt.PrefixCandidate).String()}); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("missing candidate: %v", err)
	}
	if _, err = reader.Trial(ctx, operator.ChildInput{ExperimentID: key, ID: id.New(opt.PrefixTrial).String()}); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("missing trial: %v", err)
	}
	if _, err = reader.Lesson(ctx, operator.IDInput{ID: id.New(opt.PrefixLesson).String()}); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("missing lesson: %v", err)
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
	if _, err = reader.Experiment(foreign, operator.IDInput{ID: key}); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("foreign detail: %v", err)
	}
	var input operator.ExperimentsInput
	if err = json.Unmarshal([]byte(`{"limit":1,"state":"draft"}`), &input); err != nil {
		t.Fatal(err)
	}
	page, err := reader.Experiments(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["complete"] != true || decoded["total"] != nil {
		t.Fatalf("page contract: %s", encoded)
	}
}
