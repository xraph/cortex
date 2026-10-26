package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestArtifactBoard(t *testing.T) { storetest.ArtifactBoard(t, newTestStore(t)) }

func TestArtifactPublication(t *testing.T) { storetest.ArtifactPublication(t, newTestStore(t)) }

func TestArtifactBoardTransactions(t *testing.T) {
	s := newTestStore(t)
	storetest.ArtifactBoardTransactions(t, s, assuranceDriver{store: s})
}
