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
    proxy: {
      "/graph-gateway": "http://127.0.0.1:8080",
      "/graph": "http://127.0.0.1:8080",
      "/health": "http://127.0.0.1:8080",
      "/healthz": "http://127.0.0.1:8080",
      "/readyz": "http://127.0.0.1:8080",
      "/services": "http://127.0.0.1:8080",
      "/components": "http://127.0.0.1:8080",
      "/storage-observability": "http://127.0.0.1:8080",
      "/openapi.json": "http://127.0.0.1:8080",
      "/docs": "http://127.0.0.1:8080",
      "/ws/telemetry": {
        target: "ws://127.0.0.1:8081",
        ws: true,
        rewrite: (path) => path.replace(/^\/ws\/telemetry$/, "/ws"),
      },
      "/ws/graph": {
        target: "ws://127.0.0.1:8082",
        ws: true,
        rewrite: (path) => path.replace(/^\/ws\/graph$/, "/ws"),
      },
    },
  },
  test: {
    include: ["src/**/*.{test,spec}.{js,ts}", "e2e/**/*.{test,spec}.{js,ts}"],
    environment: "jsdom",
    globals: true,
  },
  resolve: {
    conditions: ["browser"],
  },
});
