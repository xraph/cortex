# Cortex

**Human-emulating AI agent orchestration for Go.**

Cortex is a Go framework for building AI agents with human-like traits. Instead of treating agents as stateless prompt-response machines, Cortex models them with skills, personality traits, behaviors, cognitive styles, and personas — creating agents that reason and communicate like real people.

## Features

- **Human Model** — Skills, Traits, Behaviors, Cognitive Styles, Communication Styles, Perception, and Personas
- **Execution Tracking** — Runs, Steps, and Tool Calls with full observability
- **Memory** — Conversation history, working memory, and summaries per agent per tenant
- **Checkpoints** — Human-in-the-loop approval gates that pause runs for review
- **Plugin System** — 16 lifecycle hooks with type-cached dispatch (zero-cost for unimplemented hooks)
- **Multi-Tenancy** — Context-based tenant and app isolation across all operations
- **36 REST Endpoints** — Full CRUD for all entities, agent execution, streaming, and tools
- **Forge Integration** — First-class extension for the Forge application framework
- **TypeID Identifiers** — 12 type-prefixed, UUIDv7-based, K-sortable IDs

## Quick Start

```go
package main

import (
    "context"
    "log"

    "github.com/xraph/cortex"
    "github.com/xraph/cortex/agent"
    "github.com/xraph/cortex/engine"
    "github.com/xraph/cortex/store/memory"
)

func main() {
    ctx := context.Background()
    ctx = cortex.WithTenant(ctx, "acme-corp")
    ctx = cortex.WithApp(ctx, "my-app")

    // Create engine with in-memory store
    eng, err := engine.New(
        engine.WithStore(memory.New()),
    )
    if err != nil {
        log.Fatal(err)
    }

    // Create an agent
    err = eng.CreateAgent(ctx, &agent.Config{
        AppID:        "my-app",
        Name:         "assistant",
        SystemPrompt: "You are a helpful assistant.",
        Model:        "gpt-4o",
        MaxSteps:     10,
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

## Architecture

```
cortex (root)           — Config, context helpers, errors, Entity base type
├── engine              — Central coordinator, CRUD passthroughs
├── agent               — Agent configuration, flat mode & persona mode
├── skill               — Skills, tool bindings, knowledge refs, proficiency levels
├── trait               — Personality traits, bipolar dimensions, influences
├── behavior            — Reactive behaviors, triggers, actions
├── persona             — Persona composition (skills + traits + behaviors + styles)
├── cognitive           — Cognitive processing styles, phases, strategies
├── communication       — Communication styles (tone, formality, verbosity)
├── perception          — Attention filters, context windows
├── run                 — Run/Step/ToolCall tracking, state machine
├── memory              — Conversation, working memory, summaries
├── checkpoint          — Human-in-the-loop approval gates
├── id                  — 12 TypeID types (agt_, skl_, trt_, bhv_, prs_, arun_, ...)
├── store               — Composite store interface (8 sub-interfaces, 50 methods)
│   ├── postgres        — Production PostgreSQL store (bun ORM, embedded migrations)
│   └── memory          — In-memory store for testing
├── plugin              — Extension system, 16 hook interfaces, Registry
├── observability       — Prometheus-compatible metrics (11 counters)
├── audit_hook          — Structured audit trail (18 actions, 8 resources)
├── api                 — 36 REST endpoints with Forge-style handlers
└── extension           — Forge integration extension
```

## The Human Model

Cortex agents are built from composable human-model primitives:

| Concept | Analogy | Purpose |
|---------|---------|---------|
| **Skill** | What you can do | Tools, knowledge, proficiency levels |
| **Trait** | Who you are | Bipolar personality dimensions, influences |
| **Behavior** | What you habitually do | Trigger-action patterns, priorities |
| **Cognitive Style** | How you think | Phase chains, strategies, transitions |
| **Communication Style** | How you speak | Tone, formality, verbosity, adaptation |
| **Perception** | What you notice | Attention filters, context windows |
| **Persona** | Your complete identity | Composition of all the above |

## Packages

| Package | Description |
|---------|-------------|
| `github.com/xraph/cortex` | Root — config, context helpers, errors |
| `github.com/xraph/cortex/engine` | Central engine coordinator |
| `github.com/xraph/cortex/agent` | Agent configuration |
| `github.com/xraph/cortex/skill` | Skills and tool bindings |
| `github.com/xraph/cortex/trait` | Personality traits |
| `github.com/xraph/cortex/behavior` | Reactive behaviors |
| `github.com/xraph/cortex/persona` | Persona composition |
| `github.com/xraph/cortex/cognitive` | Cognitive styles |
| `github.com/xraph/cortex/communication` | Communication styles |
| `github.com/xraph/cortex/perception` | Perception models |
| `github.com/xraph/cortex/run` | Run/Step/ToolCall tracking |
| `github.com/xraph/cortex/memory` | Conversation memory |
| `github.com/xraph/cortex/checkpoint` | Human-in-the-loop checkpoints |
| `github.com/xraph/cortex/id` | TypeID identifiers |
| `github.com/xraph/cortex/store` | Composite store interface |
| `github.com/xraph/cortex/store/postgres` | PostgreSQL store |
| `github.com/xraph/cortex/store/memory` | In-memory store |
| `github.com/xraph/cortex/plugin` | Plugin system |
| `github.com/xraph/cortex/observability` | Metrics extension |
| `github.com/xraph/cortex/audit_hook` | Audit trail extension |
| `github.com/xraph/cortex/api` | HTTP API (36 routes) |
| `github.com/xraph/cortex/extension` | Forge integration |

## Forge Integration

```go
import (
    "github.com/xraph/forge"
    "github.com/xraph/cortex/extension"
    pgstore "github.com/xraph/cortex/store/postgres"
)

app := forge.New()
app.RegisterExtension(extension.New(
    extension.WithStore(pgstore.New(bunDB)),
))
app.Run()
```

This gives you auto-migration, 36 REST endpoints, health checks, and graceful shutdown out of the box.

## API

All endpoints are under `/cortex`:

| Resource | Routes | Operations |
|----------|--------|------------|
| Agents | 7 | CRUD + run + stream |
| Runs | 3 | List, get, cancel |
| Skills | 5 | CRUD |
| Traits | 5 | CRUD |
| Behaviors | 5 | CRUD |
| Personas | 5 | CRUD |
| Checkpoints | 2 | List pending, resolve |
| Memory | 2 | Get/clear conversation |
| Tools | 2 | List tools, get schema |

## Documentation

Full documentation is in the `docs/` directory, built with Fumadocs and Next.js.

```bash
cd docs && pnpm install && pnpm dev
```

## License

See [LICENSE](LICENSE) for details.
