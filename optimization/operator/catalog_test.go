package operator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store/memory"
)

func TestCatalogAdapterPreservesSourceAuthorization(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "catalog"}}}
	revoked := false
	catalog, err := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: []opt.CatalogChoice{{Scope: scope, Kind: opt.CatalogEvaluation, Ref: opt.ContentRef{Ref: "approved-plan", Revision: "1", Digest: strings.Repeat("a", 64)}, Name: "Plan"}}, Authorize: func(context.Context, opt.CatalogChoice) error {
		if revoked {
			return opt.ErrPolicyDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	auth := &authority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	reader := operator.Reader{Optimizer: service}
	ctx := cortex.WithScope(context.Background(), scope)
	page, err := reader.Catalog(ctx, operator.CatalogInput{Kind: opt.CatalogEvaluation})
	if err != nil || len(page.Items) != 1 || !page.Complete {
		t.Fatalf("catalog adapter: %+v %v", page, err)
	}
	revoked = true
	page, err = reader.Catalog(ctx, operator.CatalogInput{Kind: opt.CatalogEvaluation})
	if err != nil || len(page.Items) != 0 || !page.Complete {
		t.Fatalf("mapped permission ignored: %+v %v", page, err)
	}
	if _, err = reader.Catalog(ctx, operator.CatalogInput{Kind: "invalid"}); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("invalid kind: %v", err)
	}
	if _, err = (operator.Reader{}).Catalog(ctx, operator.CatalogInput{}); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("missing optimizer: %v", err)
	}
	auth.denied = true
	if _, err = reader.Catalog(ctx, operator.CatalogInput{}); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("current domain denial: %v", err)
	}
}
