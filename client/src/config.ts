import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

export interface AsynkorConfig {
  apiKey: string;
  serverUrl: string;
  team?: string;
}

export interface AsynkorTeam {
  slug: string;
  name?: string;
  apiKey: string;
  serverUrl: string;
  context?: string;
  refreshToken?: string;
  apiUrl?: string; // Java backend URL for auth refresh (default: https://api.asynkor.com)
}

export interface MultiTeamConfig {
  teams: AsynkorTeam[];
  activeSlug: string | null;
  configPath: string | null;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ConfigError';
  }
}

interface AsynkorJsonTeam {
  slug: string;
  name?: string;
  api_key: string;
  server_url?: string;
  context?: string;
  refresh_token?: string;
  api_url?: string;
}

interface AsynkorJson {
  api_key?: string;
  server_url?: string;
  team?: string;
  refresh_token?: string;
  api_url?: string;
  teams?: AsynkorJsonTeam[];
  active_team?: string;
}

function findAsynkorJson(startDir: string): string | null {
  let dir = startDir;
  for (let i = 0; i < 10; i++) {
    const candidate = path.join(dir, '.asynkor.json');
    if (fs.existsSync(candidate)) return candidate;
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return null;
}

/**
 * resolveConfig reads all config sources (env → .asynkor.json → ~/.asynkor/config.json)
 * and returns { config, configPath }. Returns a null apiKey if no key is found anywhere
 * (instead of throwing). configPath is the resolved .asynkor.json path for hot-reload
 * watching, or null if no file was found.
 */
export function resolveConfig(): { config: AsynkorConfig | null; configPath: string | null } {
  const jsonPath = findAsynkorJson(process.cwd());
  let fileConfig: AsynkorJson = {};
  if (jsonPath) {
    try {
      fileConfig = JSON.parse(fs.readFileSync(jsonPath, 'utf-8'));
    } catch {
      // ignore parse errors, fall through to env
    }
  }

  const userConfigPath = path.join(os.homedir(), '.asynkor', 'config.json');
  let userConfig: AsynkorJson = {};
  if (fs.existsSync(userConfigPath)) {
    try {
      userConfig = JSON.parse(fs.readFileSync(userConfigPath, 'utf-8'));
    } catch {}
  }

  const apiKey =
    process.env.ASYNKOR_API_KEY ||
    fileConfig.api_key ||
    userConfig.api_key ||
    '';

  const serverUrl =
    process.env.ASYNKOR_SERVER_URL ||
    fileConfig.server_url ||
    userConfig.server_url ||
    'https://mcp.asynkor.com';

  if (!apiKey) {
    return { config: null, configPath: jsonPath };
  }

  return {
    config: { apiKey, serverUrl, team: fileConfig.team || userConfig.team },
    configPath: jsonPath,
  };
}

export function loadConfig(): AsynkorConfig {
  const { config } = resolveConfig();
  if (!config) {
    throw new ConfigError(
      'Asynkor API key not found.\n' +
      'Set ASYNKOR_API_KEY env var, or create .asynkor.json with {"api_key": "cf_live_..."}.\n' +
      'Get your key at: https://asynkor.com/dashboard'
    );
  }
  return config;
}

export function initConfig(options: { apiKey: string; serverUrl?: string; team?: string }): void {
  const config: AsynkorJson = {
    api_key: options.apiKey,
    server_url: options.serverUrl || 'https://mcp.asynkor.com',
  };
  if (options.team) config.team = options.team;

  const target = path.join(process.cwd(), '.asynkor.json');
  fs.writeFileSync(target, JSON.stringify(config, null, 2) + '\n', { mode: 0o600 });

  // Mirror to ~/.asynkor/config.json so the proxy works system-wide — even
  // when started from a directory without .asynkor.json (e.g. user-scope
  // `claude mcp add` from any cwd, git worktrees that diverged from the
  // init root, etc.). Per-project .asynkor.json still takes priority.
  writeUserConfig({ apiKey: options.apiKey, serverUrl: options.serverUrl, team: options.team });
}

export function writeUserConfig(options: { apiKey: string; serverUrl?: string; team?: string }): void {
  const userDir = path.join(os.homedir(), '.asynkor');
  const userPath = path.join(userDir, 'config.json');

  let existing: AsynkorJson = {};
  if (fs.existsSync(userPath)) {
    try { existing = JSON.parse(fs.readFileSync(userPath, 'utf-8')); } catch { /* corrupt or unreadable, overwrite */ }
  }

  const merged: AsynkorJson = {
    ...existing,
    api_key: options.apiKey,
    server_url: options.serverUrl || existing.server_url || 'https://mcp.asynkor.com',
  };
  if (options.team) {
    merged.team = options.team;
    merged.active_team = options.team;
  }

  try { fs.mkdirSync(userDir, { recursive: true, mode: 0o700 }); } catch { /* exists */ }
  fs.writeFileSync(userPath, JSON.stringify(merged, null, 2) + '\n', { mode: 0o600 });
}

function parseJsonTeams(json: AsynkorJson, serverUrlFallback: string): AsynkorTeam[] {
  if (!json.teams || json.teams.length === 0) return [];
  return json.teams.map(t => ({
    slug: t.slug,
    name: t.name,
    apiKey: t.api_key,
    serverUrl: t.server_url || serverUrlFallback,
    context: t.context,
    refreshToken: t.refresh_token,
    apiUrl: t.api_url,
  }));
}

/**
 * resolveAllTeams reads all config sources and returns every configured team.
 * Supports both the legacy single-key format (treated as a team with slug "default")
 * and the new multi-team format with a `teams` array.
 *
 * Priority: env → .asynkor.json → ~/.asynkor/config.json
 * If `teams[]` exists in any source, it takes priority over the single `api_key`.
 */
export function resolveAllTeams(): MultiTeamConfig {
  const jsonPath = findAsynkorJson(process.cwd());
  let fileConfig: AsynkorJson = {};
  if (jsonPath) {
    try {
      fileConfig = JSON.parse(fs.readFileSync(jsonPath, 'utf-8'));
    } catch {}
  }

  const userConfigPath = path.join(os.homedir(), '.asynkor', 'config.json');
  let userConfig: AsynkorJson = {};
  if (fs.existsSync(userConfigPath)) {
    try {
      userConfig = JSON.parse(fs.readFileSync(userConfigPath, 'utf-8'));
    } catch {}
  }

  const defaultServerUrl =
    process.env.ASYNKOR_SERVER_URL ||
    fileConfig.server_url ||
    userConfig.server_url ||
    'https://mcp.asynkor.com';

  // Check for multi-team config (file-level takes priority over user-level)
  let teams = parseJsonTeams(fileConfig, defaultServerUrl);
  if (teams.length === 0) {
    teams = parseJsonTeams(userConfig, defaultServerUrl);
  }

  // If no teams[] array, fall back to single api_key → synthetic "default" team.
  // Slug priority must include ASYNKOR_TEAM env so a session that sets both
  // ASYNKOR_API_KEY+ASYNKOR_TEAM ends up with a team whose slug matches the
  // active_team validation below (otherwise activeSlug='40outof40' won't find
  // the synthesized 'default' team and the proxy thinks it has no key).
  if (teams.length === 0) {
    const apiKey =
      process.env.ASYNKOR_API_KEY ||
      fileConfig.api_key ||
      userConfig.api_key ||
      '';

    if (apiKey) {
      teams = [{
        slug: process.env.ASYNKOR_TEAM || fileConfig.team || userConfig.team || 'default',
        apiKey,
        serverUrl: defaultServerUrl,
        refreshToken: fileConfig.refresh_token || userConfig.refresh_token,
        apiUrl: fileConfig.api_url || userConfig.api_url,
      }];
    }
  }

  // Active team: env → file → user config → auto-select if only one
  let activeSlug =
    process.env.ASYNKOR_TEAM ||
    fileConfig.active_team ||
    userConfig.active_team ||
    null;

  // Auto-select if only one team is configured
  if (!activeSlug && teams.length === 1) {
    activeSlug = teams[0].slug;
  }

  // Validate activeSlug refers to an existing team
  if (activeSlug && !teams.find(t => t.slug === activeSlug)) {
    activeSlug = null;
  }

  return { teams, activeSlug, configPath: jsonPath };
}

export function addTeamToConfig(team: { slug: string; name?: string; apiKey: string; serverUrl?: string; context?: string; refreshToken?: string; apiUrl?: string }): void {
  const jsonPath = findAsynkorJson(process.cwd()) || path.join(process.cwd(), '.asynkor.json');
  let existing: AsynkorJson = {};
  if (fs.existsSync(jsonPath)) {
    try {
      existing = JSON.parse(fs.readFileSync(jsonPath, 'utf-8'));
    } catch {}
  }

  // Migrate from single-key to multi-team format if needed
  if (!existing.teams) {
    existing.teams = [];
    if (existing.api_key) {
      existing.teams.push({
        slug: existing.team || 'default',
        api_key: existing.api_key,
        server_url: existing.server_url,
      });
      delete existing.api_key;
      delete existing.server_url;
      delete existing.team;
    }
  }

  // Remove existing entry with same slug
  existing.teams = existing.teams.filter(t => t.slug !== team.slug);

  const entry: AsynkorJsonTeam = {
    slug: team.slug,
    name: team.name,
    api_key: team.apiKey,
    server_url: team.serverUrl || 'https://mcp.asynkor.com',
    context: team.context,
  };
  if (team.refreshToken) entry.refresh_token = team.refreshToken;
  if (team.apiUrl) entry.api_url = team.apiUrl;
  existing.teams.push(entry);

  fs.writeFileSync(jsonPath, JSON.stringify(existing, null, 2) + '\n', { mode: 0o600 });
}

export function removeTeamFromConfig(slug: string): boolean {
  const jsonPath = findAsynkorJson(process.cwd());
  if (!jsonPath) return false;

  let existing: AsynkorJson = {};
  try {
    existing = JSON.parse(fs.readFileSync(jsonPath, 'utf-8'));
  } catch {
    return false;
  }

  if (!existing.teams) return false;

  const before = existing.teams.length;
  existing.teams = existing.teams.filter(t => t.slug !== slug);
  if (existing.teams.length === before) return false;

  if (existing.active_team === slug) {
    delete existing.active_team;
  }

  fs.writeFileSync(jsonPath, JSON.stringify(existing, null, 2) + '\n', { mode: 0o600 });
  return true;
}

export function updateTeamKeyInConfig(slug: string, newApiKey: string, newRefreshToken?: string): void {
  const jsonPath = findAsynkorJson(process.cwd());
  if (!jsonPath) return;

  let existing: AsynkorJson = {};
  try {
    existing = JSON.parse(fs.readFileSync(jsonPath, 'utf-8'));
  } catch {
    return;
  }

  if (existing.teams) {
    const team = existing.teams.find(t => t.slug === slug);
    if (team) {
      team.api_key = newApiKey;
      if (newRefreshToken) team.refresh_token = newRefreshToken;
    }
  } else if (existing.api_key && (existing.team === slug || slug === 'default')) {
    existing.api_key = newApiKey;
    if (newRefreshToken) existing.refresh_token = newRefreshToken;
  }

  fs.writeFileSync(jsonPath, JSON.stringify(existing, null, 2) + '\n', { mode: 0o600 });
}

export function setActiveTeamInConfig(slug: string): void {
  const jsonPath = findAsynkorJson(process.cwd()) || path.join(process.cwd(), '.asynkor.json');
  let existing: AsynkorJson = {};
  if (fs.existsSync(jsonPath)) {
    try {
      existing = JSON.parse(fs.readFileSync(jsonPath, 'utf-8'));
    } catch {}
  }
  existing.active_team = slug;
  fs.writeFileSync(jsonPath, JSON.stringify(existing, null, 2) + '\n', { mode: 0o600 });
}
