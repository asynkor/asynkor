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
import fs from 'node:fs';
import os from 'node:os';
import type { AsynkorConfig } from './config.js';
import { resolveConfig } from './config.js';

const RECONNECT_BASE_MS = 3000;
const MAX_RECONNECT_ATTEMPTS = 10;
const SUBSCRIBE_POLL_MS = 3000;

// How often to stat the config file for changes. Cheap (~50μs on SSD)
// and negligible compared to the network round-trip of a tool call.
const CONFIG_CHECK_INTERVAL_MS = 3000;

export class AsynkorMcpProxy {
  private goClient: Client | null = null;
  private reconnectAttempts = 0;
  private stopping = false;
  private cachedTools: Tool[] = [];
  private cachedResources: Resource[] = [];
  private subscriptions = new Map<string, ReturnType<typeof setInterval>>();

  // Hot-reload state: tracks what we're currently connected with so we
  // can detect changes and reconnect without restarting the process.
  private currentApiKey: string;
  private currentServerUrl: string;
  private lastConfigCheck = 0;

  constructor(
    private cfg: AsynkorConfig | null,
    private readonly agentName: string = 'claude-code',
    private readonly agentVersion: string = process.env.CLAUDE_CODE_VERSION ?? 'unknown',
  ) {
    this.currentApiKey = cfg?.apiKey ?? '';
    this.currentServerUrl = cfg?.serverUrl ?? 'https://mcp.asynkor.com';
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

    const { config } = resolveConfig();
    const newKey = config?.apiKey ?? '';
    const newUrl = config?.serverUrl ?? 'https://mcp.asynkor.com';

    if (newKey === this.currentApiKey && newUrl === this.currentServerUrl) {
      return; // no change
    }

    const hadKey = !!this.currentApiKey;
    this.currentApiKey = newKey;
    this.currentServerUrl = newUrl;
    this.cfg = config;

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

  async createStdioServer(): Promise<Server> {
    // If we have a config, connect now. If not (no key yet), the server
    // starts in disconnected mode and connects when checkConfigReload()
    // detects the key appearing in .asynkor.json.
    if (this.currentApiKey) {
      this.goClient = await this.connectToGoServer();
      this.cachedTools = (await this.goClient.listTools()).tools;
      try {
        this.cachedResources = (await this.goClient.listResources()).resources ?? [];
      } catch {
        this.cachedResources = [];
      }
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
      if (this.goClient) {
        try {
          const result = await this.goClient.listTools();
          this.cachedTools = result.tools;
          return result;
        } catch {
          // fall through to cached
        }
      }
      return { tools: this.cachedTools };
    });

    server.setRequestHandler(CallToolRequestSchema, async (request) => {
      const { name, arguments: args } = request.params;

      // Hot-reload: pick up new config before the tool call. If the key
      // just appeared (e.g. user ran init mid-session), this is where
      // we connect for the first time.
      await this.checkConfigReload();

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

      const result = await this.goClient.callTool({ name, arguments: args ?? {} });
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
