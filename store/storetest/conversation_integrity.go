package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store"
)

// ConversationIntegrity checks that malformed persisted messages cannot silently
// disappear from context. Corrupt changes this agent's conversation content only.
func ConversationIntegrity(t *testing.T, st store.Store, corrupt func(context.Context, string, string) error) {
	t.Helper()
	for name, content := range map[string]string{"syntax": "{", "null": "null", "wrong_type": `{"role":42}`, "missing_role": `{"content":"private corrupt row"}`} {
		t.Run(name, func(t *testing.T) {
			ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "conversation-integrity"}}})
			ag := &agent.Config{ID: id.NewAgentID(), Name: "history-" + name}
			require.NoError(t, st.Create(ctx, ag))
			sid := id.NewSessionID()
			require.NoError(t, st.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
			require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: "constraint"}}))
			require.NoError(t, corrupt(ctx, ag.ID.String(), content))
			require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: "valid recent message"}}))
			messages, err := st.LoadConversation(ctx, ag.ID, sid, 100)
			require.ErrorIs(t, err, cortex.ErrConversationUnavailable)
			require.Nil(t, messages, "a partial conversation must not be returned")
			require.NotContains(t, err.Error(), "private")
			messages, err = st.LoadConversation(ctx, ag.ID, sid, 1)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Equal(t, "valid recent message", messages[0].Content)
		})
	}
}
