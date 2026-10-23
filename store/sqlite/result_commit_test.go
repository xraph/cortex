package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

func TestVerifiedResultRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "result.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	s := openTestStore(t, dsn)
	storetest.VerifiedResultRestart(t, s, func() store.Store {
		require.NoError(t, s.Close())
		return openTestStore(t, dsn)
	})
}

func TestVerificationJobRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "jobs.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	s := openTestStore(t, dsn)
	storetest.VerificationJobRestart(t, s, func() store.Store {
		require.NoError(t, s.Close())
		return openTestStore(t, dsn)
	})
}
