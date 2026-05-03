# @asynkor/mcp

Real-time coordination for teams running AI coding agents.

This is the open-source MCP client for [Asynkor](https://asynkor.com). It ships the `asynkor` CLI and the stdio MCP shim your IDE's agent (Claude Code, Cursor, Windsurf, Zed, JetBrains, Copilot, Codex, Trae, Antigravity, or anything that speaks MCP) connects through.

- **License:** MIT
- **Repository:** https://github.com/asynkor/asynkor
- **Docs:** https://asynkor.com/docs
- **Dashboard:** https://asynkor.com

---

## What it does

When a team runs multiple AI coding agents in parallel on the same codebase, agents collide. Two sessions touch the same file. Branches diverge. Someone spends Friday untangling merge conflicts instead of shipping.

Asynkor is the coordination layer that prevents this. Every agent on the team connects to one MCP server. Before editing a file, the agent acquires an atomic file lease. Other agents trying the same path see the lock and wait. When an agent finishes, the next one inherits the latest file content and the team's accumulated memory — across any IDE, any laptop, any agent framework.

This package (`@asynkor/mcp`) is the piece that runs on your machine. It proxies between your IDE's agent and the Asynkor server over MCP, and it ships the setup CLI.

---

## Quickstart

One command. Auto-detects every IDE on the machine (Claude Code, Cursor, Windsurf, Zed, VS Code, Codex CLI) and wires Asynkor MCP into each one's user-scope config.

```bash
npx -y @asynkor/mcp login
```

That's it. The command opens your browser to sign in, saves the API key to `~/.asynkor/config.json`, and runs `asynkor install` automatically. Restart any open editor afterwards.

### Already have an API key?

```bash
ASYNKOR_API_KEY=cf_live_... npx -y @asynkor/mcp install
```

Same effect, no browser. Useful for CI, agents installing on behalf of a user, or pasting a key from the dashboard.

### Installed a new IDE later?

Re-run `asynkor install`. It picks up the new IDE and registers Asynkor in it. Existing IDEs see no change — the command is idempotent.

### Per-project config (optional)

If you want a project-scoped `.asynkor.json` + `CLAUDE.md` + per-IDE config inside one repo, use `init` instead:

```bash
cd your-project
ASYNKOR_API_KEY=cf_live_... npx @asynkor/mcp init
```

Most users want `login`/`install` (system-wide). Use `init` only when one project needs different team credentials than your default.

---

## MCP tool surface

Eighteen tools. Every agent connected to the server can call them. Once your IDE is wired up, all but `asynkor_context_update` are auto-approved by default (the long-term doc is owner-reviewed).

**Coordination + history**

| Tool | What it does |
|------|--------------|
| `asynkor_briefing` | Team state at session start — who is active, what was recently shipped, what follow-ups are open, applicable rules, memories, zones, context, and inbox top-3. |
| `asynkor_start` | Declare the work you're about to do. Acquires file leases on declared paths, runs overlap detection, can resume a parked session via `handoff_id` or pick up an open `followup_id`. |
| `asynkor_check` | Before an edit, check for active overlaps, applicable rules, protected zones, and relevant team memories on specific paths. Read-only. |
| `asynkor_lease_acquire` | Acquire leases on additional paths mid-session (paths not declared in your initial `asynkor_start`). |
| `asynkor_lease_wait` | Block up to 25–30s waiting for a leased path to free. Returns `still_blocked` if the lease holder outlasts the window so you can work on other files and retry. |
| `asynkor_remember` / `asynkor_forget` | Save / drop short-term staging memories. Promote durable ones to long-term context. |
| `asynkor_finish` | Complete work. Uploads result, learnings, decisions, file snapshots (critical for cross-machine handoffs), follow-ups. Releases leases. |
| `asynkor_park` | Save incomplete work for another agent to resume. Stores progress, notes, learnings, decisions. Returns a `handoff_id` that appears in the next briefing. |
| `asynkor_cancel` | Clean up stale or orphaned work (disconnected sessions holding leases). Requires a `work_id` from the briefing. |
| `asynkor_context` / `asynkor_context_update` | Read / atomically rewrite the long-term project doc. |
| `asynkor_switch_team` | Switch the active team for a user-scoped API key, or confirm the current team. |

**Agent comms (v0.2 — async messaging between agents)**

| Tool | What it does |
|------|--------------|
| `asynkor_inspect` | Read-only snapshot of one teammate's live work — plan, planned paths, files touched, learnings, decisions, parked notes, and the file leases they hold. |
| `asynkor_ask` | Open an async thread targeting `work:<id>` (a specific session), `host:<name>` (a developer), or `team` (broadcast). Recipient sees it on their next briefing. |
| `asynkor_inbox` | List threads addressed to me — my work, my host, or team broadcasts. |
| `asynkor_thread` | Read the full transcript of one thread. |
| `asynkor_reply` | Append a reply. Pass `close: "true"` when the question is fully answered. |

Full reference with inputs, outputs, and examples: https://asynkor.com/docs

---

## The coordination model in one paragraph

Every edit starts with a lease — acquired atomically through a Redis Lua script so the check-and-set is single-threaded across the whole team. When an agent finishes, it uploads file snapshots (content-addressed) along with learnings and decisions. The next agent — on any machine — inherits both the latest file content and the team's accumulated memory. Sessions that can't complete hand off a full context package to the next agent via `asynkor_park`. Team memory (`asynkor_remember`) compounds across sessions; zones mark paths that need elevated confirmation.

---

## Supported IDEs

`asynkor init --ide <name>` writes the appropriate MCP config. Anything that speaks MCP works — the list below is just the IDEs we've validated.

| IDE | `--ide` flag | Notes |
|-----|--------------|-------|
| Claude Code | _auto-detected_ | First-class: `.claude/settings.json` + auto-approval + slash commands |
| Cursor | `cursor` | Writes `.cursor/mcp.json` + `.cursorrules` |
| Windsurf | `windsurf` | Writes `~/.codeium/windsurf/mcp_config.json` + `.windsurfrules` |
| Zed | `zed` | Writes `~/.config/zed/settings.json` |
| VS Code / Copilot | `vscode` | Writes `.vscode/mcp.json` |
| JetBrains (IntelliJ / WebStorm / PyCharm / Junie) | `jetbrains` | Writes `.junie/mcp.json` |
| OpenAI Codex CLI | `codex` | Writes `~/.codex/config.toml` |
| Trae | `trae` | Writes `.trae/mcp.json` |
| Google Antigravity | `antigravity` | Writes `.antigravity/mcp_config.json` |

---

## CLI reference

The `init` command registers the package locally as the `asynkor` binary, so after the first run you can invoke it without `npx`.

```
asynkor start                        Run the MCP proxy (what your IDE calls)
asynkor init                         Write .asynkor.json + IDE config
asynkor init --ide <name>            Target a specific IDE
asynkor init --link <url>            Set up from a join-link URL
asynkor setup <url>                  Alias for `init --link`
asynkor login                        Browser-based login — stores API key
asynkor status                       Print the current team briefing

asynkor teams                        List configured teams
asynkor teams create <slug>          Create a team on the backend
asynkor teams add                    Add an existing team to local config
asynkor teams remove <slug>          Remove a team from local config
asynkor teams switch <slug>          Set the active team

asynkor invite <email>               Email a team invite (admin role optional)
asynkor invite link                  Generate a shareable join-link URL
asynkor keys list                    List API keys for the active team
asynkor keys create                  Create a new API key (shown once)
asynkor keys revoke <keyId>          Revoke an API key

asynkor help                         Print this.
```

Full help with flags: `asynkor help`.

---

## Environment variables

| Variable | Purpose |
|----------|---------|
| `ASYNKOR_API_KEY` | API key. Used by `start` and by `init` for non-interactive setup. |
| `ASYNKOR_SERVER_URL` | Override the MCP server URL (default `https://mcp.asynkor.com`). |
| `ASYNKOR_TEAM` | Active team slug — overrides `active_team` in `.asynkor.json`. |

---

## Multi-team config

One developer can be on multiple teams. `.asynkor.json` supports a teams array; the active team is switched via `asynkor teams switch <slug>` or the `ASYNKOR_TEAM` env var.

```json
{
  "teams": [
    { "slug": "acme-backend",  "api_key": "cf_live_...", "context": "Main product" },
    { "slug": "open-source",   "api_key": "cf_live_...", "context": "OSS side project" }
  ],
  "active_team": "acme-backend"
}
```

Per-project keys can also live in `.asynkor.json` in the project root (gitignored), or globally in `~/.asynkor/config.json`.

---

## Self-hosting

Asynkor is fully self-hostable. If your codebase is air-gapped or subject to data-residency requirements, run the Go MCP server, Postgres, and Redis inside your own VPC — nothing leaves your perimeter.

Point the client at your deployment with `ASYNKOR_SERVER_URL` or the `serverUrl` field in `.asynkor.json`. The deployment guide lives at https://asynkor.com/docs under "Self-hosting".

---

## What Asynkor does not do

Asynkor coordinates what lands in your repo. It does not:

- **Write code.** Your agents do. Swap your agent tomorrow — Asynkor stays the same.
- **Auto-merge PRs.** Collision prevention happens before the edit. Merging stays in your existing git / CI flow.
- **Replace your CI, git host, or agent.** It sits between them.

In the hosted deployment, file snapshots transit our infrastructure between agent handoffs so the next agent inherits the latest content. For air-gapped codebases, self-host.

---

## Contributing

Issues, PRs, and discussions welcome at https://github.com/asynkor/asynkor. The client (this package) is MIT-licensed; the full Asynkor project is Apache-2.0. The bar for contributions is straightforward: real bugs, real features, honest tests.

---

## License

MIT — see LICENSE.
