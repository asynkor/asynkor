import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { SSEClientTransport } from '@modelcontextprotocol/sdk/client/sse.js';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
  ListResourcesRequestSchema,
  ReadResourceRequestSchema,
  SubscribeRequestSchema,
  UnsubscribeRequestSchema,
  ResourceUpdatedNotificationSchema,
  Tool,
  Resource,
} from '@modelcontextprotocol/sdk/types.js';
import type { CallToolResult, ReadResourceResult } from '@modelcontextprotocol/sdk/types.js';
import os from 'node:os';
import type { AsynkorConfig, AsynkorTeam } from './config.js';
import { resolveAllTeams, updateTeamKeyInConfig } from './config.js';

const RECONNECT_BASE_MS = 3000;
const MAX_RECONNECT_ATTEMPTS = 10;
const SUBSCRIBE_POLL_MS = 3000;

// How often to stat the config file for changes. Cheap (~50μs on SSD)
// and negligible compared to the network round-trip of a tool call.
const CONFIG_CHECK_INTERVAL_MS = 3000;

// Synthetic tools handled locally by the proxy for multi-team support.
const SYNTHETIC_TOOLS: Tool[] = [
  {
    name: 'asynkor_teams',
    description: 'List all configured teams and show which one is currently active. Each team includes a context description to help you choose the right one. Use this before asynkor_switch when multiple teams are available.',
    inputSchema: { type: 'object' as const, properties: {} },
  },
  {
    name: 'asynkor_switch',
    description: 'Switch to a different team. This disconnects from the current team (auto-parking any active work) and reconnects to the selected one. After switching, call asynkor_briefing to orient yourself in the new team context. Finish or park active work before switching when possible.',
    inputSchema: {
      type: 'object' as const,
      properties: {
        team: { type: 'string', description: 'Team slug to switch to. Use asynkor_teams to see available slugs.' },
      },
      required: ['team'],
    },
  },
];

export class AsynkorMcpProxy {
  private goClient: Client | null = null;
  private reconnectAttempts = 0;
  private stopping = false;
  private cachedTools: Tool[] = [];
  private cachedResources: Resource[] = [];
  private subscriptions = new Map<string, ReturnType<typeof setInterval>>();

  // Multi-team state
  private teams: AsynkorTeam[];
  private activeTeamSlug: string | null;

  // Hot-reload state: tracks what we're currently connected with so we
  // can detect changes and reconnect without restarting the process.
  private currentApiKey: string;
  private currentServerUrl: string;
  private lastConfigCheck = 0;

  constructor(
    teams: AsynkorTeam[],
    activeTeamSlug: string | null,
    private readonly agentName: string = 'claude-code',
    private readonly agentVersion: string = process.env.CLAUDE_CODE_VERSION ?? 'unknown',
  ) {
    this.teams = teams;
    this.activeTeamSlug = activeTeamSlug;

    const activeTeam = teams.find(t => t.slug === activeTeamSlug);
    this.currentApiKey = activeTeam?.apiKey ?? '';
    this.currentServerUrl = activeTeam?.serverUrl ?? 'https://mcp.asynkor.com';
  }

  /** @deprecated Use the multi-team constructor. Kept for backward compat. */
  static fromLegacyConfig(cfg: AsynkorConfig | null): AsynkorMcpProxy {
    if (!cfg) return new AsynkorMcpProxy([], null);
    const team: AsynkorTeam = {
      slug: cfg.team || 'default',
      apiKey: cfg.apiKey,
      serverUrl: cfg.serverUrl,
    };
    return new AsynkorMcpProxy([team], team.slug);
  }

  private buildTransport(): SSEClientTransport {
    const sseUrl = new URL('/sse', this.currentServerUrl);
    return new SSEClientTransport(sseUrl, {
      requestInit: {
        headers: {
          Authorization: `Bearer ${this.currentApiKey}`,
          'X-Agent': this.agentName,
          'X-Agent-Version': this.agentVersion,
          'X-Hostname': os.hostname(),
        },
      },
    });
  }

  private async connectToGoServer(): Promise<Client> {
    const transport = this.buildTransport();
    const client = new Client(
      { name: '@asynkor/mcp', version: '0.1.0' },
    );

    client.onclose = () => {
      if (!this.stopping) {
        this.goClient = null;
        this.scheduleReconnect();
      }
    };

    await client.connect(transport);
    this.reconnectAttempts = 0;
    return client;
  }

  private scheduleReconnect(): void {
    if (this.reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) {
      process.stderr.write(
        `[asynkor] Gave up reconnecting after ${MAX_RECONNECT_ATTEMPTS} attempts.\n`,
      );
      return;
    }
    this.reconnectAttempts++;
    const delay = RECONNECT_BASE_MS * Math.min(this.reconnectAttempts, 5);
    process.stderr.write(
      `[asynkor] Connection lost. Reconnecting in ${delay / 1000}s (attempt ${this.reconnectAttempts})...\n`,
    );
    setTimeout(async () => {
      try {
        // Re-check config before reconnecting — the key may have changed
        // while we were disconnected.
        await this.checkConfigReload();
        if (!this.currentApiKey) {
          process.stderr.write('[asynkor] Still no API key. Waiting for .asynkor.json...\n');
          this.scheduleReconnect();
          return;
        }
        this.goClient = await this.connectToGoServer();
        process.stderr.write('[asynkor] Reconnected.\n');
      } catch (err) {
        process.stderr.write(`[asynkor] Reconnect failed: ${err}\n`);
        this.scheduleReconnect();
      }
    }, delay);
  }

  /**
   * checkConfigReload re-reads the config from disk (at most once per
   * CONFIG_CHECK_INTERVAL_MS) and reconnects the SSE client if the API
   * key or server URL changed. This is the hot-reload mechanism that
   * lets a developer write .asynkor.json mid-session without restarting
   * Claude Code.
   *
   * Called at the top of every tool call and list handler. The stat()
   * call is ~50μs on SSD, negligible compared to the network round-trip.
   */
  private async checkConfigReload(): Promise<void> {
    const now = Date.now();
    if (now - this.lastConfigCheck < CONFIG_CHECK_INTERVAL_MS) return;
    this.lastConfigCheck = now;

    const resolved = resolveAllTeams();
    this.teams = resolved.teams;

    // If active team was set via config file, update it
    if (resolved.activeSlug && resolved.activeSlug !== this.activeTeamSlug) {
      this.activeTeamSlug = resolved.activeSlug;
    }

    const activeTeam = this.teams.find(t => t.slug === this.activeTeamSlug);
    const newKey = activeTeam?.apiKey ?? '';
    const newUrl = activeTeam?.serverUrl ?? 'https://mcp.asynkor.com';

    if (newKey === this.currentApiKey && newUrl === this.currentServerUrl) {
      return; // no change
    }

    const hadKey = !!this.currentApiKey;
    this.currentApiKey = newKey;
    this.currentServerUrl = newUrl;

    if (!newKey) {
      process.stderr.write('[asynkor] Config changed but API key is empty. Waiting...\n');
      return;
    }

    // Key appeared or changed — (re)connect.
    if (hadKey) {
      process.stderr.write('[asynkor] API key changed. Reconnecting...\n');
    } else {
      process.stderr.write('[asynkor] API key detected. Connecting...\n');
    }

    try {
      if (this.goClient) {
        await this.goClient.close();
        this.goClient = null;
      }
      this.goClient = await this.connectToGoServer();
      this.cachedTools = (await this.goClient.listTools()).tools;
      try {
        this.cachedResources = (await this.goClient.listResources()).resources ?? [];
      } catch {
        this.cachedResources = [];
      }
      process.stderr.write(`[asynkor] Connected with new key. ${this.cachedTools.length} tools available.\n`);
    } catch (err) {
      process.stderr.write(`[asynkor] Failed to connect with new key: ${err}\n`);
      this.scheduleReconnect();
    }
  }

  private startSubscriptionPoll(uri: string, server: Server): void {
    if (this.subscriptions.has(uri)) return;

    let lastHash = '';

    const poll = async () => {
      if (!this.goClient) return;
      try {
        const result = await this.goClient.readResource({ uri });
        const hash = JSON.stringify(result);
        if (hash !== lastHash) {
          lastHash = hash;
          if (lastHash !== '') {
            await server.sendResourceUpdated({ uri });
          }
        }
      } catch {
        // ignore transient errors
      }
    };

    const timer = setInterval(poll, SUBSCRIBE_POLL_MS);
    this.subscriptions.set(uri, timer);
  }

  private stopSubscriptionPoll(uri: string): void {
    const timer = this.subscriptions.get(uri);
    if (timer) {
      clearInterval(timer);
      this.subscriptions.delete(uri);
    }
  }

  // ── Synthetic tool handlers (multi-team) ──────────────────────────

  private handleTeamsList(): CallToolResult {
    const teamList = this.teams.map(t => ({
      slug: t.slug,
      name: t.name || t.slug,
      context: t.context || '(no context set — set via .asynkor.json teams[].context)',
      active: t.slug === this.activeTeamSlug,
      server_url: t.serverUrl,
    }));

    let instructions: string;
    if (this.teams.length === 0) {
      instructions = 'No teams configured. Run `npx @asynkor/mcp init` or `npx @asynkor/mcp login` to set up.';
    } else if (this.teams.length === 1) {
      instructions = 'Only one team configured — it is auto-selected.';
    } else {
      instructions = 'Call asynkor_switch with the team slug to switch. After switching, call asynkor_briefing to orient yourself.';
    }

    return {
      content: [{
        type: 'text' as const,
        text: JSON.stringify({
          teams: teamList,
          active_team: this.activeTeamSlug,
          total: this.teams.length,
          instructions,
        }),
      }],
    };
  }

  private async handleTeamSwitch(args: Record<string, unknown> | undefined): Promise<CallToolResult> {
    const slug = (args?.team as string)?.trim();
    if (!slug) {
      return {
        content: [{ type: 'text' as const, text: JSON.stringify({
          error: 'missing_team',
          message: 'The "team" parameter is required. Call asynkor_teams to see available slugs.',
        }) }],
        isError: true,
      };
    }

    const team = this.teams.find(t => t.slug === slug);
    if (!team) {
      return {
        content: [{ type: 'text' as const, text: JSON.stringify({
          error: 'team_not_found',
          message: `Team "${slug}" not found.`,
          available: this.teams.map(t => t.slug),
        }) }],
        isError: true,
      };
    }

    if (slug === this.activeTeamSlug && this.goClient) {
      return {
        content: [{ type: 'text' as const, text: JSON.stringify({
          status: 'already_active',
          team: slug,
          name: team.name || slug,
          message: `Already connected to team "${team.name || slug}". Call asynkor_briefing to see the team state.`,
        }) }],
      };
    }

    // Disconnect from current team (Go server will auto-park active work)
    if (this.goClient) {
      process.stderr.write(`[asynkor] Switching from "${this.activeTeamSlug}" to "${slug}"...\n`);
      try {
        await this.goClient.close();
      } catch {}
      this.goClient = null;
    }

    this.activeTeamSlug = slug;
    this.currentApiKey = team.apiKey;
    this.currentServerUrl = team.serverUrl;
    this.reconnectAttempts = 0;

    try {
      this.goClient = await this.connectToGoServer();
      this.cachedTools = (await this.goClient.listTools()).tools;
      try {
        this.cachedResources = (await this.goClient.listResources()).resources ?? [];
      } catch {
        this.cachedResources = [];
      }
      process.stderr.write(`[asynkor] Connected to team "${slug}". ${this.cachedTools.length} tools.\n`);

      return {
        content: [{ type: 'text' as const, text: JSON.stringify({
          status: 'switched',
          team: slug,
          name: team.name || slug,
          context: team.context,
          tools_available: this.cachedTools.length,
          next_step: 'Call asynkor_briefing to see the team state and orient yourself.',
        }) }],
      };
    } catch (err) {
      process.stderr.write(`[asynkor] Failed to connect to team "${slug}": ${err}\n`);
      this.scheduleReconnect();
      return {
        content: [{ type: 'text' as const, text: JSON.stringify({
          error: 'connection_failed',
          team: slug,
          message: `Failed to connect to team "${slug}": ${err}. Will retry in background.`,
        }) }],
        isError: true,
      };
    }
  }

  /**
   * Attempt to refresh an expired API key using the stored refresh token.
   * Flow: refresh JWT → create new API key → update config on disk → reconnect.
   * Returns the new API key on success, null on failure.
   */
  private async refreshApiKey(team: AsynkorTeam): Promise<string | null> {
    if (!team.refreshToken) return null;

    const apiUrl = team.apiUrl || team.serverUrl.replace('mcp.', 'api.');
    if (!apiUrl.startsWith('https://')) {
      process.stderr.write(`[asynkor] Refusing token refresh: API URL must use HTTPS (got ${apiUrl}).\n`);
      return null;
    }
    process.stderr.write(`[asynkor] API key invalid. Attempting token refresh for team "${team.slug}"...\n`);

    try {
      // 1. Refresh JWT using the stored refresh token
      const refreshRes = await fetch(`${apiUrl}/v1/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: team.refreshToken }),
      });

      if (!refreshRes.ok) {
        process.stderr.write(`[asynkor] Token refresh failed (HTTP ${refreshRes.status}). Run \`asynkor login\` to re-authenticate.\n`);
        return null;
      }

      const { accessToken, refreshToken: newRefreshToken } = await refreshRes.json() as {
        accessToken: string;
        refreshToken: string;
      };

      // 2. Create a new API key using the fresh JWT
      const keyRes = await fetch(`${apiUrl}/v1/teams/${team.slug}/api-keys`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Authorization': `Bearer ${accessToken}`,
        },
        body: JSON.stringify({ label: `Auto-refresh (${os.hostname()})` }),
      });

      if (!keyRes.ok) {
        process.stderr.write(`[asynkor] Failed to create new API key (HTTP ${keyRes.status}). Run \`asynkor login\`.\n`);
        return null;
      }

      const { key: newApiKey } = await keyRes.json() as { key: string };

      // 3. Update config on disk so the new key persists across restarts
      team.refreshToken = newRefreshToken;
      updateTeamKeyInConfig(team.slug, newApiKey, newRefreshToken);

      process.stderr.write(`[asynkor] API key refreshed successfully for team "${team.slug}".\n`);
      return newApiKey;
    } catch (err) {
      process.stderr.write(`[asynkor] Token refresh error: ${err}. Run \`asynkor login\` to re-authenticate.\n`);
      return null;
    }
  }

  async createStdioServer(): Promise<Server> {
    // If we have an active team with a key, connect now. If not (no key
    // yet, or multiple teams with none selected), the server starts in
    // disconnected mode and connects when the AI calls asynkor_switch or
    // when checkConfigReload() detects a key appearing in .asynkor.json.
    if (this.currentApiKey) {
      this.goClient = await this.connectToGoServer();
      this.cachedTools = (await this.goClient.listTools()).tools;
      try {
        this.cachedResources = (await this.goClient.listResources()).resources ?? [];
      } catch {
        this.cachedResources = [];
      }
    } else if (this.teams.length > 1) {
      process.stderr.write(
        `[asynkor] ${this.teams.length} teams configured, none selected. AI will choose via asynkor_teams/asynkor_switch.\n`,
      );
    } else {
      process.stderr.write(
        '[asynkor] No API key found. Waiting for .asynkor.json to appear...\n' +
        '[asynkor] Run: ASYNKOR_API_KEY=cf_live_... npx @asynkor/mcp init\n',
      );
    }

    const server = new Server(
      { name: 'asynkor', version: '0.1.0' },
      { capabilities: { tools: {}, resources: { subscribe: true } } },
    );

    // Forward notifications/resources/updated from Go server → Claude Code.
    // Re-registered after each reconnect in checkConfigReload().
    const setupNotificationForwarding = () => {
      if (!this.goClient) return;
      this.goClient.setNotificationHandler(ResourceUpdatedNotificationSchema, async (notification) => {
        try {
          await server.sendResourceUpdated(notification.params as { uri: string });
        } catch {
          // ignore if client is gone
        }
      });
    };
    setupNotificationForwarding();

    server.setRequestHandler(ListToolsRequestSchema, async () => {
      await this.checkConfigReload();
      const serverTools = this.cachedTools;
      if (this.goClient) {
        try {
          const result = await this.goClient.listTools();
          this.cachedTools = result.tools;
        } catch {
          // fall through to cached
        }
      }
      // Always include synthetic tools so the AI can discover team switching
      // even when not connected to a Go server.
      const allTools = this.teams.length > 1
        ? [...this.cachedTools, ...SYNTHETIC_TOOLS]
        : this.cachedTools;
      return { tools: allTools };
    });

    server.setRequestHandler(CallToolRequestSchema, async (request) => {
      const { name, arguments: args } = request.params;

      // Synthetic tools are handled locally — they work even when
      // not connected to a Go server.
      if (name === 'asynkor_teams') {
        return this.handleTeamsList();
      }
      if (name === 'asynkor_switch') {
        const result = await this.handleTeamSwitch(args);
        // Re-register notification forwarding after reconnect
        setupNotificationForwarding();
        return result;
      }

      // Hot-reload: pick up new config before the tool call. If the key
      // just appeared (e.g. user ran init mid-session), this is where
      // we connect for the first time.
      await this.checkConfigReload();

      // If multiple teams and none selected, guide the AI to pick one
      if (this.teams.length > 1 && !this.activeTeamSlug) {
        const teamSummaries = this.teams.map(t =>
          `• ${t.slug}${t.name ? ` (${t.name})` : ''}${t.context ? ` — ${t.context}` : ''}`
        );
        return {
          content: [{
            type: 'text' as const,
            text: JSON.stringify({
              status: 'no_team_selected',
              message: 'Multiple teams are configured but none is active. Call asynkor_teams to see them, then asynkor_switch to select one.',
              available_teams: teamSummaries,
            }),
          }],
          isError: true,
        } satisfies CallToolResult;
      }

      if (!this.goClient) {
        return {
          content: [{
            type: 'text' as const,
            text: JSON.stringify({
              error: 'not_connected',
              message: 'Asynkor is not connected yet. Make sure .asynkor.json exists with a valid api_key, or set ASYNKOR_API_KEY. The client will auto-connect within a few seconds.',
            }),
          }],
          isError: true,
        } satisfies CallToolResult;
      }

      let result = await this.goClient.callTool({ name, arguments: args ?? {} });

      // Auto-refresh: if the server reports an invalid key, try to
      // refresh the API key using the stored refresh token, reconnect,
      // and retry the tool call once.
      const resultText = (result.content as Array<{ type: string; text?: string }>)?.[0]?.text ?? '';
      if (result.isError && resultText.includes('invalid_key')) {
        const activeTeam = this.teams.find(t => t.slug === this.activeTeamSlug);
        if (activeTeam) {
          const newKey = await this.refreshApiKey(activeTeam);
          if (newKey) {
            activeTeam.apiKey = newKey;
            this.currentApiKey = newKey;
            try {
              await this.goClient?.close();
            } catch {}
            this.goClient = await this.connectToGoServer();
            setupNotificationForwarding();
            result = await this.goClient.callTool({ name, arguments: args ?? {} });
          }
        }
      }

      return result as CallToolResult;
    });

    server.setRequestHandler(ListResourcesRequestSchema, async () => {
      await this.checkConfigReload();
      if (this.goClient) {
        try {
          const result = await this.goClient.listResources();
          this.cachedResources = result.resources ?? [];
          return result;
        } catch {
          // fall through to cached
        }
      }
      return { resources: this.cachedResources };
    });

    server.setRequestHandler(ReadResourceRequestSchema, async (request) => {
      const { uri } = request.params;

      await this.checkConfigReload();

      if (!this.goClient) {
        return {
          contents: [{ uri, mimeType: 'application/json', text: JSON.stringify({ error: 'disconnected' }) }],
        } satisfies ReadResourceResult;
      }

      const result = await this.goClient.readResource({ uri });
      return result as ReadResourceResult;
    });

    server.setRequestHandler(SubscribeRequestSchema, async (request) => {
      const { uri } = request.params;
      this.startSubscriptionPoll(uri, server);
      return {};
    });

    server.setRequestHandler(UnsubscribeRequestSchema, async (request) => {
      const { uri } = request.params;
      this.stopSubscriptionPoll(uri);
      return {};
    });

    // Start a background config watcher so we pick up key changes even
    // when no tool calls are coming in (e.g. idle session where the
    // user is running init in another terminal).
    const configWatcher = setInterval(() => this.checkConfigReload(), CONFIG_CHECK_INTERVAL_MS);
    const origClose = this.close.bind(this);
    this.close = async () => {
      clearInterval(configWatcher);
      await origClose();
    };

    return server;
  }

  async close(): Promise<void> {
    this.stopping = true;
    for (const timer of this.subscriptions.values()) {
      clearInterval(timer);
    }
    this.subscriptions.clear();
    await this.goClient?.close();
    this.goClient = null;
  }
}
