// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Complete 360 Studio
// Copyright (c) 2026 boxbotix
//
// Provenance: Adapted from ForceAtlas2 Web Worker lifecycle controller in
// C360Studio/semstreams-ui src/lib/utils/sigma-layout.ts
// @ 3814b3d59dab7136011429cad99a8148aeceb0cc (MIT).

import type { MultiDirectedGraph } from "graphology";
import forceAtlas2 from "graphology-layout-forceatlas2";
import FA2Layout from "graphology-layout-forceatlas2/worker";

export const DEFAULT_FA2_AUTO_STOP_MS = 3_000;

export class LayoutController {
  private supervisor: FA2Layout | null = null;
  private autoStopTimer: ReturnType<typeof setTimeout> | null = null;
  private readonly onRunningChange?: (running: boolean) => void;

  constructor(onRunningChange?: (running: boolean) => void) {
    this.onRunningChange = onRunningChange;
  }

  start(
    graph: MultiDirectedGraph,
    durationMs = DEFAULT_FA2_AUTO_STOP_MS,
  ): boolean {
    this.stop();
    if (graph.order < 2 || graph.size === 0) {
      return false;
    }
    if (typeof Worker === "undefined") {
      return false;
    }
    try {
      const inferred = forceAtlas2.inferSettings(graph);
      this.supervisor = new FA2Layout(graph, {
        settings: {
          ...inferred,
          gravity: inferred.gravity ?? 1,
          slowDown: 4,
        },
      });
      this.supervisor.start();
      this.onRunningChange?.(true);
      this.autoStopTimer = setTimeout(() => {
        this.stop();
      }, durationMs);
      return true;
    } catch {
      this.stop();
      return false;
    }
  }

  stop(): void {
    if (this.autoStopTimer !== null) {
      clearTimeout(this.autoStopTimer);
      this.autoStopTimer = null;
    }
    if (this.supervisor) {
      try {
        this.supervisor.stop();
        this.supervisor.kill();
      } catch {
        // Ignore worker teardown errors during unmount/headless execution.
      }
      this.supervisor = null;
      this.onRunningChange?.(false);
    }
  }
}
