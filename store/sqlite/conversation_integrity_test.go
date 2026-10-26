package sqlite

import (
	"context"
	"testing"

	"github.com/xraph/cortex/store/storetest"
)

func TestConversationIntegrity(t *testing.T) {
	s := newTestStore(t)
	storetest.ConversationIntegrity(t, s, func(ctx context.Context, agentID, content string) error {
		_, err := s.sdb.Exec(ctx, "UPDATE cortex_memories SET content = ? WHERE agent_id = ? AND kind = 'conversation'", content, agentID)
		return err
	})
}
