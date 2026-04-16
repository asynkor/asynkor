#!/usr/bin/env node
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { loadConfig, resolveConfig, resolveAllTeams, initConfig, addTeamToConfig, removeTeamFromConfig, ConfigError } from './config.js';
import { AsynkorMcpProxy } from './proxy.js';
import readline from 'node:readline';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { z } from 'zod';

const [, , command, ...rest] = process.argv;

function parseFlags(args: string[]): Record<string, string> {
  const flags: Record<string, string> = {};
  for (let i = 0; i < args.length; i++) {
    if (args[i].startsWith('--') && i + 1 < args.length && !args[i + 1].startsWith('--')) {
      flags[args[i].slice(2)] = args[i + 1];
      i++;
    }
  }
  return flags;
}

type IdeType = 'claude-code' | 'vscode' | 'cursor' | 'windsurf' | 'zed' | 'jetbrains' | 'codex' | 'trae' | 'antigravity';

const ASYNKOR_WORKFLOW = `You are connected to a team via Asynkor — the shared brain for agent orchestrators.

Required workflow (every session, no exceptions):
1. FIRST: call asynkor_briefing to see what teammates are working on.
2. Before starting work: call asynkor_start with plan + paths (files you expect to touch). If the server returns an overlap or zone warning, stop and coordinate.
3. Before editing files: call asynkor_check with the file paths. Respect any warnings.
4. When you learn something important: call asynkor_remember to save it to the team brain.
5. When done: call asynkor_finish with result, learnings, decisions, files_touched, and follow-ups.

Every asynkor_finish and asynkor_remember enriches the team's shared brain. Be generous — your decisions persist beyond your session and are inherited by every future agent.`;

const ASYNKOR_CLAUDE_MD = `## Asynkor — team coordination

You are connected to a team via Asynkor. This workflow is MANDATORY, not advisory.

**If the asynkor tools are not available** (ToolSearch returns nothing, tool calls fail), STOP and tell the user: "The Asynkor MCP tools are not available — the MCP server may not be running or connected. Should I proceed without coordination?" Do NOT silently skip the workflow.

### 1. Connect — read the team brain

Call \`asynkor_briefing\` FIRST, before anything else. This gives you:
- **Active work**: who is doing what, which files are leased
- **Parked work**: unfinished sessions available for pickup (with \`handoff_id\`)
- **Active leases**: which files are currently locked by other agents
- **Recent completions, follow-ups, rules, zones, and team memory**

If the briefing shows **CONTEXT REQUIRED**, the long-term context is empty. You must scan the codebase first — see step 2.

### 2. First-time setup — populate long-term context

If the team brain is empty (no memories), \`asynkor_start\` will refuse to proceed. Before you can start any work, you must:

1. Read: README, directory structure, key config files, recent git history
2. Call \`asynkor_remember\` for each key insight — architecture decisions, conventions, tech stack, gotchas, file ownership patterns
3. Aim for 5-10 memories that give a future agent enough context to orient in under a minute

This only happens once per team. After the initial scan, every agent inherits the context automatically.

### 3. Start work — declare intent + acquire leases

Call \`asynkor_start\` with:
- \`plan\`: what you're about to do, in plain language
- \`paths\`: comma-separated list of files you expect to touch (**critical** — these become your file leases)
- \`followup_id\`: if picking up an open follow-up
- \`handoff_id\`: if resuming a parked work session (inherits the previous agent's plan, progress, and decisions)

**SAVE THE \`work_id\`** from the response. You will need it for \`asynkor_finish\` or \`asynkor_park\` if your session reconnects.

**On overlap, zone, or lease warnings — STOP:**
- Do NOT proceed. Tell the user exactly what the conflict is.
- Ask for explicit go-ahead before continuing.
- If told to wait or change scope, adjust and call \`asynkor_start\` again.

**If the response contains \`action_required\` with blocked leases:**
- You MUST call \`asynkor_lease_wait\` on the blocked paths before editing those files.
- Do NOT edit blocked files without acquiring their leases first.
- After acquiring, RE-READ the files — they may have been changed by the previous holder.

### 4. During work — leases protect your files

Your declared paths are leased automatically at start. If you need to edit additional files not in your original paths:

1. Call \`asynkor_check\` with the new paths — see if they're leased by someone else
2. If free: call \`asynkor_lease_acquire\` to lease them
3. If leased: call \`asynkor_lease_wait\` to block until they're released (up to 30s, retryable)
4. **After a wait completes: RE-READ the files before editing.** The previous holder may have changed them.

**If the response contains \`file_snapshots\`:** Another agent uploaded the actual file content. **WRITE each snapshot to your local filesystem** before editing — this is the other agent's version of the file. Edit on top of it to avoid merge conflicts.

Leases auto-expire after 5 minutes and are refreshed while your session is active. They're released when you finish, park, or disconnect.

### 5. Capture learnings — feed the team brain

Call \`asynkor_remember\` whenever you discover something a future agent should know:
- Architectural decisions and why they were made
- Gotchas, non-obvious behavior, debugging discoveries
- Patterns, conventions, file ownership
- Business logic insights

One memory per insight. Short, specific, actionable. The team brain compounds — but only if you write to it.

### 6. End work — finish or park

#### Option A: Work is done → \`asynkor_finish\`
- \`result\`: what was accomplished (be specific: files modified, behavior changed)
- \`learnings\`: key things learned
- \`decisions\`: important choices made and why
- \`files_touched\`: comma-separated list of files modified
- \`file_snapshots\`: **REQUIRED for cross-machine coordination.** JSON object mapping each modified file path to its current content. Read each file you modified and include it: \`{"src/api.ts": "<full file content>", ...}\`.
- \`followups\`: JSON array of tasks for teammates

**You MUST call \`asynkor_finish\` before ending the conversation.** Incomplete finish is better than no finish.

#### Option B: Work is not done → \`asynkor_park\`
- \`progress\`: what's done and what's left
- \`notes\`: blockers, dependencies, things to watch out for
- \`learnings\`: key things learned so far
- \`decisions\`: choices made and why
- \`files_touched\`: files modified so far

This releases your leases and saves your context as a **handoff**. The parked work appears in the briefing with a \`handoff_id\` that another agent can use to resume.

### Quick reference

| Tool | When | Key params |
|------|------|------------|
| \`asynkor_briefing\` | Session start | — |
| \`asynkor_remember\` | Learn something | content, paths, tags |
| \`asynkor_start\` | Begin work | plan, paths, handoff_id, followup_id |
| \`asynkor_check\` | Before editing files | paths |
| \`asynkor_lease_acquire\` | Need additional files | paths |
| \`asynkor_lease_wait\` | File is leased by another agent | paths, timeout_seconds |
| \`asynkor_finish\` | Work complete | result, learnings, decisions, files_touched, file_snapshots, followups |
| \`asynkor_park\` | Work incomplete, save for later | progress, notes, learnings, decisions, files_touched |
| \`asynkor_cancel\` | Clean up stale/orphaned work | work_id |`;

const AUTO_APPROVE_TOOLS = [
  'mcp__asynkor__asynkor_briefing',
  'mcp__asynkor__asynkor_start',
  'mcp__asynkor__asynkor_finish',
  'mcp__asynkor__asynkor_check',
  'mcp__asynkor__asynkor_remember',
  'mcp__asynkor__asynkor_park',
  'mcp__asynkor__asynkor_lease_acquire',
  'mcp__asynkor__asynkor_lease_wait',
  'mcp__asynkor__asynkor_cancel',
  // read-only; writes to the long-term doc (asynkor_context_update) are
  // intentionally NOT auto-approved so the owner sees every modification.
  'mcp__asynkor__asynkor_context',
];

const JoinLinkSchema = z.object({
  team: z.object({ slug: z.string().min(1).max(128), name: z.string().min(1).max(256) }),
  api_key: z.string().regex(/^cf_(live|test)_[a-f0-9]{64}$/, 'Invalid API key format'),
  server_url: z.string().url().refine(u => u.startsWith('https://'), { message: 'Server URL must use HTTPS' }),
  setup: z.object({
    asynkor_json: z.record(z.unknown()),
    mcp_install: z.string().max(1024),
    claude_md: z.string().max(32768),
  }),
});
type JoinLinkResponse = z.infer<typeof JoinLinkSchema>;

async function fetchJoinLink(url: string): Promise<JoinLinkResponse> {
  if (!url.startsWith('https://')) {
    throw new Error('Join link must use HTTPS.');
  }
  const res = await fetch(url, {
    headers: { Accept: 'application/json' },
  });
  if (!res.ok) {
    const status = res.status;
    if (status === 404 || status === 410) {
      throw new Error('This join link has expired or has already been claimed.');
    }
    throw new Error(`Failed to fetch join link (HTTP ${status}).`);
  }
  const json = await res.json();
  const parsed = JoinLinkSchema.safeParse(json);
  if (!parsed.success) {
    throw new Error(`Invalid join link response: ${parsed.error.issues.map(i => i.message).join(', ')}`);
  }
  return parsed.data;
}

function writeAsynkorJson(data: Record<string, unknown>): void {
  const target = path.join(process.cwd(), '.asynkor.json');
  fs.writeFileSync(target, JSON.stringify(data, null, 2) + '\n');
}

function writeClaudeMd(content: string): void {
  const claudeMdPath = path.join(process.cwd(), 'CLAUDE.md');
  const section = content || ASYNKOR_CLAUDE_MD;
  if (fs.existsSync(claudeMdPath)) {
    const existing = fs.readFileSync(claudeMdPath, 'utf8');
    if (!existing.includes('Asynkor')) {
      fs.appendFileSync(claudeMdPath, '\n' + section + '\n');
    }
  } else {
    fs.writeFileSync(claudeMdPath, section + '\n');
  }
}

// Hooks injected by init so the Asynkor workflow is enforced, not just
// advisory. PreToolUse on Edit/Write reminds the agent to check for
// conflicts before touching files. UserPromptSubmit reminds the agent
// to call briefing at the start of each conversation turn when it
// hasn't done so yet this session.
const ASYNKOR_HOOKS = {
  PreToolUse: [
    {
      matcher: 'Edit|Write',
      hooks: [
        {
          type: 'command',
          command: 'echo "⚠️ ASYNKOR: Have you called asynkor_check on these paths? If asynkor_start returned an overlap warning, STOP and ask the user. Do NOT proceed carefully — STOP."',
        },
      ],
    },
  ],
  Stop: [
    {
      hooks: [
        {
          type: 'command',
          command: 'echo "⚠️ ASYNKOR: You MUST call asynkor_finish before ending. Include result, learnings, decisions, files_touched, and followups. Also call asynkor_remember for any insights discovered during this session."',
        },
      ],
    },
  ],
};

function updateClaudeSettings(): void {
  const claudeDir = path.join(process.cwd(), '.claude');
  if (!fs.existsSync(claudeDir)) fs.mkdirSync(claudeDir, { recursive: true });

  const settingsPath = path.join(claudeDir, 'settings.json');
  let settings: Record<string, unknown> = {};
  if (fs.existsSync(settingsPath)) {
    try { settings = JSON.parse(fs.readFileSync(settingsPath, 'utf8')); } catch {}
  }

  // Auto-approve all asynkor tools so the agent never hits a
  // permission dialog during the workflow.
  const perms = (settings.permissions as Record<string, unknown>) ?? {};
  const allow = (perms.allow as string[]) ?? [];
  for (const p of AUTO_APPROVE_TOOLS) {
    if (!allow.includes(p)) allow.push(p);
  }
  settings.permissions = { ...perms, allow };

  // Merge the asynkor hooks. Preserves any existing hooks the user has
  // configured — we only add our entries, never overwrite theirs.
  const existingHooks = (settings.hooks as Record<string, unknown[]>) ?? {};
  for (const [event, hookEntries] of Object.entries(ASYNKOR_HOOKS)) {
    const existing = (existingHooks[event] as Array<{ matcher?: string }>) ?? [];
    // Skip if there's already a asynkor-related hook for this event
    // so we don't duplicate on re-init.
    const alreadyHasAsynkor = existing.some(h =>
      typeof h === 'object' && h !== null && 'hooks' in h &&
      JSON.stringify(h).includes('asynkor')
    );
    if (!alreadyHasAsynkor) {
      existingHooks[event] = [...existing, ...hookEntries];
    }
  }
  settings.hooks = existingHooks;

  fs.writeFileSync(settingsPath, JSON.stringify(settings, null, 2));
}

async function cmdStart(args: string[]): Promise<void> {
  const flags = parseFlags(args);
  if (flags['server-url']) process.env.ASYNKOR_SERVER_URL = flags['server-url'];
  if (flags['api-key']) process.env.ASYNKOR_API_KEY = flags['api-key'];
  if (flags['team']) process.env.ASYNKOR_TEAM = flags['team'];

  // Use resolveAllTeams() to support multi-team configs. If only one
  // team is configured, it auto-selects. If multiple teams exist without
  // an active_team set, the proxy starts disconnected and the AI picks
  // a team via asynkor_teams/asynkor_switch.
  const resolved = resolveAllTeams();

  const proxy = new AsynkorMcpProxy(resolved.teams, resolved.activeSlug);

  let server;
  try {
    server = await proxy.createStdioServer();
  } catch (err) {
    const activeTeam = resolved.teams.find(t => t.slug === resolved.activeSlug);
    if (activeTeam) {
      process.stderr.write(`[asynkor] Failed to connect to server at ${activeTeam.serverUrl}: ${err}\n`);
      process.stderr.write('[asynkor] Make sure the Asynkor server is running.\n');
    }
    // Don't exit — the proxy will retry via scheduleReconnect.
    if (resolved.teams.length === 0) {
      process.stderr.write('[asynkor] Starting in disconnected mode. Tools will return errors until an API key is configured.\n');
    } else if (!resolved.activeSlug) {
      process.stderr.write(`[asynkor] ${resolved.teams.length} teams configured, none selected. AI will choose via asynkor_teams/asynkor_switch.\n`);
    } else {
      process.exit(1);
    }
  }

  const transport = new StdioServerTransport();
  await server!.connect(transport);
}

function generateIdeConfig(ide: IdeType, apiKey: string): { file: string; content: string; rulesFile?: string; rulesContent?: string; instructions: string } {
  const mcpServersJson = JSON.stringify({
    mcpServers: {
      asynkor: {
        command: 'npx',
        args: ['-y', '@asynkor/mcp', 'start'],
        env: { ASYNKOR_API_KEY: apiKey },
      },
    },
  }, null, 2);

  const vscodeJson = JSON.stringify({
    servers: {
      asynkor: {
        command: 'npx',
        args: ['-y', '@asynkor/mcp', 'start'],
        env: { ASYNKOR_API_KEY: apiKey },
      },
    },
  }, null, 2);

  switch (ide) {
    case 'vscode':
      return {
        file: path.join(process.cwd(), '.vscode', 'mcp.json'),
        content: vscodeJson,
        instructions: 'Reload VS Code (Developer: Reload Window). Asynkor tools appear in Copilot Chat agent mode.',
      };
    case 'cursor':
      return {
        file: path.join(process.cwd(), '.cursor', 'mcp.json'),
        content: mcpServersJson,
        rulesFile: path.join(process.cwd(), '.cursorrules'),
        rulesContent: ASYNKOR_WORKFLOW,
        instructions: 'Restart Cursor. Check Settings > MCP for a green dot.',
      };
    case 'windsurf': {
      const home = os.homedir();
      return {
        file: path.join(home, '.codeium', 'windsurf', 'mcp_config.json'),
        content: mcpServersJson,
        rulesFile: path.join(process.cwd(), '.windsurfrules'),
        rulesContent: ASYNKOR_WORKFLOW,
        instructions: 'Refresh MCP servers in Cascade (hammer icon) or restart Windsurf.',
      };
    }
    case 'zed': {
      const home = os.homedir();
      const zedConfig = JSON.stringify({
        context_servers: {
          asynkor: {
            command: 'npx',
            args: ['-y', '@asynkor/mcp', 'start'],
            env: { ASYNKOR_API_KEY: apiKey },
          },
        },
      }, null, 2);
      return {
        file: path.join(home, '.config', 'zed', 'settings.json'),
        content: zedConfig,
        instructions: 'Merge the context_servers block into your existing Zed settings.json. Tools appear in the Agent Panel.',
      };
    }
    case 'jetbrains':
      return {
        file: path.join(process.cwd(), '.junie', 'mcp.json'),
        content: mcpServersJson,
        instructions: 'Restart your IDE. Check Settings > Tools > AI Assistant > MCP Servers.',
      };
    case 'codex': {
      const home = os.homedir();
      const toml = `[mcp_servers.asynkor]\ncommand = "npx"\nargs = ["-y", "@asynkor/mcp", "start"]\nenv = { "ASYNKOR_API_KEY" = "${apiKey}" }\n\n[mcp_servers.asynkor.tools.asynkor_briefing]\napproval_mode = "auto"\n\n[mcp_servers.asynkor.tools.asynkor_check]\napproval_mode = "auto"\n`;
      return {
        file: path.join(home, '.codex', 'config.toml'),
        content: toml,
        instructions: 'Restart Codex CLI. asynkor_briefing and asynkor_check are auto-approved.',
      };
    }
    case 'trae':
      return {
        file: path.join(process.cwd(), '.trae', 'mcp.json'),
        content: mcpServersJson,
        instructions: 'Restart Trae. Asynkor tools appear in the AI assistant.',
      };
    case 'antigravity':
      return {
        file: path.join(process.cwd(), '.antigravity', 'mcp_config.json'),
        content: mcpServersJson,
        instructions: 'Asynkor tools are now available in Antigravity, including Manager View.',
      };
    default:
      return {
        file: path.join(process.cwd(), '.asynkor.json'),
        content: JSON.stringify({ api_key: apiKey, server_url: 'https://mcp.asynkor.com' }, null, 2),
        instructions: 'Run: claude mcp add asynkor -- npx @asynkor/mcp start',
      };
  }
}

function writeIdeConfig(ide: IdeType, apiKey: string): void {
  const config = generateIdeConfig(ide, apiKey);

  const dir = path.dirname(config.file);
  if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });

  if (ide === 'zed' || ide === 'codex') {
    if (fs.existsSync(config.file)) {
      console.log(`\nIMPORTANT: ${config.file} already exists.`);
      console.log('Merge this config manually:');
      console.log('');
      console.log(config.content);
      console.log('');
      console.log(config.instructions);
      return;
    }
  }

  if (ide === 'windsurf' && fs.existsSync(config.file)) {
    try {
      const existing = JSON.parse(fs.readFileSync(config.file, 'utf8'));
      const incoming = JSON.parse(config.content);
      existing.mcpServers = { ...existing.mcpServers, ...incoming.mcpServers };
      fs.writeFileSync(config.file, JSON.stringify(existing, null, 2) + '\n');
    } catch {
      fs.writeFileSync(config.file, config.content + '\n');
    }
  } else {
    fs.writeFileSync(config.file, config.content + '\n');
  }
  console.log(`\u2713 ${config.file}`);

  if (config.rulesFile && config.rulesContent) {
    if (fs.existsSync(config.rulesFile)) {
      const existing = fs.readFileSync(config.rulesFile, 'utf8');
      if (!existing.includes('asynkor_briefing')) {
        fs.appendFileSync(config.rulesFile, '\n' + config.rulesContent + '\n');
        console.log(`\u2713 ${config.rulesFile} (appended)`);
      } else {
        console.log(`\u2713 ${config.rulesFile} (already has Asynkor instructions)`);
      }
    } else {
      fs.writeFileSync(config.rulesFile, config.rulesContent + '\n');
      console.log(`\u2713 ${config.rulesFile}`);
    }
  }

  console.log('');
  console.log(config.instructions);
}

async function cmdInit(args: string[]): Promise<void> {
  const flags = parseFlags(args);
  const linkUrl = flags['link'];

  if (linkUrl) {
    await setupFromJoinLink(linkUrl);
    return;
  }

  const ideFlag = flags['ide'] as IdeType | undefined;
  if (ideFlag && ideFlag !== 'claude-code') {
    const validIdes: IdeType[] = ['vscode', 'cursor', 'windsurf', 'zed', 'jetbrains', 'codex', 'trae', 'antigravity'];
    if (!validIdes.includes(ideFlag)) {
      console.error(`Unknown IDE: ${ideFlag}`);
      console.error(`Supported: ${validIdes.join(', ')}`);
      process.exit(1);
    }

    const apiKey = process.env.ASYNKOR_API_KEY?.trim() || flags['api-key']?.trim();
    if (!apiKey) {
      console.error('API key required. Set ASYNKOR_API_KEY or pass --api-key cf_live_...');
      process.exit(1);
    }

    console.log(`Asynkor setup for ${ideFlag}`);
    console.log('─'.repeat(40));
    writeIdeConfig(ideFlag, apiKey);

    initConfig({ apiKey, serverUrl: process.env.ASYNKOR_SERVER_URL?.trim() || 'https://mcp.asynkor.com' });
    console.log('\u2713 .asynkor.json created');
    return;
  }

  // Non-interactive path: if ASYNKOR_API_KEY is set in the environment, skip
  // the prompts entirely. The docs at /docs tell users to do exactly this.
  const envApiKey = process.env.ASYNKOR_API_KEY?.trim();
  if (envApiKey) {
    const serverUrl = process.env.ASYNKOR_SERVER_URL?.trim() || 'https://mcp.asynkor.com';
    const team = process.env.ASYNKOR_TEAM?.trim() || undefined;

    initConfig({ apiKey: envApiKey, serverUrl, team });
    updateClaudeSettings();
    writeClaudeMd(ASYNKOR_CLAUDE_MD);

    console.log('Asynkor setup (from environment)');
    console.log('─────────────────────────────────────');
    console.log('✓ .asynkor.json created');
    console.log('✓ .claude/settings.json updated (all asynkor tools auto-approved)');
    console.log('✓ CLAUDE.md updated with Asynkor instructions');
    console.log('\nAdd to Claude Code:');
    console.log('  claude mcp add asynkor -- npx @asynkor/mcp start');
    return;
  }

  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  const ask = (q: string): Promise<string> =>
    new Promise((resolve) => rl.question(q, resolve));

  console.log('Asynkor setup');
  console.log('─────────────────────────────────────');

  const apiKey = (await ask('API key (cf_live_...): ')).trim();
  if (!apiKey) {
    console.error('API key is required.');
    rl.close();
    process.exit(1);
  }

  const serverUrlInput = (await ask('Server URL [https://mcp.asynkor.com]: ')).trim();
  const serverUrl = serverUrlInput || 'https://mcp.asynkor.com';

  const team = (await ask('Team slug (optional): ')).trim();

  rl.close();

  initConfig({ apiKey, serverUrl, team: team || undefined });
  updateClaudeSettings();
  writeClaudeMd(ASYNKOR_CLAUDE_MD);

  console.log('\n✓ .asynkor.json created');
  console.log('✓ .claude/settings.json updated (all asynkor tools auto-approved)');
  console.log('✓ CLAUDE.md updated with Asynkor instructions');
  console.log('\nAdd to Claude Code:');
  console.log('  claude mcp add asynkor -- npx @asynkor/mcp start');
}

async function setupFromJoinLink(url: string): Promise<void> {
  let data: JoinLinkResponse;
  try {
    data = await fetchJoinLink(url);
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    console.error(`Error: ${msg}`);
    process.exit(1);
  }

  writeAsynkorJson(data.setup.asynkor_json);
  writeClaudeMd(data.setup.claude_md || ASYNKOR_CLAUDE_MD);
  updateClaudeSettings();

  const teamName = data.team.name;
  const teamSlug = data.team.slug;
  const mcpInstall = data.setup.mcp_install || 'claude mcp add asynkor -- npx @asynkor/mcp start';

  console.log(`\nConnected to team "${teamName}" (${teamSlug})`);
  console.log('');
  console.log('Config written to .asynkor.json');
  console.log('Instructions written to CLAUDE.md');
  console.log('Tools auto-approved in .claude/settings.json');
  console.log('');
  console.log('To complete setup, run:');
  console.log(`  ${mcpInstall}`);
  console.log('');
  console.log('Then restart your Claude Code session.');
}

async function cmdSetup(args: string[]): Promise<void> {
  const url = args.find((a) => !a.startsWith('--'));
  if (!url) {
    console.error('Usage: asynkor setup <join-link-url>');
    console.error('Example: asynkor setup https://asynkor.com/join/abc123');
    process.exit(1);
  }
  await setupFromJoinLink(url);
}

async function cmdLogin(args: string[]): Promise<void> {
  const flags = parseFlags(args);
  const serverUrl = flags['server-url'] || process.env.ASYNKOR_SERVER_URL?.trim() || 'https://asynkor.com';

  const http = await import('node:http');
  const { execFileSync } = await import('node:child_process');

  const port = await new Promise<number>((resolve) => {
    const srv = http.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address() as { port: number };
      srv.close(() => resolve(addr.port));
    });
  });

  let callbackResolve: (params: URLSearchParams) => void;
  const callbackPromise = new Promise<URLSearchParams>((resolve) => {
    callbackResolve = resolve;
  });

  const server = http.createServer((req, res) => {
    const url = new URL(req.url ?? '/', `http://127.0.0.1:${port}`);

    if (url.pathname === '/callback') {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end('<html><body><p>Done. You can close this tab.</p></body></html>');
      callbackResolve!(url.searchParams);
    } else {
      res.writeHead(404);
      res.end();
    }
  });

  server.listen(port, '127.0.0.1');

  const authUrl = `${serverUrl}/v1/auth/cli?port=${port}`;

  console.log('Opening browser...');
  console.log(`If it doesn't open, go to: ${authUrl}`);
  console.log('');
  console.log('Waiting for authorization...');

  try {
    const platform = process.platform;
    if (platform === 'darwin') execFileSync('open', [authUrl]);
    else if (platform === 'win32') execFileSync('cmd', ['/c', 'start', '', authUrl]);
    else execFileSync('xdg-open', [authUrl]);
  } catch {
    // browser open failed, user can use the printed URL
  }

  const timeout = setTimeout(() => {
    console.error('Timed out waiting for authorization (5 minutes).');
    server.close();
    process.exit(1);
  }, 300_000);

  const params = await callbackPromise;
  clearTimeout(timeout);
  server.close();

  const apiKey = params.get('api_key');
  const team = params.get('team');
  const refreshToken = params.get('refresh_token');

  if (!apiKey) {
    console.error('Authorization failed — no API key received.');
    process.exit(1);
  }

  const configDir = path.join(os.homedir(), '.asynkor');
  if (!fs.existsSync(configDir)) fs.mkdirSync(configDir, { recursive: true });

  const configPath = path.join(configDir, 'config.json');
  let existing: Record<string, unknown> = {};
  if (fs.existsSync(configPath)) {
    try { existing = JSON.parse(fs.readFileSync(configPath, 'utf8')); } catch {}
  }

  existing.api_key = apiKey;
  existing.server_url = 'https://mcp.asynkor.com';
  existing.api_url = serverUrl.replace('mcp.', 'api.').replace(/\/$/, '');
  if (team) existing.team = team;
  if (refreshToken) existing.refresh_token = refreshToken;

  fs.writeFileSync(configPath, JSON.stringify(existing, null, 2) + '\n', { mode: 0o600 });

  console.log('');
  console.log(`Logged in${team ? ` (team: ${team})` : ''}`);
  console.log(`API key saved to ${configPath}`);
  if (refreshToken) {
    console.log('Refresh token saved — expired keys will be auto-refreshed.');
  }
  console.log('');
  console.log('Next steps:');
  console.log('  claude mcp add asynkor -- npx @asynkor/mcp start');
}

async function cmdStatus(): Promise<void> {
  let cfg;
  try {
    cfg = loadConfig();
  } catch (err) {
    if (err instanceof ConfigError) {
      console.error(err.message);
      process.exit(1);
    }
    throw err;
  }

  const { Client } = await import('@modelcontextprotocol/sdk/client/index.js');
  const { SSEClientTransport } = await import('@modelcontextprotocol/sdk/client/sse.js');

  const transport = new SSEClientTransport(new URL('/sse', cfg.serverUrl), {
    requestInit: { headers: { Authorization: `Bearer ${cfg.apiKey}` } },
  });
  const client = new Client({ name: '@asynkor/mcp', version: '0.1.0' });

  try {
    await client.connect(transport);
  } catch (err) {
    console.error(`Cannot connect to ${cfg.serverUrl}: ${err}`);
    process.exit(1);
  }

  try {
    const result = await client.callTool({ name: 'asynkor_briefing', arguments: {} });
    const content = result.content as Array<{ type: string; text: string }>;
    const text = content.find((c) => c.type === 'text')?.text ?? '';
    console.log(text || '(empty briefing)');
  } catch (err) {
    console.error(`Failed to fetch briefing: ${err}`);
    process.exitCode = 1;
  } finally {
    await client.close();
  }
}

async function cmdTeams(args: string[]): Promise<void> {
  const sub = args[0];
  const flags = parseFlags(args.slice(1));

  if (!sub || sub === 'list') {
    const resolved = resolveAllTeams();
    if (resolved.teams.length === 0) {
      console.log('No teams configured.');
      console.log('Run `asynkor init` or `asynkor login` to set up.');
      return;
    }
    console.log(`Configured teams (${resolved.teams.length}):`);
    for (const t of resolved.teams) {
      const active = t.slug === resolved.activeSlug ? ' (active)' : '';
      const ctx = t.context ? ` — ${t.context}` : '';
      console.log(`  ${t.slug}${t.name ? ` [${t.name}]` : ''}${active}${ctx}`);
    }
    if (!resolved.activeSlug && resolved.teams.length > 1) {
      console.log('\nNo active team set. Use: asynkor teams switch <slug>');
    }
    return;
  }

  if (sub === 'add') {
    const apiKey = flags['api-key'] || process.env.ASYNKOR_API_KEY?.trim();
    const slug = flags['slug'];
    if (!apiKey || !slug) {
      console.error('Usage: asynkor teams add --slug <slug> --api-key <key> [--name <name>] [--context <desc>] [--server-url <url>]');
      process.exit(1);
    }
    addTeamToConfig({
      slug,
      name: flags['name'],
      apiKey,
      serverUrl: flags['server-url'],
      context: flags['context'],
    });
    console.log(`Team "${slug}" added to .asynkor.json`);
    return;
  }

  if (sub === 'remove') {
    const slug = args[1];
    if (!slug || slug.startsWith('--')) {
      console.error('Usage: asynkor teams remove <slug>');
      process.exit(1);
    }
    if (removeTeamFromConfig(slug)) {
      console.log(`Team "${slug}" removed.`);
    } else {
      console.error(`Team "${slug}" not found in config.`);
      process.exit(1);
    }
    return;
  }

  if (sub === 'switch') {
    const slug = args[1];
    if (!slug || slug.startsWith('--')) {
      console.error('Usage: asynkor teams switch <slug>');
      process.exit(1);
    }
    const resolved = resolveAllTeams();
    if (!resolved.teams.find(t => t.slug === slug)) {
      console.error(`Team "${slug}" not found. Available: ${resolved.teams.map(t => t.slug).join(', ')}`);
      process.exit(1);
    }

    const { setActiveTeamInConfig } = await import('./config.js');
    setActiveTeamInConfig(slug);
    console.log(`Active team set to "${slug}".`);
    return;
  }

  console.error(`Unknown teams subcommand: ${sub}`);
  console.error('Usage: asynkor teams [list|add|remove|switch]');
  process.exit(1);
}

function printHelp(): void {
  console.log(`
@asynkor/mcp — Asynkor MCP client

Usage:
  asynkor login                     Sign in via browser and get your API key automatically
  asynkor start [flags]             Start the MCP proxy server
  asynkor init                      Set up .asynkor.json (interactive, or non-interactive if ASYNKOR_API_KEY is set)
  asynkor init --ide <name>         Set up for a specific IDE
  asynkor init --link <url>         Set up from a one-time join link
  asynkor setup <url>               Set up from a one-time join link (alias)
  asynkor status                    Show current team briefing
  asynkor teams                     List configured teams
  asynkor teams add                 Add a team to config
  asynkor teams remove <slug>       Remove a team from config
  asynkor teams switch <slug>       Set the active team
  asynkor help                      Show this help

Supported IDEs (--ide flag):
  vscode          VS Code / GitHub Copilot (.vscode/mcp.json)
  cursor          Cursor (.cursor/mcp.json + .cursorrules)
  windsurf        Windsurf (~/.codeium/windsurf/mcp_config.json + .windsurfrules)
  zed             Zed (~/.config/zed/settings.json)
  jetbrains       IntelliJ / WebStorm / PyCharm (.junie/mcp.json)
  codex           OpenAI Codex CLI (~/.codex/config.toml)
  trae            Trae (.trae/mcp.json)
  antigravity     Google Antigravity (.antigravity/mcp_config.json)

Flags for start:
  --server-url <url>      MCP server URL (default: https://mcp.asynkor.com)
  --api-key <key>         API key
  --team <slug>           Active team (when multiple teams configured)

Flags for teams add:
  --slug <slug>           Team slug (required)
  --api-key <key>         API key for the team (required)
  --name <name>           Display name
  --context <desc>        Brief description (helps AI choose the right team)
  --server-url <url>      Override server URL

Multi-team config (.asynkor.json):
  {
    "teams": [
      { "slug": "my-team", "api_key": "cf_live_...", "context": "Main product" },
      { "slug": "oss-lib", "api_key": "cf_live_...", "context": "Open source CLI" }
    ],
    "active_team": "my-team"
  }

Claude Code setup:
  claude mcp add asynkor -- npx @asynkor/mcp start

Environment variables:
  ASYNKOR_API_KEY         API key (used by start, and by init for non-interactive setup)
  ASYNKOR_SERVER_URL      Override server URL (default: https://mcp.asynkor.com)
  ASYNKOR_TEAM            Active team slug (overrides active_team in config)
`);
}

switch (command) {
  case 'start':
    cmdStart(rest).catch((err) => {
      process.stderr.write(`[asynkor] Fatal: ${err}\n`);
      process.exit(1);
    });
    break;
  case 'init':
    cmdInit(rest).catch((err) => {
      console.error(err);
      process.exit(1);
    });
    break;
  case 'login':
    cmdLogin(rest).catch((err) => {
      console.error(err);
      process.exit(1);
    });
    break;
  case 'setup':
    cmdSetup(rest).catch((err) => {
      console.error(err);
      process.exit(1);
    });
    break;
  case 'status':
    cmdStatus().catch((err) => {
      console.error(err);
      process.exit(1);
    });
    break;
  case 'teams':
    cmdTeams(rest).catch((err) => {
      console.error(err);
      process.exit(1);
    });
    break;
  case 'help':
  case '--help':
  case '-h':
    printHelp();
    break;
  default:
    if (command) {
      console.error(`Unknown command: ${command}`);
    }
    printHelp();
    process.exit(command ? 1 : 0);
}
