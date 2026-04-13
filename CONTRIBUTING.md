# Contributing to Asynkor

Thanks for your interest in contributing. This guide covers the development setup and how to submit changes.

## Development setup

### Prerequisites

- Go 1.23+
- Node.js 20+
- Docker (for Redis + NATS in development)
- A running Redis instance (or `docker compose up redis nats`)

### Clone and build

```bash
git clone https://github.com/asynkor-com/asynkor.git
cd asynkor

# Go MCP server
cd mcp
go build ./...
go test ./...

# TypeScript client
cd ../client
npm install
npm run build
```

### Running locally

```bash
# Start dependencies
docker compose up -d redis nats

# Run the MCP server
cd mcp
REDIS_URL=localhost:6379 NATS_URL=nats://localhost:4222 JAVA_URL=http://localhost:3001 PORT=4000 go run .

# In another terminal, run the client proxy
cd client
ASYNKOR_SERVER_URL=http://localhost:4000 ASYNKOR_API_KEY=your_key npm start
```

### Running tests

```bash
# Go tests (uses miniredis, no external dependencies)
cd mcp && go test ./...

# TypeScript
cd client && npm test
```

## Submitting changes

1. Fork the repo
2. Create a branch: `git checkout -b my-feature`
3. Make your changes
4. Run tests: `go test ./...`
5. Commit with a clear message
6. Open a PR against `main`

### Commit messages

Follow conventional commits:

```
feat: add lease refresh on heartbeat
fix: prevent race in WaitAndAcquire partial release
docs: update MCP tools reference
```

### Code style

- **Go**: `gofmt` + standard library conventions. Atomic Redis operations use Lua scripts.
- **TypeScript**: ESLint + Prettier defaults.

## Architecture overview

See the [architecture section](README.md#architecture) in the README. Key directories:

```
mcp/
  internal/
    mcpserver/   ← MCP tool handlers (start, finish, park, lease_wait, etc.)
    lease/       ← Redis lease operations (atomic Lua scripts)
    work/        ← Work lifecycle (start → active → completed/parked)
    session/     ← SSE session management
    auth/        ← API key validation (calls backend API)
    natsbus/     ← NATS pub/sub
    teamctx/     ← Team context cache (rules, zones, memories)
    activity/    ← Activity event feed

client/
  src/
    cli.ts       ← CLI entry point (init, start, login)
    proxy.ts     ← stdio ↔ HTTP+SSE bridge
    config.ts    ← Config file management
```

## Questions?

Open an issue or ask in [Discord](https://discord.gg/asynkor).
