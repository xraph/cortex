package engine_test

import (
	"context"
	"log"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// ExampleWithA2A wires two agents that can talk to each other.
//
// There is no Output comment, so this compiles with the package and is
// never executed: it needs a real model behind it to do anything. What it
// is here for is to stop the wiring in the docs from drifting away from
// the wiring the code actually accepts.
func ExampleWithA2A() {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "tenant", Value: "acme"}},
	})

	drv := sqlitedriver.New()
	if err := drv.Open(ctx, "cortex.db"); err != nil {
		log.Fatal(err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		log.Fatal(err)
	}
	st := sqlitestore.New(db)
	if migrateErr := st.Migrate(ctx); migrateErr != nil {
		log.Fatal(migrateErr)
	}

	eng, err := engine.New(
		engine.WithStore(st),
		// Messaging is opt-in. Without this, the three tools below never
		// appear in any agent's tool list.
		engine.WithA2A(a2a.Options{
			HopCeiling:     8,
			Workers:        4,
			DefaultReplyBy: 5 * time.Minute,
		}),
	)
	if err != nil {
		log.Fatal(err)
	}

	// Start brings the dispatcher up: it carries queued messages, runs the
	// agents they are addressed to, and resumes whoever was waiting. It
	// also redrives anything a previous process queued and never carried.
	if startErr := eng.Start(ctx); startErr != nil {
		log.Fatal(startErr)
	}
	defer func() { _ = eng.Stop(ctx) }()

	for _, cfg := range []*agent.Config{
		{
			Name:  "planner",
			Model: "gpt-4o",
			SystemPrompt: "You plan work and delegate. When you need something you cannot " +
				"determine yourself, use agent_ask to ask the specialist who can.",
			MaxSteps: 10,
		},
		{
			Name:         "db-expert",
			Model:        "gpt-4o",
			SystemPrompt: "You answer questions about the production database.",
			MaxSteps:     10,
		},
	} {
		if createErr := eng.CreateAgent(ctx, cfg); createErr != nil {
			log.Fatal(createErr)
		}
	}

	// The planner's model can now call agent_ask("db-expert", ...). That
	// run suspends on a row in the database rather than a goroutine, the
	// db-expert runs, and its answer comes back as the planner's tool
	// result. If this process dies in the middle, the next one redrives it.
	r, err := eng.RunAgent(ctx, "planner", "Is the orders table safe to migrate tonight?", nil)
	if err != nil {
		log.Fatal(err)
	}
	log.Println(r.State, r.Output)
}

// ExampleEngine_SendMessage answers an agent that asked a question.
//
// This is the path behind POST /v1/agents/:name/messages: the reply
// carries the waiting ask's reply-with token, so posting it resumes the
// run behind it exactly as a peer's answer would have.
func ExampleEngine_SendMessage() {
	var (
		eng       *engine.Engine
		ctx       context.Context
		replyWith string // read from the pending ask, or from the ask message
	)

	_, err := eng.SendMessage(ctx, a2a.SendParams{
		Sender:       a2a.Address{Agent: "on-call-human"},
		Receivers:    []a2a.Address{{Agent: "planner"}},
		Performative: a2a.Inform,
		Content:      "Yes, the migration window is approved for 02:00 UTC.",
		InReplyTo:    replyWith,
	})
	if err != nil {
		log.Fatal(err)
	}
}
