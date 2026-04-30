## Asynkor — team coordination

You are connected to a team via Asynkor. This workflow is MANDATORY, not advisory.

**If the asynkor tools are not available** (ToolSearch returns nothing, tool calls fail), STOP and tell the user: "The Asynkor MCP tools are not available — the MCP server may not be running or connected. Should I proceed without coordination?" Do NOT silently skip the workflow.

### 1. Connect — read the team brain

Call `asynkor_briefing` FIRST, before anything else. This gives you:
- **Active work**: who is doing what, which files are leased
- **Parked work**: unfinished sessions available for pickup (with `handoff_id`)
- **Active leases**: which files are currently locked by other agents
- **Recent completions, follow-ups, rules, zones, and team memory**

If the briefing shows **CONTEXT REQUIRED**, the long-term context is empty. You must scan the codebase first — see step 2.

### 2. First-time setup — populate long-term context

If the team brain is empty (no memories), `asynkor_start` will refuse to proceed. Before you can start any work, you must:

1. Read: README, directory structure, key config files, recent git history
2. Call `asynkor_remember` for each key insight — architecture decisions, conventions, tech stack, gotchas, file ownership patterns
3. Aim for 5–10 memories that give a future agent enough context to orient in under a minute

This only happens once per team. After the initial scan, every agent inherits the context automatically.

### 3. Start work — declare intent + acquire leases

Call `asynkor_start` with:
- `plan`: what you're about to do, in plain language
- `paths`: comma-separated list of files you expect to touch (**critical** — these become your file leases)
- `followup_id`: if picking up an open follow-up
- `handoff_id`: if resuming a parked work session (inherits the previous agent's plan, progress, and decisions)

**SAVE THE `work_id`** from the response. You will need it for `asynkor_finish` or `asynkor_park` if your session reconnects. The session-to-work binding can break on proxy reconnects — the `work_id` is your recovery key.

**What happens automatically:**
- File leases are acquired on every path you declare. Other agents trying to edit these files will be told to wait.
- Overlap detection runs against all active work. If teammates are working on the same files or a similar plan, you'll get warnings or blocks.
- Zone enforcement checks protected areas.

**On overlap, zone, or lease warnings — STOP:**
- Do NOT proceed. Tell the user exactly what the conflict is.
- Ask for explicit go-ahead before continuing.
- If told to wait or change scope, adjust and call `asynkor_start` again.

**If the response contains `action_required` with blocked leases:**
- You MUST call `asynkor_lease_wait` on the blocked paths before editing those files.
- Do NOT edit blocked files without acquiring their leases first.
- After acquiring, RE-READ the files — they may have been changed by the previous holder.

### 4. During work — leases protect your files

Your declared paths are leased automatically at start. If you need to edit additional files not in your original paths:

1. Call `asynkor_check` with the new paths — see if they're leased by someone else
2. If free: call `asynkor_lease_acquire` to lease them
3. If leased: call `asynkor_lease_wait` to block until they're released (up to 30s, retryable)
4. **After a wait completes: RE-READ the files before editing.** The previous holder may have changed them.

**If the response contains `file_snapshots`:** Another agent uploaded the actual file content. **WRITE each snapshot to your local filesystem** before editing — this is the other agent's version of the file. Edit on top of it to avoid merge conflicts. This is critical for cross-machine coordination where both developers work on separate clones.

Leases auto-expire after 5 minutes and are refreshed while your session is active. They're released when you finish, park, or disconnect.

### 5. Capture learnings — feed the team brain

The team brain has **two stores**, not one:

1. **Long-term project context** — single versioned doc, edited via `asynkor_context_update`. Read it with `asynkor_context`. This is the canonical brain: architecture, conventions, durable gotchas, design rules. Every briefing returns the head version verbatim.
2. **Memory entries** (`asynkor_remember` → surfaced as "Team memory" in the briefing) — append-only **staging** feed. Short-term insights, incident notes, in-progress decisions, debugging breadcrumbs.

**Workflow rule.** Memory should NOT accumulate as a parallel knowledge base — that's the long-term doc's job. When you finish work:

- If a learning is **durable** (architecture, gotcha, convention, design rule that future agents must know), do BOTH:
  1. Merge it into the long-term context via `asynkor_context_update` (pass the FULL new content — versions are atomic).
  2. Call `asynkor_forget(memory_id)` on any staging entry that's now redundant. The briefing surfaces memory IDs as `[id <uuid>]` on each entry — copy from there.
- If a learning is **transient** (incident note for the current sprint, in-progress decision that won't matter in a week), leave it in `asynkor_remember` and let it age out.

**While you work**, `asynkor_remember` is fine for capturing as you go — but at finish time, audit your own staging entries. If they're durable, promote-and-forget. If they're already covered by the long-term doc, just `asynkor_forget`. The team memory list should stay small (<10 entries) — if it doesn't, someone is hoarding instead of merging.

One memory per insight. Short, specific, actionable.

### 6. End work — finish or park

#### Option A: Work is done → `asynkor_finish`
- `result`: what was accomplished (be specific: files modified, behavior changed)
- `learnings`: key things learned
- `decisions`: important choices made and why
- `files_touched`: comma-separated list of files modified
- `file_snapshots`: **REQUIRED for cross-machine coordination.** JSON object mapping each modified file path to its current content. Read each file you modified and include it: `{"src/api.ts": "<full file content>", ...}`. This lets agents on other machines get your version of the file directly from the server, so they can edit on top without conflicts.
- `followups`: JSON array of tasks for teammates

**Before calling finish**, apply the merge-and-clean rule from step 5: if any of your `learnings` / `decisions` are durable, push them to `asynkor_context_update` and `asynkor_forget` the matching staging memories. Don't leave the team memory list bloated.

This releases all your leases, persists your work to the team history, and makes your learnings available to every future agent.

**You MUST call `asynkor_finish` before ending the conversation.** Incomplete finish is better than no finish.

#### Option B: Work is not done → `asynkor_park`
- `progress`: what's done and what's left (be specific so the next agent can pick up)
- `notes`: blockers, dependencies, things to watch out for
- `learnings`: key things learned so far
- `decisions`: choices made and why
- `files_touched`: files modified so far

This releases your leases (so files aren't blocked) and saves your short-term context as a **handoff**. The parked work appears in the briefing with a `handoff_id` that another agent can use to resume exactly where you left off.

Use `asynkor_park` when:
- The user says to stop or switch tasks
- You hit a blocker that requires external input
- The session is ending but the work isn't complete
- You want to hand off to a different agent or developer

### Parallel work and sub-agents

If the briefing shows multiple open follow-ups or parked work that can be done in parallel (independent files, no overlap), consider spawning sub-agents. Each sub-agent should follow this same workflow independently. The lease system will catch any file collisions automatically — if a sub-agent's `asynkor_lease_acquire` or `asynkor_start` hits a leased file, it waits for the holder to finish, then re-reads and proceeds.

### Quick reference

| Tool | When | Key params |
|------|------|------------|
| `asynkor_briefing` | Session start | — |
| `asynkor_context` / `asynkor_context_update` | Read or rewrite the long-term project doc | content, summary |
| `asynkor_remember` | Stage a short-term insight | content, paths, tags |
| `asynkor_forget` | Delete a staging memory after merging it (or as cleanup) | memory_id |
| `asynkor_start` | Begin work | plan, paths, handoff_id, followup_id |
| `asynkor_check` | Before editing files | paths |
| `asynkor_lease_acquire` | Need additional files | paths |
| `asynkor_lease_wait` | File is leased by another agent | paths, timeout_seconds |
| `asynkor_finish` | Work complete | result, learnings, decisions, files_touched, followups |
| `asynkor_park` | Work incomplete, save for later | progress, notes, learnings, decisions, files_touched |
| `asynkor_cancel` | Clean up stale/orphaned work | work_id |
| `asynkor_inspect` | Read a teammate's live work state without interrupting | work_id |
| `asynkor_ask` | Open an async thread to a teammate (work / host / team) | target, topic, question, context_paths |
| `asynkor_inbox` | List threads addressed to me | — |
| `asynkor_thread` | Read full transcript | thread_id |
| `asynkor_reply` | Append a reply (optionally close) | thread_id, body, close |

### Agent comms — when leases aren't enough

File leases stop two agents from editing the same file. **Threads** let them coordinate beyond that — async questions, decisions, hand-offs that need a back-and-forth. All async, all auto-approved.

- **Inspect first, ask second.** `asynkor_inspect(work_id)` returns the full live state of one work — plan, planned paths, files touched, learnings, decisions, parked notes, leases held. Read this before opening a thread; the answer might already be in the metadata.
- **Routing.** `asynkor_ask(target: ...)` accepts `work:<id>` (narrowest), `host:<name>` (durable across the developer's sessions), or `team` (broadcast).
- **Don't block waiting.** Threads are async. Open one, then keep working. Replies surface in the next briefing's inbox section (top 3) and via `asynkor_inbox` (full list).
- **Close when answered.** `asynkor_reply(thread_id, body, close: "true")` removes the thread from the team's open list. If the answer is a durable architectural decision, also call `asynkor_context_update` so future agents inherit it without re-asking — threads feed the brain; the brain is the source of truth.
