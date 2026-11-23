// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from C360Studio/semboids ui/vite.config.ts
// @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).

import { sveltekit } from "@sveltejs/kit/vite";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [sveltekit()],
  server: {
    port: 5173,
    host: true,
  },
  test: {
    include: ["src/**/*.{test,spec}.{js,ts}"],
    environment: "jsdom",
    globals: true,
  },
  resolve: {
    conditions: ["browser"],
  },
});
