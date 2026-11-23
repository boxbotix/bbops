// SPDX-License-Identifier: MIT
// Copyright (c) 2026 C360Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from C360Studio/ateam ui/svelte.config.js
// @ 031e68563bf278ea197368929d2e24cfde767eec (MIT).

import adapter from "@sveltejs/adapter-static";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({
      pages: "build",
      assets: "build",
      fallback: "index.html",
      precompress: false,
      strict: true,
    }),
  },
};

export default config;
