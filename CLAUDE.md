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

Call `asynkor_remember` whenever you discover something a future agent should know:
- Architectural decisions and why they were made
- Gotchas, non-obvious behavior, debugging discoveries
- Patterns, conventions, file ownership
- Business logic insights

One memory per insight. Short, specific, actionable. The team brain compounds — but only if you write to it.

### 6. End work — finish or park

#### Option A: Work is done → `asynkor_finish`
- `result`: what was accomplished (be specific: files modified, behavior changed)
- `learnings`: key things learned
- `decisions`: important choices made and why
- `files_touched`: comma-separated list of files modified
- `file_snapshots`: **REQUIRED for cross-machine coordination.** JSON object mapping each modified file path to its current content. Read each file you modified and include it: `{"src/api.ts": "<full file content>", ...}`. This lets agents on other machines get your version of the file directly from the server, so they can edit on top without conflicts.
- `followups`: JSON array of tasks for teammates

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
| `asynkor_remember` | Learn something | content, paths, tags |
| `asynkor_start` | Begin work | plan, paths, handoff_id, followup_id |
| `asynkor_check` | Before editing files | paths |
| `asynkor_lease_acquire` | Need additional files | paths |
| `asynkor_lease_wait` | File is leased by another agent | paths, timeout_seconds |
| `asynkor_finish` | Work complete | result, learnings, decisions, files_touched, followups |
| `asynkor_park` | Work incomplete, save for later | progress, notes, learnings, decisions, files_touched |
| `asynkor_cancel` | Clean up stale/orphaned work | work_id |
