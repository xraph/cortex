package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestPublicationRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "publication.db")
	storetest.PublicationRecovery(t, func(t *testing.T, now func() time.Time) store.Store { return openTestStore(t, path, now) })
}
