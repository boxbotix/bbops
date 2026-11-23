// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from C360Studio/semboids ui/go.mod
// @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).
// Sentinel module: excludes ui/ and stray .go files inside ui/node_modules
// (such as flatted's Go port) from the root module's ./... package set.
module github.com/boxbotix/bbops/ui-exclude

go 1.26.11
