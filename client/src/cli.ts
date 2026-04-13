#!/usr/bin/env node
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { loadConfig, resolveConfig, initConfig, ConfigError } from './config.js';
import { AsynkorMcpProxy } from './proxy.js';
import readline from 'node:readline';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

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

### Required workflow — follow this EVERY session, NO EXCEPTIONS

1. **Session start**: Call \`asynkor_briefing\` FIRST, before anything else. This gives you the full team state: active work, recent decisions, open follow-ups, rules, zones, and team memory. Do not skip this.

2. **Before starting work**: Call \`asynkor_start\` with:
   - \`plan\`: what you're about to do, in plain language
   - \`paths\`: comma-separated list of files you expect to touch (critical for overlap detection)
   - \`followup_id\`: if you're picking up an open follow-up from the briefing

   **CRITICAL — OVERLAP AND ZONE WARNINGS:**
   If \`asynkor_start\` returns ANY overlap or zone warning, you MUST:
   - **STOP IMMEDIATELY.** Do NOT proceed with the work.
   - **Do NOT say "I'll proceed carefully"** — that defeats the entire purpose.
   - **Tell the user exactly what the overlap is**: who is working on what, which files conflict.
   - **Ask the user for an explicit go-ahead** before doing anything else.
   - **If the user says to wait or change scope, obey.** Adjust your plan and call \`asynkor_start\` again with the new scope.

   This is non-negotiable. Ignoring overlap warnings causes the exact problem Asynkor exists to prevent.

3. **Before editing files**: Call \`asynkor_check\` with the file paths you plan to modify. This returns active overlaps, applicable rules, protected zones, and relevant team memory. If there are warnings, follow the same STOP protocol as step 2.

4. **When you learn something important**: Call \`asynkor_remember\` to save it to the team brain. Good candidates: architectural decisions, gotchas, business logic insights, debugging discoveries, patterns found, conventions established. **Be generous — call this often.** Every memory you save makes the next agent smarter.

5. **When done — MANDATORY, NEVER SKIP**: Call \`asynkor_finish\` with:
   - \`result\`: what was accomplished
   - \`learnings\`: key things learned (architectural insights, gotchas, patterns)
   - \`decisions\`: important choices made and why
   - \`files_touched\`: comma-separated list of files modified
   - \`followups\`: JSON array of follow-up tasks for teammates

   **You MUST call \`asynkor_finish\` before ending the conversation.** If the user says "thanks" or "done" or stops responding, call \`asynkor_finish\` with whatever you have. Incomplete finish is better than no finish.

### Memory — capture aggressively

Call \`asynkor_remember\` whenever you:
- Make or discover an architectural decision
- Find a gotcha or non-obvious behavior
- Establish a pattern or convention
- Fix a bug with a non-obvious root cause
- Learn something about the codebase that would save a future agent time

One memory per insight. Short, specific, actionable.

### Parallel work and sub-agents

If the briefing shows multiple open follow-ups that can be done in parallel (independent files, no overlap), consider using the Agent tool to spawn sub-agents for each one. Each sub-agent should:
- Call \`asynkor_start\` with its own plan and paths
- Do the work
- Call \`asynkor_remember\` with any learnings
- Call \`asynkor_finish\` with its results

If a sub-agent's \`asynkor_start\` returns an overlap warning, it MUST stop and surface the conflict — same rules as above, no exceptions.

### Why this matters

Every call to \`asynkor_finish\` and \`asynkor_remember\` enriches the team's shared brain. If you skip \`asynkor_finish\` or don't call \`asynkor_remember\`, the next agent starts from scratch on everything you already figured out. That's wasted work for the entire team.`;

const AUTO_APPROVE_TOOLS = [
  'mcp__asynkor__asynkor_briefing',
  'mcp__asynkor__asynkor_start',
  'mcp__asynkor__asynkor_finish',
  'mcp__asynkor__asynkor_check',
  'mcp__asynkor__asynkor_remember',
];

interface JoinLinkResponse {
  team: { slug: string; name: string };
  api_key: string;
  server_url: string;
  setup: {
    asynkor_json: Record<string, unknown>;
    mcp_install: string;
    claude_md: string;
  };
}

async function fetchJoinLink(url: string): Promise<JoinLinkResponse> {
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
  return (await res.json()) as JoinLinkResponse;
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

  // Use resolveConfig() instead of loadConfig() so we can start even
  // without a key. The proxy's hot-reload mechanism will detect when
  // .asynkor.json appears (e.g. after the user runs init in another
  // terminal) and connect automatically — no Claude Code restart needed.
  const { config } = resolveConfig();

  const proxy = new AsynkorMcpProxy(config);

  let server;
  try {
    server = await proxy.createStdioServer();
  } catch (err) {
    if (config) {
      process.stderr.write(`[asynkor] Failed to connect to server at ${config.serverUrl}: ${err}\n`);
      process.stderr.write('[asynkor] Make sure the Asynkor server is running.\n');
    }
    // Don't exit — the proxy will retry via scheduleReconnect.
    // If there was no config, createStdioServer succeeds in disconnected
    // mode and the proxy waits for config to appear.
    if (!config) {
      process.stderr.write('[asynkor] Starting in disconnected mode. Tools will return errors until an API key is configured.\n');
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
  const { execSync } = await import('node:child_process');

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
      res.writeHead(200, { 'Content-Type': 'text/html', 'Access-Control-Allow-Origin': '*' });
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
    if (platform === 'darwin') execSync(`open "${authUrl}"`);
    else if (platform === 'win32') execSync(`start "" "${authUrl}"`);
    else execSync(`xdg-open "${authUrl}"`);
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
  if (team) existing.team = team;

  fs.writeFileSync(configPath, JSON.stringify(existing, null, 2) + '\n');

  console.log('');
  console.log(`Logged in${team ? ` (team: ${team})` : ''}`);
  console.log(`API key saved to ${configPath}`);
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

Claude Code setup:
  claude mcp add asynkor -- npx @asynkor/mcp start

Environment variables:
  ASYNKOR_API_KEY         API key (used by start, and by init for non-interactive setup)
  ASYNKOR_SERVER_URL      Override server URL (default: https://mcp.asynkor.com)
  ASYNKOR_TEAM            Optional team slug used by init when ASYNKOR_API_KEY is set
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
