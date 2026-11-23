import { describe, expect, it } from "vitest";
import { UXS_DOMAINS, formatShellBanner } from "./bootstrap";

describe("bbops COP shell bootstrap metadata", () => {
  // spec: host-runtime / Svelte 5 COP shell bootstrap metadata
  it("enumerates Quadcopter, Rover, and SurfaceBoat and formats banner deterministically", () => {
    expect(UXS_DOMAINS).toEqual(["Quadcopter", "Rover", "SurfaceBoat"]);
    expect(formatShellBanner("v0.1.0-dev")).toBe(
      "bbops COP/GCS (v0.1.0-dev) — Quadcopter · Rover · SurfaceBoat",
    );
    expect(formatShellBanner("   ")).toBe(
      "bbops COP/GCS (dev) — Quadcopter · Rover · SurfaceBoat",
    );
  });
});
