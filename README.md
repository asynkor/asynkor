<p align="center">
  <a href="https://asynkor.com">
    <img src="media/asynkor-logo.png?v=2" width="160" alt="Asynkor" />
  </a>
</p>

<p align="center">
  <strong>File leasing for AI agent teams.</strong><br />
  One MCP server. Any IDE. Zero merge conflicts.
</p>

<p align="center">
  <a href="https://www.npmjs.com/package/@asynkor/mcp"><img src="https://img.shields.io/npm/v/@asynkor/mcp?style=flat-square&color=0D7C72" alt="npm" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue?style=flat-square" alt="License" /></a>
  <a href="https://github.com/asynkor-com/asynkor/stargazers"><img src="https://img.shields.io/github/stars/asynkor-com/asynkor?style=flat-square" alt="Stars" /></a>
  <a href="https://discord.gg/asynkor"><img src="https://img.shields.io/discord/0?label=discord&style=flat-square&color=5865F2" alt="Discord" /></a>
</p>

<p align="center">
  <a href="https://asynkor.com/docs">Documentation</a> |
  <a href="https://asynkor.com">Website</a> |
  <a href="#quickstart">Quickstart</a> |
  <a href="#self-hosting">Self-hosting</a>
</p>

---

## The problem

Your team runs multiple AI agents in parallel — Claude Code, Cursor, Windsurf, Codex — across different machines. They rewrite each other's files, duplicate each other's work, and forget what the team decided yesterday.

Git catches conflicts at merge time. Asynkor prevents them at edit time.

## How it works

Agents connect to one shared MCP server. When an agent starts work, it **leases** the files it plans to touch. Other agents see the lease and wait. When the first agent finishes, it releases the lease and uploads a **snapshot** of the file content. The next agent gets the snapshot, writes it locally, and edits on top — no `git pull` needed, no merge conflicts.

```
Agent A                          Asynkor                          Agent B
  │                                │                                │
  ├─ asynkor_start(paths=[api.ts]) │                                │
  │  ◄── lease acquired ──────────►│                                │
  │                                │◄── asynkor_start(paths=[api.ts])
  │                                │──► BLOCKED: api.ts leased      │
  │  ... editing api.ts ...        │                                │
  │                                │     ... works on other files ..│
  ├─ asynkor_finish(snapshots)     │                                │
  │  ◄── leases released ────────►│                                │
  │                                │◄── asynkor_lease_wait(api.ts)  │
  │                                │──► acquired + file snapshot    │
  │                                │     writes snapshot to disk    │
  │                                │     edits on top of A's work   │
  │                                │                                │
  │         Both commit. Zero conflicts.                            │
```

## Features

- **File leasing** — atomic Redis locks with 5-minute TTL. One agent edits a file at a time. Others wait automatically.
- **Cross-machine file sync** — file content flows through the server. Two devs on separate laptops, zero merge conflicts.
- **Parking and handoffs** — save work mid-session. Another agent resumes exactly where you left off via `handoff_id`.
- **Overlap detection** — path-level and plan-text similarity. Catches conflicts before work begins, not at merge time.
- **Compounding team memory** — architectural decisions, gotchas, conventions captured by agents and inherited by every future session.
- **Protected zones** — mark sensitive code areas as warn, confirm, or block. Agents get guardrails automatically.
- **Live dashboard** — real-time view of active agents, file leases, parked work, conflicts, and activity.
- **Any IDE** — standard MCP protocol. Works with Claude Code, Cursor, Windsurf, VS Code, JetBrains, Zed, Codex CLI, and anything that supports MCP.

## Quickstart

Two commands. Works with any MCP-compatible agent.

```bash
# 1. Initialize in your project (prompts for API key)
npx @asynkor/mcp init

# 2. Register the MCP server with your agent (Claude Code example)
claude mcp add asynkor -- npx @asynkor/mcp start
```

Restart your editor. From the next session, every agent on the team shares one brain.

<details>
<summary><strong>Other IDEs</strong></summary>

Add to your agent's MCP config:

```json
{
  "mcpServers": {
    "asynkor": {
      "command": "npx",
      "args": ["-y", "@asynkor/mcp", "start"],
      "env": { "ASYNKOR_API_KEY": "your_key_here" }
    }
  }
}
```

Works with Cursor, Windsurf, VS Code (Copilot), JetBrains, Zed, Codex CLI, and any MCP-compatible agent.

</details>

## MCP Tools

| Tool | Purpose |
|------|---------|
| `asynkor_briefing` | Get team state: active work, leases, parked sessions, memory, follow-ups |
| `asynkor_start` | Declare work + acquire file leases |
| `asynkor_check` | Check rules, zones, leases for specific paths |
| `asynkor_remember` | Save knowledge to the team brain |
| `asynkor_finish` | Complete work, release leases, upload file snapshots |
| `asynkor_park` | Pause work for another agent to resume |
| `asynkor_lease_acquire` | Lease additional files mid-work |
| `asynkor_lease_wait` | Wait for blocked files (25s, retryable) |
| `asynkor_cancel` | Clean up stale/orphaned work |

## Architecture

```
Agents (Claude Code, Cursor, Windsurf, Codex)
        │
        │  stdio (MCP protocol)
        ▼
┌─────────────────────────────────┐
│  @asynkor/mcp (TypeScript)      │  ← npm package, runs locally
│  Local MCP proxy                │
└────────────────┬────────────────┘
                 │  HTTP + SSE
                 ▼
        ┌───────────────────┐
        │  asynkor-mcp (Go) │  ← this repo
        │  Coordination     │
        └──┬─────┬─────┬────┘
           │     │     │
        Redis  NATS  HTTP → Backend API
        (leases, (pub/sub)  (auth, teams,
         work,              persistence)
         sync)
```

**Go MCP Server** — real-time coordination: file leasing (atomic Lua scripts), work tracking, overlap detection, team memory distribution, file snapshot sync.

**TypeScript Client** — local proxy that bridges stdio (what IDEs speak) to HTTP+SSE (what the server speaks). Installed per-developer via npm.

**Redis** — the coordination spine. Leases, active work, sessions, file snapshots. All operations use atomic Lua scripts to prevent race conditions.

## Self-hosting

Run the full stack with Docker Compose:

```bash
git clone https://github.com/asynkor-com/asynkor.git
cd asynkor
cp .env.example .env  # edit with your values
docker compose up -d
```

Services: Go MCP server, Redis, NATS. The server is stateless — Redis holds all coordination state.

See [self-hosting docs](https://asynkor.com/docs#self-host-overview) for production deployment with TLS, backups, and monitoring.

## Documentation

Full docs at [asynkor.com/docs](https://asynkor.com/docs):

- [Introduction](https://asynkor.com/docs#introduction)
- [Quickstart](https://asynkor.com/docs#quickstart)
- [MCP Tools Reference](https://asynkor.com/docs#asynkor-briefing)
- [IDE Integrations](https://asynkor.com/docs#claude-code)
- [Team Setup](https://asynkor.com/docs#team-setup)
- [Self-hosting](https://asynkor.com/docs#self-host-overview)

## Community

- [Discord](https://discord.gg/asynkor) — questions, feedback, feature requests
- [GitHub Issues](https://github.com/asynkor-com/asynkor/issues) — bug reports
- [X / Twitter](https://x.com/asynkor) — updates

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and guidelines.

## License

[Apache 2.0](LICENSE)
