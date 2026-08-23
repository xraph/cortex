package mongo

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmongodb "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
)

// TestMigrate_backfillDefaultSessions proves the session backfill against
// a genuinely pre-v1.9.0 shape, not an empty database: documents are
// inserted directly, bypassing SaveConversation entirely (which always
// stamps a real session_id today). See the postgres migration
// 20260824000004's backfillDefaultSessions comment for the full
// reasoning behind the duplicate-content document and the
// distinct-(role,content) message_count it expects; this is the same
// scenario against mongo.
//
// Unlike postgres/sqlite, mongo's backfill has no versioned-migration
// gap to reproduce: Store.Migrate runs it unconditionally on every
// startup, and always AFTER rescopeLegacyRows within that same call (see
// store.go), so a document that only gets its scope_canon from THIS
// Migrate call is still eligible for the backfill in the same call --
// there is no "scope_canon != ” at Up time" boundary to fall on the
// wrong side of the way there is for postgres/sqlite. This test only
// exercises the already-scoped case, since that's the entire difference
// left to prove for this backend.
func TestMigrate_backfillDefaultSessions(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	// SaveConversation and this backfill both write inside a
	// multi-document transaction, which mongo only supports on a replica
	// set (or sharded cluster).
	mongoContainer, err := tcmongodb.Run(ctx, mongoConformanceImage, tcmongodb.WithReplicaSet("rs0"))
	if err != nil {
		t.Fatalf("start mongodb container: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := mongoContainer.Terminate(context.Background()); cleanupErr != nil {
			t.Logf("terminate mongodb container: %v", cleanupErr)
		}
	})

	uri, err := mongoContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("mongodb connection string: %v", err)
	}
	uri += "&directConnection=true"

	drv := mongodriver.New()
	if err = drv.Open(ctx, uri, mongodriver.WithDatabase("cortex_backfill_sessions")); err != nil {
		t.Fatalf("open mongo driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(db)
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	agentID := id.NewAgentID()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_legacy"}}}
	canon := scope.Canonical()

	// Two logically distinct messages, the first duplicated once with a
	// different Timestamp -- the shape engine.llmToMemory's
	// reload-then-resave round trip produced pre-daa7e44. Role+Content
	// dedup must land on 2 despite three physical documents.
	legacyDocs := []bson.M{
		{
			"_id": id.NewMemoryID().String(), "agent_id": agentID.String(), "session_id": "",
			"kind": "conversation", "content": `{"role":"user","content":"hello","timestamp":"2024-01-01T00:00:00Z"}`,
			"scope_l0": canon, "scope_l1": "", "scope_l2": "", "scope_canon": canon,
			"created_at": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			"_id": id.NewMemoryID().String(), "agent_id": agentID.String(), "session_id": "",
			"kind": "conversation", "content": `{"role":"assistant","content":"hi there","timestamp":"2024-01-01T00:00:01Z"}`,
			"scope_l0": canon, "scope_l1": "", "scope_l2": "", "scope_canon": canon,
			"created_at": time.Date(2024, 1, 1, 0, 0, 1, 0, time.UTC),
		},
		{
			"_id": id.NewMemoryID().String(), "agent_id": agentID.String(), "session_id": "",
			"kind": "conversation", "content": `{"role":"user","content":"hello","timestamp":"2024-01-02T00:00:00Z"}`,
			"scope_l0": canon, "scope_l1": "", "scope_l2": "", "scope_canon": canon,
			"created_at": time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, doc := range legacyDocs {
		if _, err = s.mdb.Collection(colMemories).InsertOne(ctx, doc); err != nil {
			t.Fatalf("insert legacy conversation document: %v", err)
		}
	}

	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate to run backfill: %v", err)
	}

	sessCtx := cortex.WithScope(ctx, scope)
	sessions, err := s.ListSessions(sessCtx, &session.ListFilter{AgentID: agentID, DefaultOnly: true})
	if err != nil {
		t.Fatalf("list sessions after backfill: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions after backfill = %d, want exactly 1 default session", len(sessions))
	}
	got := sessions[0]
	if !got.IsDefault {
		t.Errorf("backfilled session IsDefault = false, want true")
	}
	if got.Title != "Default" {
		t.Errorf("backfilled session Title = %q, want %q", got.Title, "Default")
	}
	if got.MessageCount != 2 {
		t.Errorf("backfilled session MessageCount = %d, want 2 (distinct role+content pairs, not the 3 physical documents)", got.MessageCount)
	}
	if got.LastMessage != "hello" {
		t.Errorf("backfilled session LastMessage = %q, want %q", got.LastMessage, "hello")
	}

	msgs, err := s.LoadConversation(sessCtx, agentID, got.ID, 0)
	if err != nil {
		t.Fatalf("load backfilled conversation: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("LoadConversation after backfill returned %d messages, want 3 (physical documents are never touched)", len(msgs))
	}

	// Re-running Migrate must be a clean no-op: the backfill's own filter
	// (session_id: "") has nothing left to find, and it must not create
	// a second default session or error on the partial unique index.
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("second re-migrate (idempotency check): %v", err)
	}
	again, err := s.ListSessions(sessCtx, &session.ListFilter{AgentID: agentID, DefaultOnly: true})
	if err != nil {
		t.Fatalf("list sessions after idempotent re-migrate: %v", err)
	}
	if len(again) != 1 || again[0].ID != got.ID {
		t.Fatalf("sessions after idempotent re-migrate = %v, want unchanged single session %s", again, got.ID)
	}
}
