import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

export interface AsynkorConfig {
  apiKey: string;
  serverUrl: string;
  team?: string;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ConfigError';
  }
}

interface AsynkorJson {
  api_key?: string;
  server_url?: string;
  team?: string;
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
  fs.writeFileSync(target, JSON.stringify(config, null, 2) + '\n');
}
