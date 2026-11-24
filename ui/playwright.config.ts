// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import { defineConfig, devices } from "@playwright/test";
import { resolvePlaywrightOutputDir } from "./e2e/paths";

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.e2e.ts",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  expect: {
    timeout: 10_000,
  },
  reporter: "list",
  outputDir: resolvePlaywrightOutputDir(),
  use: {
    ...devices["Desktop Chrome"],
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 2,
    headless: true,
    trace: "off",
    video: "off",
    screenshot: "off",
    launchOptions: {
      args: [
        "--enable-webgl",
        "--ignore-gpu-blocklist",
        "--use-gl=angle",
        "--use-angle=swiftshader",
      ],
    },
  },
});
