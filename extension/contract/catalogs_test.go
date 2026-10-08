package contract

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	"github.com/xraph/nexus"
	"github.com/xraph/nexus/provider"
	shieldengine "github.com/xraph/shield/engine"
	shieldid "github.com/xraph/shield/id"
	"github.com/xraph/shield/profile"
	"github.com/xraph/shield/scan"
	shieldsqlite "github.com/xraph/shield/store/sqlite"
	"github.com/xraph/weave/collection"
	weaveengine "github.com/xraph/weave/engine"
	weaveid "github.com/xraph/weave/id"
	weavesqlite "github.com/xraph/weave/store/sqlite"
)

func catalogDB(t *testing.T) *grove.DB {
	t.Helper()
	drv := sqlitedriver.New()
	if err := drv.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db")); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type catalogProvider struct {
	provider.Provider
	err error
}

func (catalogProvider) Name() string                        { return "test-provider" }
func (catalogProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (p catalogProvider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "test-model", Name: "Test model", Provider: "test-provider", Pricing: provider.Pricing{InputPerMillion: 0.125, OutputPerMillion: 0.25}}}, p.err
}
func TestNexusCatalogKeepsDecimalPriceStringsAndProviderFailures(t *testing.T) {
	access := func(context.Context, string) (CatalogScope, error) {
		return CatalogScope{AppID: "app", TenantID: "tenant"}, nil
	}
	catalogs := NexusCatalogs(nexus.New(nexus.WithProvider(catalogProvider{})), access)
	value, err := catalogs["models.list"](context.Background(), map[string]any{"provider": "test-provider"})
	if err != nil {
		t.Fatal(err)
	}
	row := value.(map[string]any)["items"].([]map[string]any)[0]
	pricing := row["pricing"].(map[string]any)
	if pricing["input_per_million"] != "0.125" || pricing["output_per_million"] != "0.25" {
		t.Fatalf("prices: %+v", pricing)
	}
	catalogs = NexusCatalogs(nexus.New(nexus.WithProvider(catalogProvider{err: errors.New("provider unavailable")})), access)
	if _, err := catalogs["models.list"](context.Background(), nil); err == nil {
		t.Fatal("provider failure hidden")
	}
}

func TestCatalogsRequireExplicitHostScope(t *testing.T) {
	for _, access := range []CatalogAccess{nil, func(context.Context, string) (CatalogScope, error) { return CatalogScope{AppID: "app"}, nil }} {
		for _, catalogs := range []map[string]CatalogQuery{NexusCatalogs(nil, access), WeaveCatalogs(nil, access), ShieldCatalogs(nil, access)} {
			for name, query := range catalogs {
				if _, err := query(context.Background(), nil); !errors.Is(err, dc.ErrPermissionDenied) {
					t.Fatalf("%s: %v", name, err)
				}
			}
		}
	}
}

func TestWeaveCatalogWalksAllPagesAndChecksDetailBeforeStats(t *testing.T) {
	ctx := context.Background()
	db := catalogDB(t)
	st := weavesqlite.New(db)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	eng, err := weaveengine.New(weaveengine.WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	foreign := weaveid.NewCollectionID()
	for i := 0; i < 125; i++ {
		row := &collection.Collection{ID: weaveid.NewCollectionID(), Name: fmt.Sprintf("knowledge-%03d", i), AppID: "app", TenantID: "tenant"}
		if i == 0 {
			row.ID = foreign
			row.TenantID = "foreign"
		}
		if createErr := st.CreateCollection(ctx, row); createErr != nil {
			t.Fatal(createErr)
		}
	}
	access := func(context.Context, string) (CatalogScope, error) {
		return CatalogScope{AppID: "app", TenantID: "tenant"}, nil
	}
	catalogs := WeaveCatalogs(eng, access)
	value, err := catalogs["knowledge.list"](ctx, map[string]any{"limit": 25, "offset": 100})
	if err != nil {
		t.Fatal(err)
	}
	out := value.(map[string]any)
	if out["total"] != 124 || len(out["items"].([]map[string]any)) != 24 || out["complete"] != true {
		t.Fatalf("page: %+v", out)
	}
	if summary := out["summary"].(map[string]int64); summary["collections"] != 124 {
		t.Fatalf("summary must count all authorized collections: %+v", summary)
	}
	if _, err := catalogs["knowledge.detail"](ctx, map[string]any{"id": foreign.String()}); !errors.Is(err, dc.ErrNotFound) {
		t.Fatalf("foreign detail: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogs["knowledge.list"](ctx, nil); err == nil {
		t.Fatal("storage failure was hidden")
	}
}

func TestShieldCatalogCountsAllScopedScansBeforeRunFilter(t *testing.T) {
	ctx := context.Background()
	st := shieldsqlite.New(catalogDB(t))
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	eng, err := shieldengine.New(shieldengine.WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 125; i++ {
		row := &scan.Result{ID: shieldid.NewScanID(), AppID: "app", TenantID: "tenant", Direction: scan.DirectionInput, Decision: scan.DecisionBlock, Blocked: true, Metadata: map[string]any{"run_id": "run-a"}}
		if i == 0 {
			row.TenantID = "foreign"
		}
		if i == 1 {
			row.Metadata["run_id"] = "run-b"
		}
		if createErr := st.CreateScan(ctx, row); createErr != nil {
			t.Fatal(createErr)
		}
	}
	for _, tenant := range []string{"tenant", "foreign"} {
		if createErr := st.CreateProfile(ctx, &profile.SafetyProfile{ID: shieldid.NewSafetyProfileID(), Name: tenant, AppID: "app", TenantID: tenant, Enabled: true}); createErr != nil {
			t.Fatal(createErr)
		}
	}
	catalogs := ShieldCatalogs(eng, func(context.Context, string) (CatalogScope, error) {
		return CatalogScope{AppID: "app", TenantID: "tenant"}, nil
	})
	value, err := catalogs["safety.scans"](ctx, map[string]any{"run_id": "run-a", "limit": 25, "offset": 100})
	if err != nil {
		t.Fatal(err)
	}
	out := value.(map[string]any)
	if out["total"] != 123 || len(out["items"].([]map[string]any)) != 23 || out["summary"].(map[string]int64)["blocked"] != 123 {
		t.Fatalf("scans: %+v", out)
	}
	value, err = catalogs["safety.profiles"](ctx, nil)
	if err != nil || value.(map[string]any)["total"] != 1 {
		t.Fatalf("profiles: %v %v", value, err)
	}
}
