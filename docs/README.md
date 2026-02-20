# Cortex Documentation

Documentation site for [Cortex](https://github.com/xraph/cortex) — human-emulating AI agent orchestration for Go.

Built with [Fumadocs](https://fumadocs.dev) and Next.js.

## Development

```bash
pnpm install
pnpm dev
```

Open http://localhost:3000 to preview.

## Structure

| Path | Description |
|------|-------------|
| `content/docs/` | MDX documentation pages |
| `content/docs/meta.json` | Top-level navigation |
| `app/(home)` | Landing page |
| `app/docs` | Documentation layout |
| `app/api/search/route.ts` | Search handler |

## Content Organization

| Section | Pages | Description |
|---------|-------|-------------|
| Introduction | 3 | Overview, getting started, architecture |
| Concepts | 5 | Identity, entities, configuration, errors, multi-tenancy |
| Human Model | 8 | Skills, traits, behaviors, cognitive/communication/perception styles, personas |
| Execution | 3 | Runs, memory, checkpoints |
| Infrastructure | 4 | PostgreSQL store, plugins, observability, audit |
| Guides | 4 | Full example, Forge extension, custom store, custom plugin |
| API Reference | 2 | HTTP API (36 routes), Go packages (22 packages) |
