// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import os from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { resolvePlaywrightOutputDir, uiRoot } from "./paths";

describe("resolvePlaywrightOutputDir", () => {
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  it("resolves a checkout-scoped test-results default without colliding across git worktrees or shared tmpdir", () => {
    expect(resolvePlaywrightOutputDir(uiRoot, undefined)).toBe(
      path.join(uiRoot, "test-results"),
    );
    expect(resolvePlaywrightOutputDir(uiRoot, "   ")).toBe(
      path.join(uiRoot, "test-results"),
    );
    expect(resolvePlaywrightOutputDir(uiRoot, undefined)).not.toBe(
      path.join(os.tmpdir(), "bbops-playwright-results"),
    );

    const wtA = resolvePlaywrightOutputDir("/worktrees/wt-a/ui", undefined);
    const wtB = resolvePlaywrightOutputDir("/worktrees/wt-b/ui", undefined);
    expect(wtA).toBe(path.resolve("/worktrees/wt-a/ui", "test-results"));
    expect(wtB).toBe(path.resolve("/worktrees/wt-b/ui", "test-results"));
    expect(wtA).not.toBe(wtB);
  });

  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  it("honors explicit absolute, checkout-relative, and ambient BBOPS_PLAYWRIGHT_OUTPUT_DIR overrides while keeping explicit undefined pure", () => {
    const absOverride = path.join(os.tmpdir(), "custom-bbops-pw-out");
    expect(resolvePlaywrightOutputDir(uiRoot, absOverride)).toBe(
      path.resolve(absOverride),
    );
    expect(resolvePlaywrightOutputDir(uiRoot, "  artifacts/pw-custom  ")).toBe(
      path.join(uiRoot, "artifacts", "pw-custom"),
    );

    const prevEnv = process.env.BBOPS_PLAYWRIGHT_OUTPUT_DIR;
    process.env.BBOPS_PLAYWRIGHT_OUTPUT_DIR = absOverride;
    try {
      expect(resolvePlaywrightOutputDir()).toBe(path.resolve(absOverride));
      expect(resolvePlaywrightOutputDir(uiRoot)).toBe(
        path.resolve(absOverride),
      );
      expect(resolvePlaywrightOutputDir(uiRoot, undefined)).toBe(
        path.join(uiRoot, "test-results"),
      );
    } finally {
      if (prevEnv === undefined) {
        delete process.env.BBOPS_PLAYWRIGHT_OUTPUT_DIR;
      } else {
        process.env.BBOPS_PLAYWRIGHT_OUTPUT_DIR = prevEnv;
      }
    }
  });
});
