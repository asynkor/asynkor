/**
 * install.ts — Nozomio-style one-command install across every MCP-capable IDE
 * on the machine. `asynkor install` detects each IDE by its standard user-scope
 * config dir and writes/merges the Asynkor MCP server entry. Idempotent, so
 * re-running picks up newly-installed IDEs.
 *
 * Why USER-scope (not project-scope): the user wants Asynkor to "just work"
 * from any project, like Nozomio. User-scope MCP configs are read by the IDE
 * regardless of which directory the editor was opened from.
 *
 * IDEs that only support project-scoped MCP configs (Trae, Antigravity, the
 * regular JetBrains/.junie path) are intentionally not auto-installed here —
 * they need `asynkor init --ide <name>` inside a specific project. The
 * `asynkor install` summary mentions them so users know.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

const HOME = os.homedir();
const PLATFORM = process.platform;

interface ServerBlock {
  command: string;
  args: string[];
  env: Record<string, string>;
}

function serverBlock(apiKey: string, team?: string): ServerBlock {
  const env: Record<string, string> = { ASYNKOR_API_KEY: apiKey };
  if (team) env.ASYNKOR_TEAM = team;
  return { command: 'npx', args: ['-y', '@asynkor/mcp', 'start'], env };
}

export type InstallAction = 'created' | 'updated' | 'unchanged';

export type InstallResult =
  | { ok: true; ide: string; name: string; file: string; action: InstallAction; note?: string }
  | { ok: false; ide: string; name: string; reason: string }
  | { ok: 'skipped'; ide: string; name: string; reason: string };

interface IdeAdapter {
  id: string;
  name: string;
  /** True if any signal (config dir or binary) suggests this IDE is installed. */
  detected(): boolean;
  /** Write/merge the Asynkor MCP server into the IDE's user-scope config. */
  install(apiKey: string, team?: string): InstallResult;
}

/* ---------- helpers ---------- */

function readJsonFile(file: string): Record<string, any> {
  if (!fs.existsSync(file)) return {};
  try {
    const txt = fs.readFileSync(file, 'utf-8');
    if (!txt.trim()) return {};
    return JSON.parse(txt);
  } catch {
    return {};
  }
}

function writeJsonFile(file: string, data: Record<string, any>): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, JSON.stringify(data, null, 2) + '\n');
}

function dirExists(p: string): boolean {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
}

function fileExists(p: string): boolean {
  try {
    return fs.statSync(p).isFile();
  } catch {
    return false;
  }
}

/**
 * Common path for every IDE that consumes the standard `mcpServers: {...}`
 * shape (Cursor, Windsurf, Claude Code's `~/.claude.json`, etc.).
 */
function mergeMcpServers(file: string, apiKey: string, team: string | undefined, ide: string, name: string, key = 'mcpServers'): InstallResult {
  const existed = fileExists(file);
  const json = readJsonFile(file);
  const before = JSON.stringify(json[key]?.asynkor ?? null);
  json[key] = json[key] || {};
  json[key].asynkor = serverBlock(apiKey, team);
  const after = JSON.stringify(json[key].asynkor);

  if (existed && before === after) {
    return { ok: true, ide, name, file, action: 'unchanged' };
  }
  writeJsonFile(file, json);
  return { ok: true, ide, name, file, action: existed ? 'updated' : 'created' };
}

/* ---------- adapters ---------- */

const claudeCode: IdeAdapter = {
  id: 'claude-code',
  name: 'Claude Code',
  detected() {
    return fileExists(path.join(HOME, '.claude.json')) || dirExists(path.join(HOME, '.claude'));
  },
  install(apiKey, team) {
    return mergeMcpServers(path.join(HOME, '.claude.json'), apiKey, team, 'claude-code', 'Claude Code');
  },
};

const cursor: IdeAdapter = {
  id: 'cursor',
  name: 'Cursor',
  detected() {
    return (
      dirExists(path.join(HOME, '.cursor')) ||
      dirExists(path.join(HOME, 'Library/Application Support/Cursor')) ||
      dirExists(path.join(HOME, '.config/Cursor'))
    );
  },
  install(apiKey, team) {
    return mergeMcpServers(path.join(HOME, '.cursor/mcp.json'), apiKey, team, 'cursor', 'Cursor');
  },
};

const windsurf: IdeAdapter = {
  id: 'windsurf',
  name: 'Windsurf',
  detected() {
    return (
      dirExists(path.join(HOME, '.codeium/windsurf')) ||
      dirExists(path.join(HOME, 'Library/Application Support/Windsurf')) ||
      dirExists(path.join(HOME, '.config/Windsurf'))
    );
  },
  install(apiKey, team) {
    return mergeMcpServers(path.join(HOME, '.codeium/windsurf/mcp_config.json'), apiKey, team, 'windsurf', 'Windsurf');
  },
};

const zed: IdeAdapter = {
  id: 'zed',
  name: 'Zed',
  detected() {
    return (
      dirExists(path.join(HOME, '.config/zed')) ||
      dirExists(path.join(HOME, 'Library/Application Support/Zed'))
    );
  },
  install(apiKey, team) {
    // Zed uses `context_servers` (not `mcpServers`) at the top level of settings.json.
    return mergeMcpServers(path.join(HOME, '.config/zed/settings.json'), apiKey, team, 'zed', 'Zed', 'context_servers');
  },
};

const vscode: IdeAdapter = {
  id: 'vscode',
  name: 'VS Code (Copilot Chat)',
  detected() {
    if (PLATFORM === 'darwin') return dirExists(path.join(HOME, 'Library/Application Support/Code/User'));
    if (PLATFORM === 'linux') return dirExists(path.join(HOME, '.config/Code/User'));
    if (PLATFORM === 'win32') return dirExists(path.join(HOME, 'AppData/Roaming/Code/User'));
    return false;
  },
  install(apiKey, team) {
    let userDir: string;
    if (PLATFORM === 'darwin') userDir = path.join(HOME, 'Library/Application Support/Code/User');
    else if (PLATFORM === 'linux') userDir = path.join(HOME, '.config/Code/User');
    else userDir = path.join(HOME, 'AppData/Roaming/Code/User');

    // VS Code MCP user-scope config: `{userDir}/mcp.json` with `servers: { ... }` shape.
    return mergeMcpServers(path.join(userDir, 'mcp.json'), apiKey, team, 'vscode', 'VS Code (Copilot Chat)', 'servers');
  },
};

const codex: IdeAdapter = {
  id: 'codex',
  name: 'OpenAI Codex CLI',
  detected() {
    return dirExists(path.join(HOME, '.codex'));
  },
  install(apiKey, team) {
    const file = path.join(HOME, '.codex/config.toml');
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const existed = fileExists(file);
    const existing = existed ? fs.readFileSync(file, 'utf-8') : '';

    const teamLine = team ? `, "ASYNKOR_TEAM" = "${team}"` : '';
    const block =
      `[mcp_servers.asynkor]\n` +
      `command = "npx"\n` +
      `args = ["-y", "@asynkor/mcp", "start"]\n` +
      `env = { "ASYNKOR_API_KEY" = "${apiKey}"${teamLine} }\n` +
      `\n` +
      `[mcp_servers.asynkor.tools.asynkor_briefing]\n` +
      `approval_mode = "auto"\n` +
      `\n` +
      `[mcp_servers.asynkor.tools.asynkor_check]\n` +
      `approval_mode = "auto"\n`;

    let updated: string;
    let action: InstallAction;

    if (existing.includes('[mcp_servers.asynkor]')) {
      // Replace existing asynkor block. Match from `[mcp_servers.asynkor]` up to
      // the next top-level `[section]` that is NOT a sub-table of asynkor.
      const re = /\[mcp_servers\.asynkor\][\s\S]*?(?=\n\[(?!mcp_servers\.asynkor)|\s*$)/;
      updated = existing.replace(re, block.trimEnd());
      if (!updated.endsWith('\n')) updated += '\n';
      if (updated === existing) return { ok: true, ide: 'codex', name: 'OpenAI Codex CLI', file, action: 'unchanged' };
      action = 'updated';
    } else {
      const sep = existing && !existing.endsWith('\n') ? '\n\n' : existing ? '\n' : '';
      updated = existing + sep + block;
      action = existed ? 'updated' : 'created';
    }

    fs.writeFileSync(file, updated);
    return { ok: true, ide: 'codex', name: 'OpenAI Codex CLI', file, action };
  },
};

/** Order matters for the install summary — most popular first. */
const ADAPTERS: IdeAdapter[] = [claudeCode, cursor, windsurf, zed, vscode, codex];

/**
 * IDEs whose MCP configs are project-scope only (no user-scope analogue we
 * can write). Listed for the summary so users know to run
 * `asynkor init --ide <name>` from inside the relevant project.
 */
const PROJECT_SCOPE_ONLY = [
  { id: 'jetbrains', name: 'JetBrains / Junie', cmd: 'asynkor init --ide jetbrains' },
  { id: 'trae', name: 'Trae', cmd: 'asynkor init --ide trae' },
  { id: 'antigravity', name: 'Google Antigravity', cmd: 'asynkor init --ide antigravity' },
];

/* ---------- public API ---------- */

export interface InstallSummary {
  installed: { ide: string; name: string; file: string; action: InstallAction }[];
  notDetected: { ide: string; name: string }[];
  errors: { ide: string; name: string; reason: string }[];
  projectScopeOnly: { id: string; name: string; cmd: string }[];
}

export function installAllIdes(apiKey: string, team?: string): InstallSummary {
  const out: InstallSummary = {
    installed: [],
    notDetected: [],
    errors: [],
    projectScopeOnly: PROJECT_SCOPE_ONLY,
  };

  for (const adapter of ADAPTERS) {
    if (!adapter.detected()) {
      out.notDetected.push({ ide: adapter.id, name: adapter.name });
      continue;
    }
    try {
      const r = adapter.install(apiKey, team);
      if (r.ok === true) {
        out.installed.push({ ide: r.ide, name: r.name, file: r.file, action: r.action });
      } else if (r.ok === false) {
        out.errors.push({ ide: r.ide, name: r.name, reason: r.reason });
      }
      // ok === 'skipped' — currently unused, reserved for future per-adapter skips
    } catch (err) {
      out.errors.push({ ide: adapter.id, name: adapter.name, reason: err instanceof Error ? err.message : String(err) });
    }
  }
  return out;
}

/**
 * Pretty-print the summary to stdout. Symbols mirror what `asynkor init`
 * already uses (✓ for done, · for skipped) so the visual style is consistent.
 */
export function printInstallSummary(summary: InstallSummary): void {
  console.log('Asynkor system-wide install');
  console.log('─'.repeat(40));

  if (summary.installed.length === 0 && summary.errors.length === 0) {
    console.log('No supported IDEs detected on this machine.');
    console.log('Install one of: Claude Code, Cursor, Windsurf, Zed, VS Code, Codex CLI');
    console.log('Then re-run: asynkor install');
    return;
  }

  for (const entry of summary.installed) {
    const verb = entry.action === 'created' ? 'wrote' : entry.action === 'updated' ? 'updated' : 'already configured';
    console.log(`✓ ${entry.name} — ${verb} ${entry.file}`);
  }

  for (const entry of summary.errors) {
    console.log(`✗ ${entry.name} — ${entry.reason}`);
  }

  if (summary.notDetected.length > 0) {
    const names = summary.notDetected.map((e) => e.name).join(', ');
    console.log(`· Not detected (skipped): ${names}`);
  }

  console.log('');
  console.log('Restart any open editor to pick up the new MCP server.');
  console.log('When you install a new IDE later, re-run `asynkor install`.');

  if (summary.projectScopeOnly.length > 0) {
    console.log('');
    console.log('Project-scope only (run inside a project):');
    for (const e of summary.projectScopeOnly) {
      console.log(`  • ${e.name}: ${e.cmd}`);
    }
  }
}
