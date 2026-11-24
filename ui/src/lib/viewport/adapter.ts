// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

export interface WorldPoint {
  readonly ned?: readonly [number, number, number];
  readonly lla?: readonly [number, number, number | null];
}

export interface ScreenProjection {
  readonly x: number;
  readonly y: number;
  readonly visible: boolean;
  readonly rangeM: number;
}

export interface ViewportCameraState {
  readonly centerNed: [number, number];
  readonly zoomPxPerM: number;
  readonly bearingRad: number;
  readonly originLla: [number, number, number] | null;
}

export interface ViewportSize {
  readonly widthPx: number;
  readonly heightPx: number;
}

export interface UnprojectedWorldPoint {
  readonly ned: [number, number, number];
  readonly lla: [number, number, number] | null;
}

/**
 * Renderer-agnostic tactical viewport projection contract (D1 Option A).
 * M2.2 backs this contract with NedGridAdapter; M4.1 (#16) drops in a
 * MapLibre/PMTiles surface adapter underneath the upper 2D safety-HUD canvas
 * without refactoring tactical glyph, crab-angle, or reticle draw code.
 */
export interface ViewportAdapter {
  project(point: WorldPoint): ScreenProjection;
  unproject(xPx: number, yPx: number): UnprojectedWorldPoint;
  screenCourseRad(worldHeadingRad: number): number;
  flyTo(point: WorldPoint, zoomPxPerM?: number): boolean;
  setCamera(patch: Partial<ViewportCameraState>): void;
  setViewportSize(widthPx: number, heightPx: number): void;
  getCamera(): ViewportCameraState;
  getViewportSize(): ViewportSize;
}
