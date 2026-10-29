# AGENTS.md

Cortex is an agent orchestration engine for Go. You compose agents from skills, traits, behaviors and personas, run them through a ReAct loop with tools, memory, checkpoints and suspensions, and coordinate several of them with orchestration strategies and agent-to-agent messaging. The root module is the engine on its own. Anything that drags in a heavy dependency (forge, fabriq, gRPC, the MCP SDK) lives in a module of its own.

| Module | Directory | Tagged by release |
|---|---|---|
| `github.com/xraph/cortex` | `.` | `vX.Y.Z` |
| `github.com/xraph/cortex/api` | `api` | `api/vX.Y.Z` |
| `github.com/xraph/cortex/sentinel` | `sentinel` | `sentinel/vX.Y.Z` |
| `github.com/xraph/cortex/extension` | `extension` | `extension/vX.Y.Z` |
| `github.com/xraph/cortex/integrations/fabriq` | `integrations/fabriq` | `integrations/fabriq/vX.Y.Z` |
| `github.com/xraph/cortex/integrations/sandbox` | `integrations/sandbox` | `integrations/sandbox/vX.Y.Z` (first release pending) |
| `github.com/xraph/cortex/optimization` | `optimization` | `optimization/vX.Y.Z` |
| `github.com/xraph/cortex/mcpbridge` | `mcpbridge` | `mcpbridge/vX.Y.Z` |
| `github.com/xraph/cortex/a2aremote` | `a2aremote` | no |
| `github.com/xraph/cortex/a2aremote/grpcbind` | `a2aremote/grpcbind` | no |
| `github.com/xraph/cortex/inttest` | `inttest` | no, never published |

Every sub-module requires the root (and any sibling it uses) at a fixed version and carries a `replace github.com/xraph/cortex => ../` (or `../../`). `extension` also replaces `cortex/sentinel => ../sentinel`, and `grpcbind` replaces `cortex/a2aremote => ../`. Those replaces are intentional. Two requires name versions that were never tagged, `grpcbind`'s require of `a2aremote` and `inttest`'s pseudo-version of `integrations/fabriq`, and that only works because neither module is published.

## Commands

All modules declare Go 1.26.0. Build with `GOWORK=off`, which is what CI sees.

```bash
GOWORK=off go build ./...
make test              # go test -v ./...  (root module only)
make test-race         # go test -race -v ./...
make test-coverage     # race + atomic coverage, 30-minute package timeout
make vet               # go vet ./...      (root module only)
make lint              # golangci-lint in every module, inttest with -tags integration
make lint-fix
make fmt               # gofmt -s -w . then goimports -local github.com/xraph/cortex
make test-integration  # cd inttest && go test -tags=integration -p 1 -timeout 30m ./...
```

`make lint` is the one target that walks every `go.mod`. It runs `GOWORK=off golangci-lint run --allow-serial-runners --max-same-issues 0 ./...` inside each module and fails if any of them fails. Everything else in the Makefile works on the root module only, so you test a sub-module from its own directory:

```bash
cd extension && GOWORK=off go test -race -count=1 ./...
```

`make build`, `make run`, `make install`, `make dev` and `make all` need `./cmd/cortex`, and `cmd/` is empty, so they fail. `go build ./...` is the build.

`./updatemod.sh` runs `go mod tidy` in every directory with a `go.mod`.

### What CI runs

`.github/workflows/ci.yml` runs on pushes and pull requests to `main`:

| Job | Scope | What it runs |
|---|---|---|
| `ci` (shared `xraph/workflows` `go-ci.yml@v1`, Go 1.26) | root | `make test`, `make test-coverage` (race + atomic coverage, 30-minute package timeout), golangci-lint at the version pinned there, `gofmt -l .`, `make vet`, a `go mod tidy` diff, gosec and govulncheck (both gating), and a check that nested modules require cortex at versions the proxy can serve |
| `format` | whole tree | `goimports -l -local github.com/xraph/cortex .` must print nothing |
| `submodules` | `api`, `sentinel`, `extension`, `integrations/fabriq`, `integrations/sandbox`, `optimization`, `mcpbridge`, `a2aremote`, `a2aremote/grpcbind` | `go mod tidy` diff, `go build ./...`, `go test -race -count=1 ./...` |
| `docs` | `docs/` | `pnpm install --frozen-lockfile`, `pnpm types:check`, `pnpm lint`, `pnpm build` |

`a2aremote` and `a2aremote/grpcbind` now run in the sub-module CI matrix. `inttest` is still outside that matrix. Run `make test-integration` when you change it, and run `make lint` locally for every module.

### Tests and Docker

The SQLite store tests always run. The Postgres and Mongo conformance, migration and rescope tests in `store/postgres` and `store/mongo` start their own containers through testcontainers-go, and each one calls `testcontainers.SkipIfProviderIsNotHealthy` first. Without Docker they skip and the package still reports `ok`, so run with `-v` if you need to know whether they ran. CI has Docker, so there they run for real.

The Mongo test fixture is pinned to MongoDB 7.0.41 by image digest. The previous
8.3.2 image refuses to start on Linux kernels >=6.19, including current hosted
CI runners. You can test another version explicitly:

```bash
CORTEX_TEST_MONGO_IMAGE=mongo:7 GOWORK=off go test -v ./store/mongo/
```

Passing the pinned fixture does not qualify other MongoDB versions.

`inttest/` holds the end-to-end fabriq test, behind the `integration` build tag. It starts a Postgres container through `fabriqtest.StartPostgres` and fails, not skips, without Docker. Run it with `make test-integration`.

`store/scopespy`'s completeness test calls every `store.Store` method through a nil embedded interface and recovers the panic. On at least one local macOS and Go combination that call spins until the test times out instead of panicking. If a full local run hangs there, leave that package out (`go list ./... | grep -v /store/scopespy`) and trust Linux CI for it.

## Layout

| Path | What lives there |
|---|---|
| `*.go` (root) | package `cortex`: sentinel errors, `Scope` and its context helpers, `Config`, `Entity`, the `ID` alias, `ToolAuthorizer`, rescoping, `AgentRunner` |
| `id/` | TypeID identifiers and prefixes |
| `agent/`, `skill/`, `trait/`, `behavior/`, `persona/`, `prompt/` | what an agent is made of; each package owns its `Store` interface |
| `run/`, `session/`, `memory/`, `checkpoint/`, `suspension/` | execution state |
| `engine/` | the coordinator: ReAct loop, tools, sessions, suspend and resume, sweeper, orchestration and a2a wiring |
| `orchestration/` | sequential, parallel, router, hierarchical, debate and blackboard strategies |
| `a2a/` | in-process agent-to-agent bus, conversations, contract net |
| `llm/`, `safety/`, `knowledge/` | interfaces, with `llm/nexus`, `safety/shield` and `knowledge/weave` adapters that discover their service from the vessel container |
| `cognitive/`, `communication/`, `perception/` | value objects for how an agent thinks, talks and perceives |
| `plugin/` | lifecycle hook interfaces and the registry |
| `audit_hook/`, `observability/` | audit trail and metrics hooks |
| `store/` | composite `store.Store`, backends `postgres`, `sqlite`, `mongo`, the `storetest` conformance suite and the `scopespy` test double |
| `api/` | REST handlers (own module) |
| `sentinel/` | Sentinel evaluation adapter (own module) |
| `extension/` | Forge extension and its dashboard contract in `extension/contract/` (own module) |
| `integrations/fabriq/` | package `fabriqbrain`: fabriq as a `knowledge.Provider`, extra tools and a learning-loop plugin (own module) |
| `a2aremote/`, `a2aremote/grpcbind/` | Agent2Agent over JSON-RPC, REST and SSE, and the gRPC binding |
| `inttest/` | end-to-end tests |
| `scripts/pin-submodules.sh` | the release's sub-module pinning step |
| `docs/` | Fumadocs site (`make docs`, `make docs-build`) |
| `MIGRATION.md` | the dashboard move from Go templates to `extension/contract` and the React plugin |

## Conventions

### Scope comes first

Every stored entity belongs to a host-defined `cortex.Scope`, an ordered list of `Level{Key, Value}` that you put on the context with `cortex.WithScope`. Store methods read it back with `cortex.ScopeFromContext` and return `cortex.ErrNoScope` when it's zero, so a scope dropped somewhere upstream fails loudly and never turns into a query across every host. Level order decides which indexed column each level lands in, so a host can't reorder its levels between releases. Every store gives the first three levels their own columns and filters reads by those alone. Deeper levels are written to `scope_extra` (JSONB on Postgres, a JSON string on SQLite, a map on MongoDB) and to `scope_canon`, but no read matches on them, so scopes that differ only below the third level see each other's rows. List filters match by prefix: a workspace scope reaches every project under it. `Scope.Covers` asks the upward question.

When you add a store method, write the scope predicate in all three backends and add a case to `store/storetest`. The engine-side call has to carry a scope too, or `scopespy` will catch it.

### Errors

Sentinels live in `errors.go` with a `cortex: ` prefix. Wrap with `%w` and the same prefix (`fmt.Errorf("cortex: get agent: %w", err)`). Backends translate a missing row into the entity's `Err...NotFound`, so callers branch with `errors.Is`. Suspension errors (`ErrNotSuspended`, `ErrSuspensionExpired`, `ErrResultsMismatch`, `ErrInvalidContinuation`) are matched on by callers, so don't reuse one for a different condition.

### IDs

`id.ID` wraps a TypeID. Prefixes include `agt`, `arun`, `astp`, `tcall`, `skl`, `trt`, `bhv`, `prs`, `ses`, `sus`, `orch`, `orchcfg`, `msg`, `conv` and `dlv`. The full list is in `id/id.go`.

### Stores

`store.Store` embeds every subsystem's store interface plus `Migrate(ctx, ...cortex.MigrateOption)`, `Ping` and `Close`. Backends build on a `*grove.DB`. Migrations are registered in Go in each backend's `migrations.go`. The `.sql` files under `store/postgres/migrations/` aren't loaded by anything.

### Engine, plugins and the Forge extension

`engine.New(opts ...engine.Option)` takes `func(*Engine) error` options such as `WithStore`, `WithLLM`, `WithSafety`, `WithKnowledge`, `WithTool`, `WithToolAuthorizer` and `WithLogger`. The engine logs through `github.com/xraph/go-utils/log` and defaults to `log.NewNoopLogger()`; the Forge extension passes the app's logger. Log a failure with `e.logError` or `e.logWarn` (engine/logging.go): they log the error's type, never its text, and throttle each message to a line a minute. Fields are IDs and counts, never run content or scope values.

Lifecycle hooks live in `plugin/`, one interface per event, and you register a hook with `engine.WithExtension`.

`extension.New(opts ...ExtOption)` embeds `*forge.BaseExtension`. `Register` loads config from `extensions.cortex` or `cortex`, takes a store from the named grove database or any `*grove.DB` in the container, adds the nexus, shield, weave and sentinel `EngineOption`s (each is a no-op when its service isn't registered), builds the engine and provides `*engine.Engine`. `Start` runs migrations unless `DisableMigrate` is set. The extension mounts no HTTP routes by itself: the REST handlers are in the `api` module, and you pass them in with `extension.WithRoutes`, under `/cortex` by default.

### Generated code and lint

`a2aremote/grpcbind/a2apb/*.pb.go` come from `a2a.proto` through `a2apb/generate.sh`. Don't hand-edit either; the proto is vendored from the A2A spec.

`.golangci.yml` is a golangci-lint v2 config. `errcheck` runs with `check-blank` and `check-type-assertions`, `govet` enables everything but `fieldalignment` (so shadowing fails), revive's `exported` rule wants doc comments, `nolintlint` flags a stale `//nolint`, and test files are exempt from gosec, errcheck and gocritic. Imports go in three groups: standard library, third party, then `github.com/xraph/cortex/...`.

Commits use conventional commits, usually with a scope: `fix(engine): ...`, `feat(dashboard): ...`, `build(deps): ...`. The version headings in `CHANGELOG.md` don't follow the module's tags, so read `git tag` for the real version.

## Dependencies

See each `go.mod` for versions.

- The root depends on `github.com/xraph/nexus`, `shield`, `weave`, `grove` (with `mongodriver`, `pgdriver` and `sqlitedriver`), `go-utils` and `vessel`. It doesn't depend on forge.
- `api` adds `github.com/xraph/forge`.
- `sentinel` depends on `github.com/xraph/sentinel`.
- `extension` adds forge, and pulls in `github.com/xraph/sentinel` through `cortex/sentinel`.
- `integrations/fabriq` depends on `github.com/xraph/fabriq/core`.
- `inttest` pulls in `github.com/xraph/fabriq`, and trove through it.

Nothing in forgery consumes cortex, so it releases last.

To bump a dependency, `go get` each xraph module at its release in every module that requires it directly, then tidy them all. Weave, for example, is a direct require of both the root and `extension`:

```bash
GOWORK=off go get github.com/xraph/weave@vX.Y.Z
(cd extension && GOWORK=off go get github.com/xraph/weave@vX.Y.Z)
GOWORK=off ./updatemod.sh
GOWORK=off make lint
```

Grove tags its drivers at the same version as its root, so move them together. Never commit a `go.work` or a `replace` that points at a sibling repo. `.gitignore` doesn't cover `go.work`, so look at `git status` before you commit. Dependabot only watches the root `go.mod`.

## Releasing

You release by dispatching the workflow. Never push a tag by hand.

1. Release and pin whatever cortex needs upstream first: nexus, shield, weave, grove, sentinel, fabriq/core or forge.
2. Wait for CI on `main` to go green: `gh run list --workflow ci.yml --branch main --limit 1`.
3. `gh workflow run release.yml --ref main -f tag=vX.Y.Z`

`.github/workflows/release.yml` runs these steps:

1. It tags `main` with the input as `github-actions[bot]` and pushes the tag. Nothing checks the input before that push (`pin-submodules.sh` validates it two steps later), so type the leading `v`.
2. It verifies the root with `go mod download`, a `go mod tidy` diff, `go build ./...` and `go test -race -count=1 ./...`, then does the same in `api`, `sentinel`, `extension`, `integrations/fabriq`, `integrations/sandbox`, `optimization` and `mcpbridge`.
3. It runs `scripts/pin-submodules.sh vX.Y.Z`. The script validates the version, then uses `go mod edit -require` to point every `github.com/xraph/cortex...` require in those seven `go.mod` files at the new tag, and it fails the run if it finds a cortex require still at `v0.0.0` (a `v0.0.0-...` pseudo-version counts).
4. It commits the seven `go.mod` files as `chore: pin sub-modules to vX.Y.Z`, tags that commit `api/vX.Y.Z`, `sentinel/vX.Y.Z`, `extension/vX.Y.Z`, `integrations/fabriq/vX.Y.Z`, `integrations/sandbox/vX.Y.Z`, `optimization/vX.Y.Z` and `mcpbridge/vX.Y.Z` (skipping any that already exist), pushes the tags and runs `git reset --hard HEAD~1`.
5. In a throwaway module it runs `go get github.com/xraph/cortex/<mod>@vX.Y.Z` against `proxy.golang.org` for each of the seven, so a tag that can't resolve fails the run.
6. It publishes a GitHub release with notes from `git log` since the previous tag.

The pin commit is never pushed to a branch, and only the sub-module tags reach it. So `main` keeps each sub-module's requires at an earlier release with `replace => ../` for local work, while a consumer who fetches `cortex/extension@vX.Y.Z` resolves the pinned requires from the tag, which means a sub-module tag and the root tag of the same version point at different commits: the root tag at the `main` commit, the sub-module tags at the pin commit on top of it.

`a2aremote`, `a2aremote/grpcbind` and `inttest` are never tagged.

The root tag is pushed before anything is verified. If a later step fails, `vX.Y.Z` exists without some or all of its sub-module tags, and a re-run with the same input stops at `git tag`, so fix the cause and release the next patch version.

## Branch rules

`main` has one ruleset, and it blocks deletion and force-pushes. There are no required checks or reviews, so direct pushes to `main` go through, and CI runs after the push.
