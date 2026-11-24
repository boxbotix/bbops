// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from WebSocket lifecycle patterns in C360Studio/semboids
// ui/src/lib/stores/flock.svelte.ts & ui/src/lib/stores/graph.svelte.ts
// @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).

export const WS_CONNECTING = 0;
export const WS_OPEN = 1;
export const WS_CLOSING = 2;
export const WS_CLOSED = 3;

export const DEFAULT_KEEPALIVE_INTERVAL_MS = 25_000;
export const DEFAULT_INITIAL_BACKOFF_MS = 500;
export const DEFAULT_MAX_BACKOFF_MS = 30_000;

export type WsConnectionStatus =
  "disconnected" | "connecting" | "connected" | "reconnecting";

export interface WebSocketLike {
  readonly readyState: number;
  onopen: ((event: never) => unknown) | null;
  onmessage: ((event: MessageEvent<unknown>) => unknown) | null;
  onerror: ((event: never) => unknown) | null;
  onclose: ((event: CloseEvent) => unknown) | null;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export interface ManagedWsOptions {
  url?: string;
  defaultPath: string;
  envUrl?: string;
  keepaliveIntervalMs?: number;
  initialBackoffMs?: number;
  maxBackoffMs?: number;
  now?: () => number;
  createSocket?: (url: string) => WebSocketLike;
  onStatusChange: (status: WsConnectionStatus) => void;
  onOpen?: () => void;
  onMessage: (rawText: string) => void;
  onDisconnect?: (info: {
    wasConnected: boolean;
    code?: number;
    timestamp: number;
  }) => void;
}

export function resolveWebSocketUrl(
  explicitUrl: string | undefined,
  envUrl: string | undefined,
  defaultPath: string,
): string {
  const candidate = explicitUrl?.trim() || envUrl?.trim();
  if (candidate) {
    return candidate;
  }
  if (typeof window !== "undefined" && window.location?.host) {
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const normalizedPath = defaultPath.startsWith("/")
      ? defaultPath
      : `/${defaultPath}`;
    return `${protocol}//${window.location.host}${normalizedPath}`;
  }
  const normalizedPath = defaultPath.startsWith("/")
    ? defaultPath
    : `/${defaultPath}`;
  return `ws://127.0.0.1:8080${normalizedPath}`;
}

export function computeReconnectDelayMs(
  attempt: number,
  initialBackoffMs = DEFAULT_INITIAL_BACKOFF_MS,
  maxBackoffMs = DEFAULT_MAX_BACKOFF_MS,
): number {
  const safeAttempt = Math.max(0, Math.floor(attempt));
  const raw = initialBackoffMs * Math.pow(2, safeAttempt);
  return Math.min(maxBackoffMs, raw);
}

export class ManagedWebSocketClient {
  private readonly keepaliveIntervalMs: number;
  private readonly initialBackoffMs: number;
  private readonly maxBackoffMs: number;
  private readonly now: () => number;
  private readonly createSocket: (url: string) => WebSocketLike;
  private readonly options: ManagedWsOptions;

  private socket: WebSocketLike | null = null;
  private keepaliveTimer: ReturnType<typeof setInterval> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectAttempt = 0;
  private shouldReconnect = false;
  private activeConnectionEstablished = false;
  private resolvedUrl: string;

  constructor(options: ManagedWsOptions) {
    const keepalive =
      options.keepaliveIntervalMs ?? DEFAULT_KEEPALIVE_INTERVAL_MS;
    if (keepalive <= 0 || keepalive >= 30_000) {
      throw new RangeError(
        `keepaliveIntervalMs (${keepalive}) must be in (0, 30000) ms`,
      );
    }
    this.keepaliveIntervalMs = keepalive;
    this.initialBackoffMs =
      options.initialBackoffMs ?? DEFAULT_INITIAL_BACKOFF_MS;
    this.maxBackoffMs = options.maxBackoffMs ?? DEFAULT_MAX_BACKOFF_MS;
    this.now = options.now ?? (() => Date.now());
    this.createSocket =
      options.createSocket ?? ((targetUrl: string) => new WebSocket(targetUrl));
    this.options = options;
    this.resolvedUrl = resolveWebSocketUrl(
      options.url,
      options.envUrl,
      options.defaultPath,
    );
  }

  get url(): string {
    return this.resolvedUrl;
  }

  get currentBackoffAttempt(): number {
    return this.reconnectAttempt;
  }

  connect(overrideUrl?: string): void {
    const nextUrl =
      overrideUrl !== undefined
        ? resolveWebSocketUrl(
            overrideUrl,
            this.options.envUrl,
            this.options.defaultPath,
          )
        : this.resolvedUrl;
    const urlChanged = nextUrl !== this.resolvedUrl;

    if (
      !urlChanged &&
      this.socket !== null &&
      (this.socket.readyState === WS_CONNECTING ||
        this.socket.readyState === WS_OPEN)
    ) {
      return;
    }

    this.clearKeepaliveTimer();
    this.clearReconnectTimer();

    if (this.socket !== null) {
      const previous = this.socket;
      const wasConnected = this.activeConnectionEstablished;
      this.socket = null;
      this.activeConnectionEstablished = false;
      previous.onopen = null;
      previous.onmessage = null;
      previous.onerror = null;
      previous.onclose = null;
      if (
        previous.readyState === WS_CONNECTING ||
        previous.readyState === WS_OPEN
      ) {
        previous.close(1000, "endpoint override");
      }
      if (wasConnected) {
        this.options.onDisconnect?.({
          wasConnected: true,
          code: 1000,
          timestamp: this.now(),
        });
      }
    }

    if (urlChanged) {
      this.resolvedUrl = nextUrl;
      this.reconnectAttempt = 0;
    }

    this.shouldReconnect = true;
    this.openSocket(this.reconnectAttempt > 0 ? "reconnecting" : "connecting");
  }

  disconnect(): void {
    this.shouldReconnect = false;
    this.reconnectAttempt = 0;
    this.clearKeepaliveTimer();
    this.clearReconnectTimer();
    const current = this.socket;
    this.socket = null;
    this.activeConnectionEstablished = false;
    if (current !== null) {
      current.onopen = null;
      current.onmessage = null;
      current.onerror = null;
      current.onclose = null;
      if (
        current.readyState === WS_CONNECTING ||
        current.readyState === WS_OPEN
      ) {
        current.close(1000, "client disconnect");
      }
    }
    this.options.onStatusChange("disconnected");
  }

  private openSocket(initialStatus: "connecting" | "reconnecting"): void {
    this.clearKeepaliveTimer();
    this.activeConnectionEstablished = false;
    this.options.onStatusChange(initialStatus);

    let ws: WebSocketLike;
    try {
      ws = this.createSocket(this.resolvedUrl);
    } catch {
      this.scheduleReconnect(false, undefined);
      return;
    }
    this.socket = ws;

    ws.onopen = () => {
      if (this.socket !== ws) {
        return;
      }
      this.reconnectAttempt = 0;
      this.activeConnectionEstablished = true;
      this.options.onStatusChange("connected");
      this.options.onOpen?.();
      this.startKeepalive(ws);
    };

    ws.onmessage = (event) => {
      if (this.socket !== ws) {
        return;
      }
      if (typeof event.data === "string") {
        this.options.onMessage(event.data);
      }
    };

    ws.onerror = () => {
      // Browser WebSockets always follow onerror with onclose; reconnect runs in onclose.
    };

    ws.onclose = (event) => {
      if (this.socket !== ws) {
        return;
      }
      const wasConnected = this.activeConnectionEstablished;
      this.socket = null;
      this.activeConnectionEstablished = false;
      this.clearKeepaliveTimer();
      this.options.onDisconnect?.({
        wasConnected,
        code: event?.code,
        timestamp: this.now(),
      });
      if (!this.shouldReconnect) {
        this.options.onStatusChange("disconnected");
        return;
      }
      this.scheduleReconnect(wasConnected, event?.code);
    };
  }

  private startKeepalive(ws: WebSocketLike): void {
    this.clearKeepaliveTimer();
    this.keepaliveTimer = setInterval(() => {
      if (this.socket !== ws || ws.readyState !== WS_OPEN) {
        return;
      }
      try {
        ws.send(
          JSON.stringify({
            type: "keepalive",
            timestamp: this.now(),
          }),
        );
      } catch {
        // Ignore transient write errors; socket close triggers reconnect.
      }
    }, this.keepaliveIntervalMs);
  }

  private scheduleReconnect(_wasConnected: boolean, _code?: number): void {
    this.clearReconnectTimer();
    if (!this.shouldReconnect) {
      this.options.onStatusChange("disconnected");
      return;
    }
    const delayMs = computeReconnectDelayMs(
      this.reconnectAttempt,
      this.initialBackoffMs,
      this.maxBackoffMs,
    );
    this.reconnectAttempt += 1;
    this.options.onStatusChange("reconnecting");
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.shouldReconnect) {
        return;
      }
      this.openSocket("reconnecting");
    }, delayMs);
  }

  private clearKeepaliveTimer(): void {
    if (this.keepaliveTimer !== null) {
      clearInterval(this.keepaliveTimer);
      this.keepaliveTimer = null;
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }
}
