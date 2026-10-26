package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex/store/storetest"
)

func TestCurrentTaskAcceptanceRead(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine", tcpostgres.WithDatabase("planning_operator"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	driver := pgdriver.New()
	require.NoError(t, driver.Open(ctx, dsn))
	db, err := grove.Open(driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	storage := New(db)
	require.NoError(t, storage.Migrate(ctx))
	storetest.CurrentTaskAcceptanceRead(t, storage)
}
