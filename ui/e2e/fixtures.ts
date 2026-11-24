// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import {
  type ChildProcessWithoutNullStreams,
  execFileSync,
  spawn,
} from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import readline from "node:readline";
import { fileURLToPath } from "node:url";
import { test as base, expect, type Page } from "@playwright/test";

export interface StackVehicles {
  readonly quadcopter: string;
  readonly rover: string;
  readonly surfaceBoat: string;
}

export interface StackInfo {
  readonly ready: boolean;
  readonly baseURL: string;
  readonly udpAddr: string;
  readonly org: string;
  readonly platform: string;
  readonly vehicles: StackVehicles;
  readonly externalContactId: string;
  readonly corridorEntityId: string;
}

export interface ExternalContactFixtureInput {
  readonly id: string;
  readonly title: string;
  readonly contact_type: string;
  readonly source?: string;
  readonly feed_status?: string;
  readonly latitude: number;
  readonly longitude: number;
  readonly altitude?: number;
  readonly observed_at?: string;
  readonly received_at?: string;
  readonly expires_at?: string;
  readonly uncertainty_meters?: number;
  readonly monitors_entity_id?: string;
  readonly corridor_entity_id?: string;
}

export interface CopStackFixture {
  readonly page: Page;
  readonly info: StackInfo;
  readonly pageErrors: readonly string[];
  readonly consoleErrors: readonly string[];
  pulseCrsfOverride(sysid: number): Promise<void>;
  emitStatusText(sysid: number, severity: number, text: string): Promise<void>;
  upsertExternalContact(contact: ExternalContactFixtureInput): Promise<void>;
}

interface HarnessReply {
  id?: string;
  ok?: boolean;
  error?: string;
  ready?: boolean;
}

const e2eDir = path.dirname(fileURLToPath(import.meta.url));
const uiRoot = path.resolve(e2eDir, "..");
const repoRoot = path.resolve(uiRoot, "..");

function ensureCompiledUiBuild(): string {
  const configuredDir = process.env.BBOPS_E2E_UI_DIR?.trim();
  const targetDir = configuredDir || path.join(uiRoot, "build");
  const indexHtml = path.join(targetDir, "index.html");
  if (!fs.existsSync(indexHtml)) {
    execFileSync("npm", ["run", "build"], {
      cwd: uiRoot,
      stdio: "inherit",
    });
  }
  return targetDir;
}

class HarnessProcess {
  private readonly proc: ChildProcessWithoutNullStreams;
  private readonly pending = new Map<
    string,
    { resolve: () => void; reject: (err: Error) => void }
  >();
  private nextReqId = 0;
  private stderrBuf = "";

  private constructor(proc: ChildProcessWithoutNullStreams) {
    this.proc = proc;
    proc.stderr.setEncoding("utf8");
    proc.stderr.on("data", (chunk: string) => {
      this.stderrBuf += chunk;
    });
  }

  static async start(uiDir: string): Promise<{
    harness: HarnessProcess;
    info: StackInfo;
  }> {
    const prebuiltBin = process.env.BBOPS_E2E_BIN?.trim();
    const cmd = prebuiltBin || "go";
    const args = prebuiltBin
      ? ["-ui-dir", uiDir]
      : ["run", "./internal/e2eharness/cmd/e2eharness", "-ui-dir", uiDir];

    const proc = spawn(cmd, args, {
      cwd: repoRoot,
      env: { ...process.env, GOTOOLCHAIN: "auto" },
      stdio: ["pipe", "pipe", "pipe"],
    });

    const harness = new HarnessProcess(proc);
    const rl = readline.createInterface({ input: proc.stdout });

    return await new Promise<{ harness: HarnessProcess; info: StackInfo }>(
      (resolve, reject) => {
        let initialized = false;
        const startupTimer = setTimeout(() => {
          if (!initialized) {
            proc.kill("SIGTERM");
            reject(
              new Error(`Timed out starting e2eharness:\n${harness.stderrBuf}`),
            );
          }
        }, 30_000);

        proc.once("exit", (code, sig) => {
          clearTimeout(startupTimer);
          const err = new Error(
            `e2eharness exited (code=${String(code)}, signal=${String(sig)}):\n${harness.stderrBuf}`,
          );
          if (!initialized) {
            reject(err);
            return;
          }
          for (const entry of harness.pending.values()) {
            entry.reject(err);
          }
          harness.pending.clear();
        });

        rl.on("line", (line) => {
          if (!line.trim()) {
            return;
          }
          let parsed: HarnessReply & Partial<StackInfo>;
          try {
            parsed = JSON.parse(line) as HarnessReply & Partial<StackInfo>;
          } catch {
            return;
          }
          if (
            !initialized &&
            parsed.ready &&
            parsed.baseURL &&
            parsed.vehicles
          ) {
            initialized = true;
            clearTimeout(startupTimer);
            resolve({ harness, info: parsed as StackInfo });
            return;
          }
          if (parsed.id) {
            const waiter = harness.pending.get(parsed.id);
            if (waiter) {
              harness.pending.delete(parsed.id);
              if (parsed.ok) {
                waiter.resolve();
              } else {
                waiter.reject(
                  new Error(parsed.error ?? "e2eharness action failed"),
                );
              }
            }
          }
        });
      },
    );
  }

  async sendCommand(payload: Record<string, unknown>): Promise<void> {
    this.nextReqId += 1;
    const id = `req-${this.nextReqId}`;
    const line = `${JSON.stringify({ id, ...payload })}\n`;
    await new Promise<void>((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.proc.stdin.write(line, (err) => {
        if (err) {
          this.pending.delete(id);
          reject(err);
        }
      });
    });
  }

  async stop(): Promise<void> {
    if (this.proc.exitCode !== null || this.proc.killed) {
      return;
    }
    const exitPromise = new Promise<void>((resolve) => {
      this.proc.once("exit", () => resolve());
    });
    try {
      await Promise.race([
        this.sendCommand({ action: "shutdown" }),
        new Promise<void>((resolve) => setTimeout(resolve, 2_000)),
      ]);
    } catch {
      // Ignore shutdown write errors when child already began exiting.
    }
    this.proc.stdin.end();
    await Promise.race([
      exitPromise,
      new Promise<void>((resolve) => {
        setTimeout(() => {
          if (this.proc.exitCode === null) {
            this.proc.kill("SIGKILL");
          }
          resolve();
        }, 8_000);
      }),
    ]);
  }
}

export const test = base.extend<{ copStack: CopStackFixture }>({
  copStack: async ({ page }, use) => {
    const uiDir = ensureCompiledUiBuild();
    const { harness, info } = await HarnessProcess.start(uiDir);
    const pageErrors: string[] = [];
    const consoleErrors: string[] = [];

    page.on("pageerror", (err) => {
      pageErrors.push(err.stack ?? err.message);
    });
    page.on("console", (msg) => {
      if (msg.type() === "error") {
        consoleErrors.push(msg.text());
      }
    });

    try {
      await use({
        page,
        info,
        pageErrors,
        consoleErrors,
        pulseCrsfOverride: (sysid) =>
          harness.sendCommand({ action: "pulseCrsfOverride", sysid }),
        emitStatusText: (sysid, severity, text) =>
          harness.sendCommand({
            action: "emitStatusText",
            sysid,
            severity,
            text,
          }),
        upsertExternalContact: (contact) =>
          harness.sendCommand({ action: "upsertExternalContact", contact }),
      });
    } finally {
      await harness.stop();
    }
  },
});

export { expect };
