// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import path from "node:path";
import { fileURLToPath } from "node:url";

export const e2eDir = path.dirname(fileURLToPath(import.meta.url));
export const uiRoot = path.resolve(e2eDir, "..");
export const repoRoot = path.resolve(uiRoot, "..");

/**
 * Resolves a checkout-scoped Playwright output directory so concurrent runs
 * across git worktrees never clobber each other's failure artifacts.
 */
export function resolvePlaywrightOutputDir(
  checkoutUiDir = uiRoot,
  envOutputDir = process.env.BBOPS_PLAYWRIGHT_OUTPUT_DIR,
): string {
  const trimmed = envOutputDir?.trim();
  if (trimmed) {
    return path.resolve(checkoutUiDir, trimmed);
  }
  return path.resolve(checkoutUiDir, "test-results");
}
