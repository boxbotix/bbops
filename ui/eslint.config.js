// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from C360Studio/semboids ui/eslint.config.js
// @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).

import js from "@eslint/js";
import tseslint from "@typescript-eslint/eslint-plugin";
import tsparser from "@typescript-eslint/parser";
import svelte from "eslint-plugin-svelte";
import prettier from "eslint-config-prettier";
import globals from "globals";

export default [
  js.configs.recommended,
  ...svelte.configs["flat/recommended"],
  {
    files: ["**/*.svelte"],
    languageOptions: {
      parser: svelte.parser,
      parserOptions: {
        parser: tsparser,
      },
      globals: {
        ...globals.browser,
      },
    },
    plugins: {
      "@typescript-eslint": tseslint,
    },
    rules: {
      "no-unused-vars": "off",
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
    },
  },
  {
    files: ["**/*.{ts,tsx}", "**/*.svelte.ts"],
    languageOptions: {
      parser: tsparser,
      parserOptions: {
        ecmaVersion: 2022,
        sourceType: "module",
        extraFileExtensions: [".svelte"],
      },
      globals: {
        ...globals.browser,
        ...globals.node,
        $state: "readonly",
        $derived: "readonly",
        $effect: "readonly",
        $props: "readonly",
        $bindable: "readonly",
        $inspect: "readonly",
        $host: "readonly",
      },
    },
    plugins: {
      "@typescript-eslint": tseslint,
    },
    rules: {
      ...tseslint.configs.recommended.rules,
      "no-unused-vars": "off",
      "no-undef": "off",
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
    },
  },
  prettier,
  {
    ignores: ["build/", ".svelte-kit/", "node_modules/"],
  },
];
