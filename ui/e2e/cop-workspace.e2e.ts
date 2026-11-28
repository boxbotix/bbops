// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import type { Locator, Page, WebSocketRoute } from "@playwright/test";
import { expect, test } from "./fixtures";

interface BoxRect {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

function boxesOverlap(a: BoxRect, b: BoxRect): boolean {
  const eps = 0.5;
  return (
    a.x + eps < b.x + b.width &&
    a.x + a.width - eps > b.x &&
    a.y + eps < b.y + b.height &&
    a.y + a.height - eps > b.y
  );
}

async function assertZeroPairwiseOverlapWithinParent(
  parent: Locator,
  cells: readonly Locator[],
): Promise<void> {
  const parentBox = await parent.boundingBox();
  expect(parentBox).not.toBeNull();
  const boxes: BoxRect[] = [];
  for (const cell of cells) {
    await expect(cell).toBeVisible();
    const box = await cell.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.width).toBeGreaterThan(0);
    expect(box!.height).toBeGreaterThan(0);
    expect(box!.x).toBeGreaterThanOrEqual(parentBox!.x - 1);
    expect(box!.y).toBeGreaterThanOrEqual(parentBox!.y - 1);
    expect(box!.x + box!.width).toBeLessThanOrEqual(
      parentBox!.x + parentBox!.width + 1,
    );
    expect(box!.y + box!.height).toBeLessThanOrEqual(
      parentBox!.y + parentBox!.height + 1,
    );
    boxes.push(box!);
  }
  for (let i = 0; i < boxes.length; i += 1) {
    for (let j = i + 1; j < boxes.length; j += 1) {
      expect(boxesOverlap(boxes[i]!, boxes[j]!)).toBe(false);
    }
  }
}

async function sampleCanvas2DPixels(
  page: Page,
  selector: string,
  bgRgb: readonly [number, number, number],
): Promise<{
  readonly width: number;
  readonly height: number;
  readonly clientWidth: number;
  readonly clientHeight: number;
  readonly nonBgPixels: number;
  readonly pureWhitePixels: number;
}> {
  return await page.locator(selector).evaluate((el, bg) => {
    const canvas = el as HTMLCanvasElement;
    const ctx = canvas.getContext("2d");
    if (!ctx || canvas.width === 0 || canvas.height === 0) {
      return {
        width: canvas.width,
        height: canvas.height,
        clientWidth: canvas.clientWidth,
        clientHeight: canvas.clientHeight,
        nonBgPixels: 0,
        pureWhitePixels: 0,
      };
    }
    const { data } = ctx.getImageData(0, 0, canvas.width, canvas.height);
    let nonBgPixels = 0;
    let pureWhitePixels = 0;
    // Stride by 4 pixels (16 bytes) for fast full-frame coverage.
    for (let idx = 0; idx < data.length; idx += 16) {
      const r = data[idx]!;
      const g = data[idx + 1]!;
      const b = data[idx + 2]!;
      const a = data[idx + 3]!;
      if (a > 0) {
        const diff =
          Math.abs(r - bg[0]) + Math.abs(g - bg[1]) + Math.abs(b - bg[2]);
        if (diff > 18) {
          nonBgPixels += 1;
        }
        if (a > 220 && r >= 250 && g >= 250 && b >= 250) {
          pureWhitePixels += 1;
        }
      }
    }
    return {
      width: canvas.width,
      height: canvas.height,
      clientWidth: canvas.clientWidth,
      clientHeight: canvas.clientHeight,
      nonBgPixels,
      pureWhitePixels,
    };
  }, bgRgb);
}

interface SigmaHoverSample {
  readonly exists: boolean;
  readonly darkPillPixels: number;
  readonly lightTextPixels: number;
  readonly pureWhitePixels: number;
  readonly centroidX: number;
  readonly centroidY: number;
}

async function sampleSigmaHoverLayer(
  sigmaStage: Locator,
): Promise<SigmaHoverSample> {
  return await sigmaStage.evaluate((stageEl) => {
    const hoverCanvas = stageEl.querySelector(
      "canvas.sigma-hovers",
    ) as HTMLCanvasElement | null;
    if (!hoverCanvas || hoverCanvas.width === 0 || hoverCanvas.height === 0) {
      return {
        exists: false,
        darkPillPixels: 0,
        lightTextPixels: 0,
        pureWhitePixels: 0,
        centroidX: 0,
        centroidY: 0,
      };
    }
    const ctx = hoverCanvas.getContext("2d");
    if (!ctx) {
      return {
        exists: false,
        darkPillPixels: 0,
        lightTextPixels: 0,
        pureWhitePixels: 0,
        centroidX: 0,
        centroidY: 0,
      };
    }
    const { width, height } = hoverCanvas;
    const scaleX =
      hoverCanvas.clientWidth > 0 ? hoverCanvas.clientWidth / width : 0.5;
    const scaleY =
      hoverCanvas.clientHeight > 0 ? hoverCanvas.clientHeight / height : 0.5;
    const { data } = ctx.getImageData(0, 0, width, height);
    let darkPillPixels = 0;
    let lightTextPixels = 0;
    let pureWhitePixels = 0;
    let sumX = 0;
    let sumY = 0;
    for (let y = 0; y < height; y += 1) {
      for (let x = 0; x < width; x += 1) {
        const idx = (y * width + x) * 4;
        const a = data[idx + 3]!;
        if (a < 180) {
          continue;
        }
        const r = data[idx]!;
        const g = data[idx + 1]!;
        const b = data[idx + 2]!;
        // #0f172a = rgb(15, 23, 42)
        if (
          Math.abs(r - 15) <= 4 &&
          Math.abs(g - 23) <= 4 &&
          Math.abs(b - 42) <= 4
        ) {
          darkPillPixels += 1;
          sumX += x * scaleX;
          sumY += y * scaleY;
        }
        // #f8fafc = rgb(248, 250, 252) (or antialiased blend toward #0f172a)
        if (r >= 200 && g >= 205 && b >= 210) {
          lightTextPixels += 1;
        }
        // Sigma's unstyled default white hover background (#ffffff = 255,255,255)
        if (r === 255 && g === 255 && b === 255) {
          pureWhitePixels += 1;
        }
      }
    }
    return {
      exists: true,
      darkPillPixels,
      lightTextPixels,
      pureWhitePixels,
      centroidX: darkPillPixels > 0 ? sumX / darkPillPixels : 0,
      centroidY: darkPillPixels > 0 ? sumY / darkPillPixels : 0,
    };
  });
}

async function locateSigmaNodeViaWebGLPicking(
  sigmaStage: Locator,
  targetEntityId: string,
): Promise<{ readonly x: number; readonly y: number } | null> {
  return await sigmaStage.evaluate((stageEl, entityId) => {
    const mouseCanvas = stageEl.querySelector(
      "canvas.sigma-mouse",
    ) as HTMLCanvasElement | null;
    if (!mouseCanvas) {
      return null;
    }
    const bbox = mouseCanvas.getBoundingClientRect();
    let hitCount = 0;
    let sumX = 0;
    let sumY = 0;
    for (let y = 12; y < bbox.height - 12; y += 5) {
      for (let x = 12; x < bbox.width - 12; x += 5) {
        mouseCanvas.dispatchEvent(
          new MouseEvent("mousemove", {
            bubbles: true,
            clientX: bbox.left + x,
            clientY: bbox.top + y,
          }),
        );
        if (stageEl.getAttribute("data-hovered-node") === entityId) {
          hitCount += 1;
          sumX += x;
          sumY += y;
        }
      }
    }
    // Clear hover state before returning so real Playwright click drives selection cleanly.
    mouseCanvas.dispatchEvent(
      new MouseEvent("mousemove", {
        bubbles: true,
        clientX: -100,
        clientY: -100,
      }),
    );
    if (hitCount === 0) {
      return null;
    }
    return { x: sumX / hitCount, y: sumY / hitCount };
  }, targetEntityId);
}

test.describe("Full-stack Chromium 4-workspace COP/GCS end-to-end verification", () => {
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  test("streams live multi-domain dual-WebSocket telemetry and SKG state across Fleet, Vehicle, Analysis, and Diagnostics workspaces", async ({
    copStack,
  }) => {
    const { page, info } = copStack;
    await page.goto(info.baseURL);

    // 1. Both live WebSockets converge in the invariant topbar against the embedded Go host + NATS/JetStream SKG.
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "cop",
    );
    await expect(page.getByTestId("stream-pill-telemetry")).toContainText(
      "Telemetry: Live (3)",
      { timeout: 15_000 },
    );
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      "Fleet data: Live · synced",
      { timeout: 15_000 },
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toHaveText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );
    await expect(page.getByTestId("fleet-filmstrip")).not.toContainText("SAFE");

    // Verify approved workspace labels (Fleet, Vehicle, Diagnostics, Analysis) expose accessible names and render unclipped at 1440x900 and 1024x768.
    const workspaceNav = page.getByRole("navigation", {
      name: "Task workspaces",
    });
    const fleetTab = workspaceNav.getByRole("button", { name: /^Fleet\b/ });
    const vehicleTab = workspaceNav.getByRole("button", { name: /^Vehicle\b/ });
    const diagnosticsTab = workspaceNav.getByRole("button", {
      name: /^Diagnostics\b/,
    });
    const analysisTab = workspaceNav.getByRole("button", {
      name: /^Analysis\b/,
    });
    const workspaceTabs = [fleetTab, vehicleTab, diagnosticsTab, analysisTab];

    for (const vp of [
      { width: 1024, height: 768 },
      { width: 1440, height: 900 },
    ] as const) {
      await page.setViewportSize(vp);
      await assertZeroPairwiseOverlapWithinParent(workspaceNav, workspaceTabs);
      for (const tab of workspaceTabs) {
        const clipped = await tab.evaluate((el) => {
          const btn = el as HTMLElement;
          const span = btn.querySelector("span") as HTMLElement | null;
          return (
            btn.scrollWidth > btn.clientWidth + 1 ||
            (span ? span.scrollWidth > span.clientWidth + 1 : false)
          );
        });
        expect(clipped).toBe(false);
      }
    }

    // 2. COP TacticalCanvas DPR=2 layers paint non-blank NED grid & vehicle glyphs, and concentric range-ring labels clear .viewport-toolbar.
    await expect
      .poll(async () => {
        const grid = await sampleCanvas2DPixels(
          page,
          ".surface-grid-canvas",
          [9, 13, 22],
        );
        const hudLayer = await sampleCanvas2DPixels(
          page,
          ".safety-hud-canvas",
          [0, 0, 0],
        );
        return (
          grid.width === Math.floor(grid.clientWidth * 2) &&
          hudLayer.width === Math.floor(hudLayer.clientWidth * 2) &&
          grid.nonBgPixels > 50 &&
          hudLayer.nonBgPixels > 50
        );
      })
      .toBe(true);

    const toolbarBox = await page.getByTestId("viewport-toolbar").boundingBox();
    const gridCanvasBox = await page
      .locator(".surface-grid-canvas")
      .boundingBox();
    expect(toolbarBox).not.toBeNull();
    expect(gridCanvasBox).not.toBeNull();
    expect(toolbarBox!.height).toBeGreaterThan(20);

    const toolbarClearance = await page
      .locator(".surface-grid-canvas")
      .evaluate(
        (el, localToolbar) => {
          const canvas = el as HTMLCanvasElement;
          const ctx = canvas.getContext("2d")!;
          const dprX = canvas.width / canvas.clientWidth;
          const dprY = canvas.height / canvas.clientHeight;
          const { data, width, height } = ctx.getImageData(
            0,
            0,
            canvas.width,
            canvas.height,
          );

          const tbLeft = Math.max(0, Math.floor(localToolbar.left * dprX));
          const tbRight = Math.min(width, Math.ceil(localToolbar.right * dprX));
          const tbTop = Math.max(0, Math.floor(localToolbar.top * dprY));
          const tbBottom = Math.min(
            height,
            Math.ceil(localToolbar.bottom * dprY),
          );
          const cxPx = Math.round(width * 0.5);

          let ringArcPixelsInToolbarBand = 0;
          let glyphPixelsUnderToolbar = 0;
          let eastAxisLabelPixels = 0;

          for (let y = tbTop; y < tbBottom; y += 1) {
            for (let x = tbLeft; x < tbRight; x += 1) {
              const idx = (y * width + x) * 4;
              const r = data[idx]!;
              const g = data[idx + 1]!;
              const b = data[idx + 2]!;
              // Ring stroke rgba(51,65,85,0.45) over #090d16 blends to ~rgb(28,36,50); crosshair blends to ~rgb(33,56,86).
              if (Math.abs(x - cxPx) > 12 && b >= 42 && g < 52) {
                ringArcPixelsInToolbarBand += 1;
              }
              // Range-ring labels rgba(148,163,184,0.72) blend to ~rgb(109,121,139) and North "N" #60a5fa has G=165.
              if (g >= 80 || r >= 70) {
                glyphPixelsUnderToolbar += 1;
              }
            }
          }

          // East-axis relocated range-ring label ("100m") sits to the right of cx and strictly below toolbarBottom.
          for (let y = tbBottom; y < height; y += 1) {
            for (let x = cxPx + Math.round(60 * dprX); x < width; x += 1) {
              const idx = (y * width + x) * 4;
              const r = data[idx]!;
              const g = data[idx + 1]!;
              if (r >= 75 && g >= 85) {
                eastAxisLabelPixels += 1;
              }
            }
          }

          return {
            ringArcPixelsInToolbarBand,
            glyphPixelsUnderToolbar,
            eastAxisLabelPixels,
          };
        },
        {
          left: toolbarBox!.x - gridCanvasBox!.x,
          right: toolbarBox!.x + toolbarBox!.width - gridCanvasBox!.x,
          top: toolbarBox!.y - gridCanvasBox!.y,
          bottom: toolbarBox!.y + toolbarBox!.height - gridCanvasBox!.y,
        },
      );
    expect(toolbarClearance.ringArcPixelsInToolbarBand).toBeGreaterThan(20);
    expect(toolbarClearance.glyphPixelsUnderToolbar).toBe(0);
    expect(toolbarClearance.eastAxisLabelPixels).toBeGreaterThan(20);

    // 3. Hit-test Quadcopter #1 directly on COP TacticalCanvas (NED [24, -14] at default 4 px/m zoom).
    const initialCanvasSize = await page
      .locator(".safety-hud-canvas")
      .evaluate((el) => ({ w: el.clientWidth, h: el.clientHeight }));
    await page.locator(".safety-hud-canvas").click({
      position: {
        x: initialCanvasSize.w * 0.5 + -14 * 4,
        y: initialCanvasSize.h * 0.5 - 24 * 4,
      },
    });
    await expect
      .poll(() => new URL(page.url()).hash)
      .toContain(`sel=${info.vehicles.quadcopter}`);

    const authBanner = page.getByTestId("vehicle-authority-banner");
    await expect(authBanner).toContainText("UXS #1 · quadcopter");
    await expect(page.getByTestId("authority-badge")).toHaveText(
      "P2 CompanionTeleop",
    );
    await expect(page.getByTestId("armed-badge")).toHaveText("ARMED");
    await expect(page.getByTestId("banner-link-tier")).toHaveText("SITL");
    await expect(page.getByTestId("cop-solo-panel-summary")).toContainText(
      "UXS #1",
    );
    await expect(
      page
        .getByTestId("cop-solo-panel-summary")
        .getByRole("button", { name: "Vehicle [D]" }),
    ).toBeVisible();
    await expect(
      page
        .getByTestId("cop-solo-panel-summary")
        .getByRole("button", { name: "Diagnostics [B]" }),
    ).toBeVisible();
    await expect(
      page
        .getByTestId("cop-solo-panel-summary")
        .getByRole("button", { name: "Analysis [K]" }),
    ).toBeVisible();

    // Exercise COP canvas double-click tether plus keyboard hotkeys T (Tether), C (Chase), H (Declutter), and Escape (Clear).
    const tetherBtn = page.getByRole("button", { name: "Tether [T]" });
    const chaseBtn = page.getByRole("button", { name: "Chase [C]" });
    const liveCanvasSize = await page
      .locator(".safety-hud-canvas")
      .evaluate((el) => ({ w: el.clientWidth, h: el.clientHeight }));
    await page.locator(".safety-hud-canvas").dblclick({
      position: {
        x: liveCanvasSize.w * 0.5 + 28 * 4,
        y: liveCanvasSize.h * 0.5 - -18 * 4,
      },
    });
    await expect(tetherBtn).toHaveAttribute("aria-pressed", "true");
    await expect
      .poll(() => new URL(page.url()).hash)
      .toContain(`tether=${info.vehicles.rover}`);

    await page.keyboard.press("t");
    await expect(tetherBtn).toHaveAttribute("aria-pressed", "false");
    await page.keyboard.press("t");
    await expect(tetherBtn).toHaveAttribute("aria-pressed", "true");

    await page.keyboard.press("c");
    await expect(chaseBtn).toHaveAttribute("aria-pressed", "true");

    await page.keyboard.press("h");
    await expect(
      page.getByRole("button", { name: "minimal", exact: true }),
    ).toHaveAttribute("aria-pressed", "true");
    await page.keyboard.press("h");
    await expect(
      page.getByRole("button", { name: "tactical", exact: true }),
    ).toHaveAttribute("aria-pressed", "true");
    await page.keyboard.press("h");
    await expect(
      page.getByRole("button", { name: "none", exact: true }),
    ).toHaveAttribute("aria-pressed", "true");

    await page.keyboard.press("Escape");
    await expect(tetherBtn).toHaveAttribute("aria-pressed", "false");
    await expect(chaseBtn).toHaveAttribute("aria-pressed", "false");
    await expect(authBanner).toContainText("UXS #2 · rover");
    expect(new URL(page.url()).hash).not.toContain("tether=");

    // 4. Switch to Vehicle [D], select UXS #1 via FleetFilmstrip [1], and verify DPR=2 horizon, [M] split swap, and zero-overlap readouts.
    await page.keyboard.press("1");
    await expect(authBanner).toContainText("UXS #1 · quadcopter");
    await page.keyboard.press("d");
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "pilot",
    );
    await expect(page.locator(".pilot-mode-label")).toContainText(
      "VEHICLE WORKSPACE",
    );
    await expect(page.getByTestId("pilot-map-primary")).toBeVisible();

    await expect
      .poll(async () => {
        const horizon = await sampleCanvas2DPixels(
          page,
          ".hud-canvas",
          [9, 13, 22],
        );
        return (
          horizon.width === Math.floor(horizon.clientWidth * 2) &&
          horizon.nonBgPixels > 100
        );
      })
      .toBe(true);

    const readoutStrip = page.getByTestId("hud-readout-strip");
    await expect(readoutStrip).toBeVisible();
    await expect(page.getByTestId("hud-readout-alt").locator("dd")).toHaveText(
      "128.5 m",
    );
    await expect(
      page.getByTestId("hud-readout-attitude").locator("dd"),
    ).toHaveText("14° / -8° / 132°");
    await expect(page.getByTestId("hud-readout-crab")).toHaveCount(0);

    await assertZeroPairwiseOverlapWithinParent(readoutStrip, [
      page.getByTestId("hud-readout-gs"),
      page.getByTestId("hud-readout-vs"),
      page.getByTestId("hud-readout-alt"),
      page.getByTestId("hud-readout-attitude"),
    ]);

    // Swap Pilot to 50/50 Split via [M] and select SurfaceBoat #3 via [3] to verify signed Fossen Ch. 2 crab-angle (-30.1°) with zero overlap.
    await page.keyboard.press("m");
    await expect(page.getByTestId("pilot-split-view")).toBeVisible();
    await expect.poll(() => new URL(page.url()).hash).toContain("pilot=split");

    await page.keyboard.press("3");
    await expect(authBanner).toContainText("UXS #3 · surface-boat");
    const crabCell = page.getByTestId("hud-readout-crab");
    await expect(crabCell).toBeVisible();
    await expect(crabCell.locator("dd")).toHaveText("-30.1°");

    await assertZeroPairwiseOverlapWithinParent(readoutStrip, [
      page.getByTestId("hud-readout-gs"),
      page.getByTestId("hud-readout-vs"),
      page.getByTestId("hud-readout-alt"),
      page.getByTestId("hud-readout-attitude"),
      crabCell,
    ]);

    // 5. Open Cortex [K] unselected first so all 5 SKG entities mount in real WebGL Sigma (DPR=2, dark #0f172a hover pill, zero #ffffff white-on-white).
    await page.goto(`${info.baseURL}/#mode=cortex`);
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "cortex",
    );
    await expect(page.locator(".stream-pill").nth(1)).toContainText("synced", {
      timeout: 15_000,
    });
    await expect(page.getByTestId("cortex-subgraph-fallback")).toHaveCount(0);

    const sigmaStage = page.getByTestId("cortex-sigma-host");
    await expect(sigmaStage).toBeVisible();
    await expect(sigmaStage).toHaveAttribute("data-sigma-ready", "true", {
      timeout: 10_000,
    });
    await expect(page.getByTestId("cortex-causal-subgraph")).toContainText(
      "5 nodes · 2 edges",
      { timeout: 10_000 },
    );
    await expect(page.getByTestId("cortex-causal-subgraph")).not.toContainText(
      "settling",
      { timeout: 10_000 },
    );

    const sigmaCanvasMetrics = await sigmaStage.evaluate((stageEl) => {
      const canvases = Array.from(stageEl.querySelectorAll("canvas"));
      return {
        canvasCount: canvases.length,
        allDprScaled: canvases.every(
          (c) =>
            c.width === Math.floor(c.clientWidth * 2) &&
            c.height === Math.floor(c.clientHeight * 2),
        ),
      };
    });
    expect(sigmaCanvasMetrics.canvasCount).toBeGreaterThanOrEqual(2);
    expect(sigmaCanvasMetrics.allDprScaled).toBe(true);

    // Unselected Sigma hover layer starts blank (proving highlight pill pixels come exclusively from selection/hover).
    const unselectedHover = await sampleSigmaHoverLayer(sigmaStage);
    expect(unselectedHover.exists).toBe(true);
    expect(unselectedHover.darkPillPixels).toBe(0);
    expect(unselectedHover.lightTextPixels).toBe(0);

    // Click Rover #2 directly on the real WebGL Sigma stage and verify dark #0f172a highlight pill + 23 grounded triples.
    const roverSigmaPos = await locateSigmaNodeViaWebGLPicking(
      sigmaStage,
      info.vehicles.rover,
    );
    expect(roverSigmaPos).not.toBeNull();
    await sigmaStage
      .locator("canvas.sigma-mouse")
      .click({ position: roverSigmaPos! });
    await page.mouse.move(8, 8);

    await expect
      .poll(() => new URL(page.url()).hash)
      .toContain(`sel=${info.vehicles.rover}`);
    await expect(authBanner).toContainText("UXS #2 · rover");
    await expect(page.getByTestId("authority-badge")).toHaveText(
      "P3 CompanionAutonomous",
    );
    await expect(page.getByTestId("cortex-provenance-column")).toContainText(
      "GROUNDED PROVENANCE (23)",
    );
    await expect(page.getByTestId("cortex-provenance-column")).toContainText(
      "companion-autonomous",
    );
    await expect(sigmaStage).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.rover,
    );

    let roverHover = await sampleSigmaHoverLayer(sigmaStage);
    await expect
      .poll(async () => {
        roverHover = await sampleSigmaHoverLayer(sigmaStage);
        return (
          roverHover.darkPillPixels > 80 &&
          roverHover.lightTextPixels > 8 &&
          roverHover.pureWhitePixels === 0
        );
      })
      .toBe(true);

    // Select Quadcopter #1 via [1] and run Cortex "2-Hop & Similar" (POST /graph-gateway/search + POST /graph-gateway/graphql pathSearch).
    await page.keyboard.press("1");
    await expect(authBanner).toContainText("UXS #1 · quadcopter");
    await expect(sigmaStage).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.quadcopter,
    );

    const expandButton = page.getByTestId("cortex-similar-btn");
    await expect(expandButton).toBeEnabled();
    const [gqlResponse] = await Promise.all([
      page.waitForResponse(
        (resp) =>
          resp.url().includes("/graph-gateway/graphql") &&
          resp.request().method() === "POST",
      ),
      expandButton.click(),
    ]);
    expect(gqlResponse.status()).toBe(200);
    const gqlPayload = (await gqlResponse.json()) as {
      data?: {
        pathSearch?: {
          entities?: Array<{ id: string; depth: number }>;
        };
      };
      errors?: unknown[];
    };
    expect(gqlPayload.errors ?? []).toEqual([]);
    const discoveredIds = (gqlPayload.data?.pathSearch?.entities ?? []).map(
      (item) => item.id,
    );
    expect(discoveredIds).toContain(info.externalContactId);
    expect(discoveredIds).toContain(info.corridorEntityId);
    await expect(page.getByTestId("cortex-error-banner")).toHaveCount(0);
    await expect(page.getByTestId("cortex-causal-subgraph")).toContainText(
      "3 nodes · 2 edges",
    );

    await expect
      .poll(async () => {
        const quadHover = await sampleSigmaHoverLayer(sigmaStage);
        return (
          quadHover.darkPillPixels > 80 &&
          quadHover.lightTextPixels > 8 &&
          quadHover.pureWhitePixels === 0
        );
      })
      .toBe(true);

    // 6. Switch to Bench [B], verify DPR=2 20 Hz oscilloscope, and inject live Lane 3 P1 CRSF override + STATUSTEXT transitions.
    await page.keyboard.press("b");
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "bench",
    );
    await expect
      .poll(async () => {
        const scope = await sampleCanvas2DPixels(
          page,
          '[data-testid="bench-oscilloscope"]',
          [9, 13, 22],
        );
        return (
          scope.width === Math.floor(scope.clientWidth * 2) &&
          scope.nonBgPixels > 50
        );
      })
      .toBe(true);

    await page.keyboard.press("2");
    await expect(authBanner).toContainText("UXS #2 · rover");

    await copStack.pulseCrsfOverride(1);
    await copStack.emitStatusText(3, 4, "CROSS CURRENT DRIFT");

    const timeline = page.getByTestId("transition-event-timeline");
    await expect(timeline).toContainText("CROSS CURRENT DRIFT", {
      timeout: 10_000,
    });
    await expect(timeline).toContainText("direct-crsf-override", {
      timeout: 10_000,
    });

    const sys1EventBtn = timeline
      .locator("button.event-btn")
      .filter({ hasText: "SYS#1" })
      .first();
    await sys1EventBtn.click();

    await expect(authBanner).toContainText("UXS #1 · quadcopter");
    await expect(page.getByTestId("authority-badge")).toHaveText(
      "P1 DirectCrsfOverride",
      { timeout: 10_000 },
    );
    await expect(page.getByTestId("authority-badge")).toHaveClass(
      /severity-warning/,
    );

    expect(copStack.pageErrors).toEqual([]);
    expect(copStack.consoleErrors).toEqual([]);
  });

  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  // spec: cop-ui / Task-modal 4-workspace shell, invariant safety topbar, and Fleet Filmstrip
  test("preserves strict UXS authority, HUD, and actuation isolation and distinct observed/received freshness across Pilot and Bench for full and partial external COP contacts", async ({
    copStack,
  }) => {
    const { page, info } = copStack;

    // 1. Delayed external contact with distinct known observed_at vs received_at, expires_at, and uncertainty_meters.
    const delayedContactId = `${info.org}.${info.platform}.sensor.cop.vessel.delayed-9`;
    const observedIso = "2030-01-15T12:00:00.000Z";
    const receivedIso = "2030-01-15T12:00:12.500Z";
    const expiresIso = "2030-01-15T12:05:00.000Z";
    await copStack.upsertExternalContact({
      id: delayedContactId,
      title: "AIS Delayed Tanker-9",
      contact_type: "cargo-vessel",
      source: "ais-coastal-relay",
      feed_status: "degraded",
      latitude: 37.772,
      longitude: -122.423,
      observed_at: observedIso,
      received_at: receivedIso,
      expires_at: expiresIso,
      uncertainty_meters: 18.5,
    });

    // 2. Partial-metadata external contact with known observed_at and omitted received_at, expires_at, and uncertainty_meters.
    const partialContactId = `${info.org}.${info.platform}.sensor.cop.vessel.partial-4`;
    const partialObservedIso = "2030-01-15T12:02:00.000Z";
    await copStack.upsertExternalContact({
      id: partialContactId,
      title: "Radar Track Partial-4",
      contact_type: "unknown-surface",
      source: "harbor-radar-2",
      feed_status: "stale",
      latitude: 37.771,
      longitude: -122.421,
      observed_at: partialObservedIso,
    });

    await page.goto(
      `${info.baseURL}/#mode=pilot&sel=${encodeURIComponent(delayedContactId)}`,
    );

    await expect(page.locator(".stream-pill").nth(1)).toContainText("synced", {
      timeout: 15_000,
    });

    const contactBanner = page.getByTestId("external-contact-banner");
    await expect(contactBanner).toBeVisible();
    await expect(contactBanner).toContainText("AIS Delayed Tanker-9");
    await expect(contactBanner).toContainText("type: cargo-vessel");
    await expect(contactBanner).toContainText("feed: degraded");
    await expect(contactBanner).toContainText("source: ais-coastal-relay");
    await expect(contactBanner).toContainText(`observed: ${observedIso}`);
    await expect(contactBanner).toContainText(`received: ${receivedIso}`);
    await expect(contactBanner).toContainText(`expires: ${expiresIso}`);
    await expect(contactBanner).toContainText("uncertainty: ±18.5 m");

    const pilotIsolation = page.getByTestId("pilot-contact-isolation");
    await expect(pilotIsolation).toBeVisible();
    await expect(pilotIsolation).toContainText(`observed: ${observedIso}`);
    await expect(pilotIsolation).toContainText(`received: ${receivedIso}`);
    await expect(pilotIsolation).toContainText(`expires: ${expiresIso}`);
    await expect(pilotIsolation).toContainText("uncertainty: ±18.5 m");

    // Non-UXS external contact MUST NEVER expose vehicle P1-P4 authority, arming, link-tier, HUD attitude readouts, or guarded actions.
    await expect(page.getByTestId("vehicle-authority-banner")).toHaveCount(0);
    await expect(page.getByTestId("authority-badge")).toHaveCount(0);
    await expect(page.getByTestId("armed-badge")).toHaveCount(0);
    await expect(page.getByTestId("hud-link-tier")).toHaveCount(0);
    await expect(page.getByTestId("hud-readout-strip")).toHaveCount(0);
    await expect(page.getByTestId("pilot-guarded-actions")).toHaveCount(0);

    // Bench also surfaces distinct observed/received freshness and withholds parameter tuning controls on external COP contacts.
    await page.keyboard.press("b");
    const benchNotice = page.getByTestId("bench-contact-notice");
    await expect(benchNotice).toContainText("AIS Delayed Tanker-9");
    await expect(benchNotice).toContainText(`observed: ${observedIso}`);
    await expect(benchNotice).toContainText(`received: ${receivedIso}`);
    await expect(benchNotice).toContainText(`expires: ${expiresIso}`);
    await expect(benchNotice).toContainText("uncertainty: ±18.5 m");
    await expect(page.getByTestId("bench-tuning-withheld")).toContainText(
      "External COP contacts expose no parameter tuning rack.",
    );
    await expect(page.getByTestId("bench-staged-count")).toHaveCount(0);

    // Switch to the partial-metadata contact via FleetFilmstrip and verify unknown-preserving fields across Bench and Pilot.
    const partialCard = page
      .getByTestId("fleet-filmstrip")
      .locator(`button[data-entity-id="${partialContactId}"]`);
    await partialCard.click();

    await expect(contactBanner).toContainText("Radar Track Partial-4");
    await expect(contactBanner).toContainText(
      `observed: ${partialObservedIso}`,
    );
    await expect(contactBanner).toContainText("received: unknown");
    await expect(contactBanner).toContainText("expires: unknown");
    await expect(contactBanner).toContainText("uncertainty: unknown");

    await expect(benchNotice).toContainText("Radar Track Partial-4");
    await expect(benchNotice).toContainText(`observed: ${partialObservedIso}`);
    await expect(benchNotice).toContainText("received: unknown");
    await expect(benchNotice).toContainText("expires: unknown");
    await expect(benchNotice).toContainText("uncertainty: unknown");

    await page.keyboard.press("d");
    await expect(pilotIsolation).toContainText(
      `observed: ${partialObservedIso}`,
    );
    await expect(pilotIsolation).toContainText("received: unknown");
    await expect(pilotIsolation).toContainText("expires: unknown");
    await expect(pilotIsolation).toContainText("uncertainty: unknown");

    expect(copStack.pageErrors).toEqual([]);
    expect(copStack.consoleErrors).toEqual([]);
  });

  // spec: cop-ui / Fleet health readiness, per-vehicle observation freshness, and last-known fault retention
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  test("withholds FLEET NOMINAL across pre-sync startup, empty ENTITY_STATES sync, and dropped WebSockets before converging on live recovery (#39)", async ({
    copStack,
  }) => {
    const { page, info } = copStack;
    const heldTelemetrySockets: WebSocketRoute[] = [];
    const heldGraphSockets: WebSocketRoute[] = [];
    let allowUpstream = false;
    let dropImmediately = false;

    await page.routeWebSocket(/\/ws\/telemetry$/, (ws) => {
      if (allowUpstream) {
        ws.connectToServer();
        return;
      }
      if (dropImmediately) {
        void ws.close({ code: 1006, reason: "simulated telemetry outage" });
        return;
      }
      heldTelemetrySockets.push(ws);
    });

    await page.routeWebSocket(/\/ws\/graph$/, (ws) => {
      if (allowUpstream) {
        ws.connectToServer();
        return;
      }
      if (dropImmediately) {
        void ws.close({ code: 1006, reason: "simulated graph outage" });
        return;
      }
      heldGraphSockets.push(ws);
    });

    await page.goto(info.baseURL);

    // 1. Pre-sync startup: Fleet data socket is open/awaiting initial ENTITY_STATES sync -> never claims FLEET NOMINAL.
    await expect
      .poll(() => heldGraphSockets.length, { timeout: 10_000 })
      .toBeGreaterThanOrEqual(1);
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      "Fleet data: Syncing…",
    );
    await expect(page.getByTestId("fleet-health-badge")).toHaveText(
      "AWAITING FLEET DATA…",
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toHaveCount(0);

    // 2. Empty ENTITY_STATES sync (0 ownship vehicles) -> explicit "No vehicles detected", never FLEET NOMINAL.
    heldGraphSockets.at(-1)!.send(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 0,
      }),
    );
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      "Fleet data: Live · synced",
    );
    await expect(page.getByTestId("fleet-health-badge")).toHaveText(
      "No vehicles detected",
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toHaveCount(0);

    // Empty fleet: every Fleet tab resolves aria-controls to its mounted tabpanel, Tab enters the read-only panel (never BODY), and Shift+Tab returns to the active tab.
    await page.getByTestId("cop-solo-tab-summary").focus();
    for (const [tabTestId, panelId, stepAfter] of [
      ["cop-solo-tab-summary", "cop-solo-panel-summary", "ArrowRight"],
      ["cop-solo-tab-alerts", "cop-solo-panel-alerts", "ArrowRight"],
      ["cop-solo-tab-skg", "cop-solo-panel-skg", "Home"],
    ] as const) {
      const tabLoc = page.getByTestId(tabTestId);
      await expect(tabLoc).toBeFocused();
      const panelCheck = await page.evaluate(() => {
        const ids = [
          "cop-solo-tab-summary",
          "cop-solo-tab-alerts",
          "cop-solo-tab-skg",
        ];
        const panels = ids.map((id) => {
          const tab = document.getElementById(id);
          const ctrl = tab?.getAttribute("aria-controls") ?? "";
          const panel = document.getElementById(ctrl);
          return {
            tabId: id,
            panelId: panel?.id ?? null,
            labelledBy: panel?.getAttribute("aria-labelledby") ?? null,
            exposed: Boolean(
              panel &&
              !panel.hidden &&
              window.getComputedStyle(panel).display !== "none",
            ),
          };
        });
        return {
          allResolved: panels.every(
            (p) => p.panelId !== null && p.labelledBy === p.tabId,
          ),
          exposedIds: panels.filter((p) => p.exposed).map((p) => p.panelId),
        };
      });
      expect(panelCheck).toEqual({
        allResolved: true,
        exposedIds: [panelId],
      });
      await page.keyboard.press("Tab");
      await expect(page.locator(`#${panelId}`)).toBeFocused();
      await page.keyboard.press("Shift+Tab");
      await expect(tabLoc).toBeFocused();
      await page.keyboard.press(stepAfter);
    }

    // 3. Simulated upstream drop across both WebSockets -> RECONNECTING FLEET DATA…, never FLEET NOMINAL.
    dropImmediately = true;
    for (const sock of [...heldTelemetrySockets, ...heldGraphSockets]) {
      await sock.close({ code: 1006, reason: "simulated upstream drop" });
    }
    await expect(page.getByTestId("stream-pill-telemetry")).toContainText(
      /Telemetry: (Reconnecting…|Connecting…)/,
    );
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      /Fleet data: (Reconnecting · last known|Reconnecting…|Syncing…)/,
    );
    await expect(page.getByTestId("fleet-health-badge")).toHaveText(
      /^(RECONNECTING FLEET DATA…|AWAITING FLEET DATA…)$/,
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toHaveCount(0);

    // 4. Restore upstream WebSocket forwarding -> automatic backoff recovery converges to FLEET NOMINAL (3).
    dropImmediately = false;
    allowUpstream = true;
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      "Fleet data: Live · synced",
      { timeout: 15_000 },
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toHaveText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );

    expect(copStack.pageErrors).toEqual([]);
  });

  // spec: cop-ui / Conventional keyboard traversal, WAI-ARIA tablist navigation, focus management, and character-key shortcut gating
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  test("preserves Tab/Shift+Tab order across canvases, enforces single-stop WAI-ARIA inspector tabs and focus recovery, gates character shortcuts across reloads, and keeps :focus-visible unobstructed (#41)", async ({
    copStack,
  }) => {
    const { page, info } = copStack;
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(info.baseURL);

    await expect(page.getByTestId("fleet-nominal-badge")).toHaveText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );

    const workspaceNav = page.getByRole("navigation", {
      name: "Task workspaces",
    });
    const fleetNavBtn = page.getByTestId("workspace-tab-cop");
    const vehicleNavBtn = page.getByTestId("workspace-tab-pilot");
    const diagnosticsNavBtn = page.getByTestId("workspace-tab-bench");
    const analysisNavBtn = page.getByTestId("workspace-tab-cortex");

    await expect(fleetNavBtn).toHaveAttribute("aria-current", "page");
    await expect(vehicleNavBtn).not.toHaveAttribute("aria-current", "page");
    await expect(diagnosticsNavBtn).not.toHaveAttribute("aria-current", "page");
    await expect(analysisNavBtn).not.toHaveAttribute("aria-current", "page");

    // 1. Page-entry Tab enters topbar with visible, unobstructed :focus-visible outline.
    await page.keyboard.press("Tab");
    const firstTabCheck = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el || el === document.body) {
        return null;
      }
      const style = window.getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      const hit = document.elementFromPoint(
        rect.left + rect.width * 0.5,
        rect.top + rect.height * 0.5,
      );
      return {
        inTopbar: Boolean(
          el.closest('[data-testid="invariant-safety-topbar"]'),
        ),
        focusVisible: el.matches(":focus-visible"),
        outlineStyle: style.outlineStyle,
        outlineWidth: style.outlineWidth,
        unobstructed: Boolean(hit && (hit === el || el.contains(hit))),
      };
    });
    expect(firstTabCheck).toEqual({
      inTopbar: true,
      focusVisible: true,
      outlineStyle: "solid",
      outlineWidth: "2px",
      unobstructed: true,
    });

    // 2. Clicking TacticalCanvas then pressing Tab / Shift+Tab moves focus in DOM order without toggling Lights-Out; L toggles Lights-Out.
    const hudCanvas = page.locator(".safety-hud-canvas");
    await hudCanvas.click({ position: { x: 24, y: 120 } });
    await page.keyboard.press("Tab");
    await expect(page.getByTestId("cop-roster")).toBeVisible();
    const postCanvasTab = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el) return null;
      const rect = el.getBoundingClientRect();
      const hit = document.elementFromPoint(
        rect.left + rect.width * 0.5,
        rect.top + rect.height * 0.5,
      );
      return {
        text: el.textContent?.trim(),
        inToolbar: Boolean(el.closest('[data-testid="viewport-toolbar"]')),
        focusVisible: el.matches(":focus-visible"),
        unobstructed: Boolean(hit && (hit === el || el.contains(hit))),
      };
    });
    expect(postCanvasTab).toEqual({
      text: "quadcopter",
      inToolbar: true,
      focusVisible: true,
      unobstructed: true,
    });

    await hudCanvas.click({ position: { x: 24, y: 120 } });
    await page.keyboard.press("Shift+Tab");
    await expect(page.getByTestId("cop-roster")).toBeVisible();
    const postCanvasShiftTabInRoster = await page.evaluate(() =>
      Boolean(document.activeElement?.closest('[data-testid="cop-roster"]')),
    );
    expect(postCanvasShiftTabInRoster).toBe(true);

    // L toggles Lights-Out on and off; topbar Lights-Out button exposes L shortcut.
    const lightsOutBtn = page.getByTestId("lights-out-toggle");
    await expect(lightsOutBtn).toHaveAttribute("aria-keyshortcuts", "L");
    await page.keyboard.press("l");
    await expect(lightsOutBtn).toHaveAttribute("aria-pressed", "true");
    await expect(page.getByTestId("cop-roster")).toHaveCount(0);
    await page.keyboard.press("l");
    await expect(lightsOutBtn).toHaveAttribute("aria-pressed", "false");
    await expect(page.getByTestId("cop-roster")).toBeVisible();

    // 3. Fleet inspector tablist: all 3 aria-controls targets stay mounted, read-only panels expose tabindex="0", and focus recovers on unmount.
    const summaryTab = page.getByTestId("cop-solo-tab-summary");
    const alertsTab = page.getByTestId("cop-solo-tab-alerts");
    const skgTab = page.getByTestId("cop-solo-tab-skg");

    const assertMountedPanels = async (expectedExposedId: string) => {
      const state = await page.evaluate(() => {
        const ids = [
          "cop-solo-tab-summary",
          "cop-solo-tab-alerts",
          "cop-solo-tab-skg",
        ];
        const entries = ids.map((id) => {
          const tab = document.getElementById(id);
          const ctrl = tab?.getAttribute("aria-controls") ?? "";
          const panel = document.getElementById(ctrl);
          return {
            tabId: id,
            panelId: panel?.id ?? null,
            role: panel?.getAttribute("role") ?? null,
            labelledBy: panel?.getAttribute("aria-labelledby") ?? null,
            exposed: Boolean(
              panel &&
              !panel.hidden &&
              window.getComputedStyle(panel).display !== "none",
            ),
          };
        });
        return {
          allResolved: entries.every(
            (e) =>
              e.panelId !== null &&
              e.role === "tabpanel" &&
              e.labelledBy === e.tabId,
          ),
          exposedIds: entries.filter((e) => e.exposed).map((e) => e.panelId),
        };
      });
      expect(state).toEqual({
        allResolved: true,
        exposedIds: [expectedExposedId],
      });
    };

    // Healthy fleet with no entity selected & 0 exceptions: Tab enters each read-only tabpanel (never BODY) and Shift+Tab returns to active tab.
    await summaryTab.focus();
    for (const [tabLoc, panelId, nextKey] of [
      [summaryTab, "cop-solo-panel-summary", "ArrowRight"],
      [alertsTab, "cop-solo-panel-alerts", "ArrowRight"],
      [skgTab, "cop-solo-panel-skg", "Home"],
    ] as const) {
      await expect(tabLoc).toBeFocused();
      await assertMountedPanels(panelId);
      await page.keyboard.press("Tab");
      await expect(page.locator(`#${panelId}`)).toBeFocused();
      await page.keyboard.press("Shift+Tab");
      await expect(tabLoc).toBeFocused();
      await page.keyboard.press(nextKey);
    }

    await page.keyboard.press("1");
    await summaryTab.focus();
    await expect(summaryTab).toHaveAttribute("tabindex", "0");
    await expect(alertsTab).toHaveAttribute("tabindex", "-1");
    await expect(skgTab).toHaveAttribute("tabindex", "-1");
    await assertMountedPanels("cop-solo-panel-summary");

    // Single Tab stop: when Summary contains focusable buttons, pressing Tab skips inactive tabs and lands directly on the first jump button.
    await page.keyboard.press("Tab");
    await expect(
      page
        .getByTestId("cop-solo-panel-summary")
        .getByRole("button", { name: "Vehicle [D]" }),
    ).toBeFocused();

    // Unmounting the focused panel button via reactive hash deselection restores focus to the active Summary tab (not document.body).
    await page.evaluate(() => {
      window.location.hash = "";
    });
    await expect(
      page
        .getByTestId("cop-solo-panel-summary")
        .getByRole("button", { name: "Vehicle [D]" }),
    ).toHaveCount(0);
    await expect(summaryTab).toBeFocused();

    // With UXS #1 selected in a healthy fleet (0 exceptions), Exceptions panel is still read-only -> Tab enters cop-solo-panel-alerts and Shift+Tab returns to alertsTab.
    await page.keyboard.press("1");
    await summaryTab.focus();
    await page.keyboard.press("ArrowRight");
    await expect(alertsTab).toBeFocused();
    await expect(alertsTab).toHaveAttribute("aria-selected", "true");
    await assertMountedPanels("cop-solo-panel-alerts");
    await page.keyboard.press("Tab");
    await expect(page.getByTestId("cop-solo-panel-alerts")).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(alertsTab).toBeFocused();

    await page.keyboard.press("End");
    await expect(skgTab).toBeFocused();
    await expect(skgTab).toHaveAttribute("aria-selected", "true");
    await assertMountedPanels("cop-solo-panel-skg");

    await page.keyboard.press("ArrowLeft");
    await expect(alertsTab).toBeFocused();
    await assertMountedPanels("cop-solo-panel-alerts");

    await page.keyboard.press("Home");
    await expect(summaryTab).toBeFocused();
    await expect(summaryTab).toHaveAttribute("aria-selected", "true");
    await assertMountedPanels("cop-solo-panel-summary");

    // 4. Cross-workspace jump button focuses destination stage (not document.body), and Bench/Cortex canvases preserve Tab order.
    await page.keyboard.press("Tab");
    await page.keyboard.press("Tab");
    const jumpBenchBtn = page
      .getByTestId("cop-solo-panel-summary")
      .getByRole("button", { name: "Diagnostics [B]" });
    await expect(jumpBenchBtn).toBeFocused();
    await page.keyboard.press("Enter");

    await expect(diagnosticsNavBtn).toHaveAttribute("aria-current", "page");
    await expect(page.getByTestId("bench-workspace")).toBeFocused();

    await page.getByTestId("bench-oscilloscope").click({
      position: { x: 40, y: 60 },
    });
    await page.keyboard.press("Tab");
    const postScopeFocusTag = await page.evaluate(
      () => document.activeElement?.tagName ?? null,
    );
    expect(postScopeFocusTag).toBe("INPUT");

    // Switch to Analysis via hotkey K -> stage focused; typing shortcut letters inside search input never triggers hotkeys.
    await page.keyboard.press("Escape");
    await page.getByTestId("bench-oscilloscope").click({
      position: { x: 40, y: 60 },
    });
    await page.keyboard.press("k");
    await expect(analysisNavBtn).toHaveAttribute("aria-current", "page");
    await expect(page.getByTestId("cortex-workspace")).toBeFocused();

    await page.getByTestId("cortex-sigma-host").click({
      position: { x: 20, y: 20 },
    });
    await page.keyboard.press("Tab");
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "cortex",
    );
    await expect(lightsOutBtn).toHaveAttribute("aria-pressed", "false");

    const queryInput = page.getByTestId("cortex-query-input");
    await queryInput.click();
    await queryInput.pressSequentially("gdbkml1tch");
    await expect(queryInput).toHaveValue("gdbkml1tch");
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "cortex",
    );
    await expect(lightsOutBtn).toHaveAttribute("aria-pressed", "false");

    // 5. Direct topbar click preserves button focus; declutter group describes H cycle only while shortcuts are enabled; Shortcuts: Off persists across reload.
    await fleetNavBtn.click();
    await expect(fleetNavBtn).toBeFocused();
    await expect(fleetNavBtn).toHaveAttribute("aria-current", "page");

    await page.keyboard.press("1");
    await page.keyboard.press("t");
    const tetherRegularBtn = page
      .getByTestId("viewport-toolbar")
      .getByRole("button", { name: /^Tether\b/ });
    await expect(tetherRegularBtn).toHaveAttribute("aria-pressed", "true");

    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Accessibility.enable");
    const readDeclutterAx = async () => {
      const { nodes } = await cdp.send("Accessibility.getFullAXTree");
      const groupNode = nodes.find(
        (n) => n.role?.value === "group" && n.name?.value === "HUD declutter",
      );
      const levelButtons = ["none", "minimal", "tactical"].map((lvl) => {
        const btn = nodes.find(
          (n) => n.role?.value === "button" && n.name?.value === lvl,
        );
        const keyshortcuts =
          btn?.properties?.find((p) => p.name === "keyshortcuts")?.value
            ?.value ?? null;
        return {
          name: lvl,
          description: btn?.description?.value ?? "",
          keyshortcuts,
        };
      });
      return {
        groupDescription: groupNode?.description?.value ?? "",
        levelButtons,
      };
    };

    expect(await readDeclutterAx()).toEqual({
      groupDescription: "Press H to cycle HUD declutter levels",
      levelButtons: [
        {
          name: "none",
          description: "HUD declutter: none",
          keyshortcuts: null,
        },
        {
          name: "minimal",
          description: "HUD declutter: minimal",
          keyshortcuts: null,
        },
        {
          name: "tactical",
          description: "HUD declutter: tactical",
          keyshortcuts: null,
        },
      ],
    });

    const shortcutsBtn = page.getByTestId("character-shortcuts-toggle");
    await expect(shortcutsBtn).toHaveText("Shortcuts: On");
    await shortcutsBtn.click();
    await expect(shortcutsBtn).toHaveText("Shortcuts: Off");
    await expect(shortcutsBtn).toHaveAttribute("aria-pressed", "false");
    await expect(page.locator("kbd")).toHaveCount(0);
    await expect(
      page
        .getByTestId("cop-solo-panel-summary")
        .getByRole("button", { name: "Vehicle", exact: true }),
    ).toBeVisible();
    await expect(
      page
        .getByTestId("viewport-toolbar")
        .getByRole("button", { name: "Tether", exact: true }),
    ).toBeVisible();

    expect(await readDeclutterAx()).toEqual({
      groupDescription: "",
      levelButtons: [
        {
          name: "none",
          description: "HUD declutter: none",
          keyshortcuts: null,
        },
        {
          name: "minimal",
          description: "HUD declutter: minimal",
          keyshortcuts: null,
        },
        {
          name: "tactical",
          description: "HUD declutter: tactical",
          keyshortcuts: null,
        },
      ],
    });
    await cdp.detach();

    // Character keys are ignored when off; Escape still clears camera tether.
    for (const key of ["d", "b", "k", "l", "2", "c", "h"]) {
      await page.keyboard.press(key);
    }
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "cop",
    );
    await expect(lightsOutBtn).toHaveAttribute("aria-pressed", "false");
    await expect(tetherRegularBtn).toHaveAttribute("aria-pressed", "true");

    await page.keyboard.press("Escape");
    await expect(tetherRegularBtn).toHaveAttribute("aria-pressed", "false");

    // Verify browser accessibility tree snapshot exposes clean role/name semantics with inactive shortcut hints omitted.
    const railAriaSnapshot = await page
      .getByTestId("cop-solo-rail")
      .ariaSnapshot();
    expect(railAriaSnapshot).toContain("tablist");
    expect(railAriaSnapshot).toContain('tab "Summary" [selected]');
    expect(railAriaSnapshot).toContain('tabpanel "Summary"');

    const navAriaSnapshot = await workspaceNav.ariaSnapshot();
    expect(navAriaSnapshot).toContain('button "Fleet"');
    expect(navAriaSnapshot).toContain('button "Lights-Out"');
    expect(navAriaSnapshot).toContain('button "Shortcuts: Off"');
    expect(navAriaSnapshot).not.toContain("[G]");
    expect(navAriaSnapshot).not.toContain("[L]");

    // Reloading the page preserves Shortcuts: Off from localStorage; re-enabling restores Shortcuts: On.
    await page.reload();
    await expect(page.getByTestId("character-shortcuts-toggle")).toHaveText(
      "Shortcuts: Off",
    );
    await expect(page.locator("kbd")).toHaveCount(0);

    await page.getByTestId("character-shortcuts-toggle").click();
    await expect(page.getByTestId("character-shortcuts-toggle")).toHaveText(
      "Shortcuts: On",
    );
    await expect(
      workspaceNav.getByRole("button", { name: /^Fleet\b/ }).locator("kbd"),
    ).toHaveText("G");

    expect(copStack.pageErrors).toEqual([]);
  });

  // spec: cop-ui / Session-scoped Diagnostics parameter draft and Analysis investigation continuity across workspace switches
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  test("preserves per-vehicle Diagnostics drafts (Heading P Gain 3.20 -> 3.30, baseline, single-vehicle Revert Draft) and Analysis query/tier/results/provenance across Fleet and Vehicle round trips (#40)", async ({
    copStack,
  }) => {
    const { page, info } = copStack;

    await page.goto(info.baseURL);
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      "synced",
      { timeout: 15_000 },
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toContainText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );

    const setRangeValue = async (paramKey: string, nextValue: string) => {
      const rangeInput = page.locator(
        `[data-param-key="${paramKey}"] input[type="range"]`,
      );
      await expect(rangeInput).toBeVisible();
      await rangeInput.evaluate((el, val) => {
        const input = el as HTMLInputElement;
        input.value = val;
        input.dispatchEvent(new Event("input", { bubbles: true }));
      }, nextValue);
    };

    const selectFilmstripVehicle = async (entityId: string) => {
      await page
        .locator(`[data-testid="filmstrip-item"][data-entity-id="${entityId}"]`)
        .click();
    };

    // 1. Select UXS #1 (quadcopter), open Diagnostics, and stage Heading P Gain 3.20 -> 3.30 (1 staged).
    await selectFilmstripVehicle(info.vehicles.quadcopter);
    await page.getByTestId("workspace-tab-bench").click();
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "bench",
    );
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("3.20 1/s");

    await setRangeValue("uxs.control.heading_kp", "3.3");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("3.30 1/s");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("(base 3.20)");

    // 2. Select UXS #2 (rover) in Diagnostics and stage Max Groundspeed 6.0 -> 9.5 (1 staged) without affecting UXS #1.
    await selectFilmstripVehicle(info.vehicles.rover);
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("2.10 1/s");

    await setRangeValue("uxs.limits.max_groundspeed_mps", "9.5");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(
      page.locator('[data-param-key="uxs.limits.max_groundspeed_mps"]'),
    ).toContainText("9.5 m/s");
    await expect(
      page.locator('[data-param-key="uxs.limits.max_groundspeed_mps"]'),
    ).toContainText("(base 6.0)");

    // 3. Select UXS #1, open Analysis, switch tier to SEMANTIC, and submit predicate query "uxs.domain.kind".
    await selectFilmstripVehicle(info.vehicles.quadcopter);
    await page.getByTestId("workspace-tab-cortex").click();
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "cortex",
    );

    await page.getByTestId("cortex-tier-semantic").click();
    await expect(page.getByTestId("cortex-tier-semantic")).toHaveAttribute(
      "aria-pressed",
      "true",
    );

    const queryInput = page.getByTestId("cortex-query-input");
    await queryInput.fill("uxs.domain.kind");
    await page.getByTestId("cortex-query-submit").click();

    await expect(page.getByTestId("cortex-session-status")).toHaveAttribute(
      "data-status",
      "completed",
      { timeout: 10_000 },
    );
    await expect(page.getByTestId("cortex-hit-item")).toHaveCount(3);
    await expect(page.getByTestId("cortex-causal-subgraph")).toContainText(
      "5 nodes · 2 edges",
      { timeout: 10_000 },
    );
    await expect(page.getByTestId("cortex-sigma-host")).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.quadcopter,
    );
    await expect(page.getByTestId("cortex-provenance-column")).toContainText(
      "quadcopter",
    );

    // 4. Switch through Fleet -> select UXS #2 -> Vehicle -> Diagnostics and verify both vehicle drafts survived in strict isolation.
    await page.getByTestId("workspace-tab-cop").click();
    await selectFilmstripVehicle(info.vehicles.rover);
    await page.getByTestId("workspace-tab-pilot").click();
    await page.getByTestId("workspace-tab-bench").click();

    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(
      page.locator('[data-param-key="uxs.limits.max_groundspeed_mps"]'),
    ).toContainText("9.5 m/s");
    await expect(
      page.locator('[data-param-key="uxs.limits.max_groundspeed_mps"]'),
    ).toContainText("(base 6.0)");

    await selectFilmstripVehicle(info.vehicles.quadcopter);
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("3.30 1/s");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("(base 3.20)");

    // 5. Revert UXS #1 ONLY, round-trip through Fleet (selecting UXS #2), and verify UXS #1 stays 0 staged while UXS #2 stays 1 staged.
    await page.getByTestId("bench-tuning-revert").click();
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("3.20 1/s");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).not.toContainText("(base 3.20)");

    await page.getByTestId("workspace-tab-cop").click();
    await selectFilmstripVehicle(info.vehicles.rover);
    await page.getByTestId("workspace-tab-bench").click();

    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(
      page.locator('[data-param-key="uxs.limits.max_groundspeed_mps"]'),
    ).toContainText("9.5 m/s");

    await selectFilmstripVehicle(info.vehicles.quadcopter);
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");

    // 6. Select UXS #2 in Fleet and return to Analysis: query text, SEMANTIC tier, hits, 2-hop subgraph, pinned UXS #1 citations, and earlier-selection provenance all survive!
    await page.getByTestId("workspace-tab-cop").click();
    await selectFilmstripVehicle(info.vehicles.rover);
    await page.getByTestId("workspace-tab-cortex").click();

    await expect(page.getByTestId("cortex-query-input")).toHaveValue(
      "uxs.domain.kind",
    );
    await expect(page.getByTestId("cortex-tier-semantic")).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await expect(page.getByTestId("cortex-hit-item")).toHaveCount(3);
    await expect(page.getByTestId("cortex-causal-subgraph")).toContainText(
      "5 nodes · 2 edges",
    );
    await expect(page.getByTestId("cortex-sigma-host")).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.quadcopter,
    );
    await expect(page.getByTestId("cortex-provenance-column")).toContainText(
      "quadcopter",
    );

    const provBanner = page.getByTestId("cortex-result-provenance");
    await expect(provBanner).toHaveAttribute("data-earlier-selection", "true");
    await expect(provBanner).toContainText("retrieval scope: Fleet-wide");
    await expect(provBanner).toContainText(
      "vehicle selected at submission: UXS #1",
    );
    await expect(provBanner).toContainText("earlier selection");
    await expect(provBanner).toContainText("UXS #2");

    await expect(
      page.getByTestId("cortex-inspection-divergence"),
    ).toBeVisible();
    await page.getByTestId("cortex-sync-selection-btn").click();
    await expect(page.getByTestId("cortex-sigma-host")).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.rover,
    );
    await expect(page.getByTestId("cortex-provenance-column")).toContainText(
      "rover",
    );

    expect(copStack.pageErrors).toEqual([]);
    expect(copStack.consoleErrors).toEqual([]);
  });

  // spec: cop-ui / Diagnostics sample baseline provenance, exact numeric staging, and non-actuating offline readiness
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  test("displays first-view sample-default provenance and plain-language local staging, enforces exact numeric bounds/step validation without hotkey capture, summarizes target-vehicle diffs, and emits zero outbound command traffic (#43)", async ({
    copStack,
  }) => {
    const { page, info } = copStack;

    const outboundHttpMutations: Array<{ method: string; url: string }> = [];
    const outboundWsPayloads: Array<string | Buffer> = [];

    const isPermittedKeepaliveWsPayload = (
      payload: string | Buffer,
    ): boolean => {
      if (typeof payload !== "string") {
        return false;
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(payload);
      } catch {
        return false;
      }
      if (
        typeof parsed !== "object" ||
        parsed === null ||
        Array.isArray(parsed)
      ) {
        return false;
      }
      const record = parsed as Record<string, unknown>;
      const keys = Object.keys(record);
      return (
        keys.length === 2 &&
        Object.prototype.hasOwnProperty.call(record, "type") &&
        Object.prototype.hasOwnProperty.call(record, "timestamp") &&
        record.type === "keepalive" &&
        typeof record.timestamp === "number" &&
        Number.isFinite(record.timestamp)
      );
    };

    const assertOnlyKeepaliveWsFrames = (
      payloads: ReadonlyArray<string | Buffer>,
    ): void => {
      const unexpected = payloads.filter(
        (payload) => !isPermittedKeepaliveWsPayload(payload),
      );
      expect(unexpected).toEqual([]);
    };

    // Self-verify that unexpected text and binary WebSocket frames fail the outbound assertion.
    expect(() =>
      assertOnlyKeepaliveWsFrames([
        JSON.stringify({ type: "keepalive", timestamp: 1_700_000_000_000 }),
      ]),
    ).not.toThrow();
    expect(() =>
      assertOnlyKeepaliveWsFrames(['{"op":"set","value":325}']),
    ).toThrow();
    expect(() =>
      assertOnlyKeepaliveWsFrames([
        '{"type":"keepalive","timestamp":1,"op":"set"}',
      ]),
    ).toThrow();
    expect(() => assertOnlyKeepaliveWsFrames(["ping"])).toThrow();
    expect(() =>
      assertOnlyKeepaliveWsFrames([Buffer.from([0xfd, 0x09, 0x00])]),
    ).toThrow();

    page.on("request", (req) => {
      const method = req.method().toUpperCase();
      if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS") {
        outboundHttpMutations.push({ method, url: req.url() });
      }
    });
    page.on("websocket", (ws) => {
      ws.on("framesent", (frame) => {
        outboundWsPayloads.push(frame.payload);
      });
    });

    await page.goto(info.baseURL);
    await expect(page.getByTestId("stream-pill-fleet")).toContainText(
      "synced",
      { timeout: 15_000 },
    );
    await expect(page.getByTestId("fleet-nominal-badge")).toContainText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );

    const selectFilmstripEntity = async (entityId: string) => {
      await page
        .locator(`[data-testid="filmstrip-item"][data-entity-id="${entityId}"]`)
        .click();
    };

    // 1. Open Diagnostics on UXS #1 (quadcopter) BEFORE editing (0 staged) and verify first-view sample provenance, plain-language local-draft copy, baseline/bounds, and nearby unavailable-apply text.
    await selectFilmstripEntity(info.vehicles.quadcopter);
    await page.getByTestId("workspace-tab-bench").click();
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "bench",
    );
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");

    const sourceBadge = page.getByTestId("bench-baseline-source-badge");
    await expect(sourceBadge).toHaveAttribute(
      "data-baseline-source",
      "domain-default",
    );
    await expect(sourceBadge).toHaveText("SAMPLE DOMAIN DEFAULTS");
    await expect(page.getByTestId("bench-provenance-headline")).toContainText(
      "Values shown for UXS #1 are sample quadcopter domain defaults, not parameters read from the vehicle.",
    );
    await expect(page.getByTestId("bench-local-draft-notice")).toContainText(
      "Edits create a local draft in this browser only and cannot change the vehicle in this version.",
    );
    await expect(page.getByTestId("bench-tuning-rack")).not.toContainText(
      "ADR-111",
    );
    await expect(page.getByTestId("bench-tuning-rack")).not.toContainText(
      "M3.1",
    );
    await expect(
      page
        .locator('[data-param-key="uxs.control.heading_kp"]')
        .getByTestId("bench-param-meta"),
    ).toContainText("Baseline 3.20 1/s · Bounds 0.10–8.00 1/s · step 0.05");
    await expect(page.getByTestId("bench-change-summary-empty")).toContainText(
      "No local parameter changes staged for UXS #1.",
    );
    await expect(page.getByTestId("bench-tuning-apply")).toBeDisabled();
    await expect(
      page.getByTestId("bench-apply-unavailable-note"),
    ).toContainText(
      "Applying parameters to the vehicle is unavailable in this version. No values have been sent, applied, or confirmed on the vehicle.",
    );

    // 2. Exact numeric entry alongside sliders (with Shortcuts: On): typing digits ("1", "2", "3") never triggers FleetFilmstrip vehicle hotkeys!
    const kpCard = page.locator('[data-param-key="uxs.control.heading_kp"]');
    const kpNumInput = kpCard.locator('input[type="number"]');
    const kpRangeInput = kpCard.locator('input[type="range"]');

    await kpNumInput.click();
    await kpNumInput.fill("3.45");
    await expect(kpRangeInput).toHaveValue("3.45");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(page.getByTestId("invariant-safety-topbar")).toContainText(
      "UXS #1 · quadcopter",
    );

    // Out-of-bounds ("9.25") and off-step ("3.42") numeric entries show inline validation error without mutating staged 3.45.
    await kpNumInput.fill("9.25");
    await expect(kpNumInput).toHaveAttribute("aria-invalid", "true");
    await expect(kpCard.getByTestId("bench-param-error")).toContainText(
      "Maximum is 8.00 1/s",
    );
    await expect(kpRangeInput).toHaveValue("3.45");

    await kpNumInput.fill("3.42");
    await expect(kpNumInput).toHaveAttribute("aria-invalid", "true");
    await expect(kpCard.getByTestId("bench-param-error")).toContainText(
      "Must align to step 0.05 1/s",
    );
    await expect(kpRangeInput).toHaveValue("3.45");

    // Escape clears invalid numeric text back to staged 3.45 without workspace or tether side effects.
    await kpNumInput.press("Escape");
    await expect(kpNumInput).not.toHaveAttribute("aria-invalid", "true");
    await expect(kpNumInput).toHaveValue("3.45");
    await expect(kpCard.getByTestId("bench-param-error")).toHaveCount(0);

    // Near-step off-step "325.00001" on Teleop Deadman Timeout (starting at 250 ms, step 25) rejects without rounding, then exact "325" stages 325 ms.
    const deadmanRow = page.locator(
      '[data-param-key="uxs.mux.teleop_deadman_ms"]',
    );
    const deadmanNumInput = deadmanRow.locator('input[type="number"]');
    const deadmanRangeInput = deadmanRow.locator('input[type="range"]');
    await deadmanNumInput.fill("325.00001");
    await expect(deadmanNumInput).toHaveAttribute("aria-invalid", "true");
    await expect(deadmanRow.getByTestId("bench-param-error")).toContainText(
      "Must align to step 25 ms",
    );
    await expect(deadmanRangeInput).toHaveValue("250");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");

    await deadmanNumInput.fill("325");
    await expect(deadmanNumInput).not.toHaveAttribute("aria-invalid", "true");
    await expect(deadmanRangeInput).toHaveValue("325");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("2 staged");

    const summary = page.getByTestId("bench-change-summary");
    await expect(summary).toContainText("TARGET UXS #1 (quadcopter)");
    await expect(summary).toContainText("vs. sample quadcopter defaults");
    await expect(summary).toContainText("3.20 → 3.45 1/s (+0.25 1/s)");
    await expect(summary).toContainText("250 → 325 ms (+75 ms)");
    await expect(page.getByTestId("bench-tuning-apply")).toBeDisabled();

    // 3. Switch to UXS #2 (rover), stage exact value 7.5 m/s, switch to external contact (withheld), and return to UXS #1 -> provenance and isolated drafts preserved!
    await selectFilmstripEntity(info.vehicles.rover);
    await expect(sourceBadge).toHaveText("SAMPLE DOMAIN DEFAULTS");
    await expect(page.getByTestId("bench-provenance-headline")).toContainText(
      "sample rover domain defaults",
    );
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");

    await page
      .locator(
        '[data-param-key="uxs.limits.max_groundspeed_mps"] input[type="number"]',
      )
      .fill("7.5");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(summary).toContainText("TARGET UXS #2 (rover)");
    await expect(summary).toContainText("6.0 → 7.5 m/s (+1.5 m/s)");

    await selectFilmstripEntity(info.externalContactId);
    await expect(page.getByTestId("bench-tuning-withheld")).toContainText(
      "External COP contacts expose no parameter tuning rack.",
    );
    await expect(
      page.getByTestId("bench-tuning-rack").locator("input"),
    ).toHaveCount(0);

    await selectFilmstripEntity(info.vehicles.quadcopter);
    await expect(page.getByTestId("bench-staged-count")).toHaveText("2 staged");
    await expect(summary).toContainText("TARGET UXS #1 (quadcopter)");

    // 4. Revert UXS #1 -> clears UXS #1 to 0 staged while UXS #2 stays at 1 staged.
    await page.getByTestId("bench-tuning-revert").click();
    await expect(page.getByTestId("bench-staged-count")).toHaveText("0 staged");
    await expect(page.getByTestId("bench-change-summary-empty")).toBeVisible();

    await selectFilmstripEntity(info.vehicles.rover);
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");

    // 5. Assert zero outbound HTTP mutations and strictly keepalive-only outbound WebSocket frames!
    expect(outboundHttpMutations).toEqual([]);
    assertOnlyKeepaliveWsFrames(outboundWsPayloads);
    expect(copStack.pageErrors).toEqual([]);
    expect(copStack.consoleErrors).toEqual([]);
  });

  // spec: cop-ui / Compact desktop viewport layout, persistent safety header, and non-modal Fleet panel controls
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  test("keeps Fleet context and safety status usable without page scroll at 1024x768 and 1440x900, preserves focus and #40 state across non-modal panels, and avoids overflow or clipping across empty, multi-fault, growing-roster, long-name, and 200% zoom states (#42)", async ({
    copStack,
  }) => {
    const { page, info } = copStack;

    const heldEmptyGraphSockets: WebSocketRoute[] = [];
    let activeGraphSocket: WebSocketRoute | null = null;
    let allowUpstream = false;

    await page.routeWebSocket(/\/ws\/graph$/, (ws) => {
      if (!allowUpstream) {
        heldEmptyGraphSockets.push(ws);
        return;
      }
      activeGraphSocket = ws;
      const server = ws.connectToServer();
      server.onMessage((msg) => {
        ws.send(msg);
      });
    });

    const assertInViewport = async (
      loc: Locator,
      vpW: number,
      vpH: number,
    ): Promise<BoxRect> => {
      await expect(loc).toBeVisible();
      const box = await loc.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.width).toBeGreaterThan(0);
      expect(box!.height).toBeGreaterThan(0);
      expect(box!.x).toBeGreaterThanOrEqual(-1);
      expect(box!.y).toBeGreaterThanOrEqual(-1);
      expect(box!.x + box!.width).toBeLessThanOrEqual(vpW + 1);
      expect(box!.y + box!.height).toBeLessThanOrEqual(vpH + 1);
      return box!;
    };

    const assertZeroHorizontalPageOverflow = async (): Promise<void> => {
      const metrics = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
      }));
      expect(metrics.scrollWidth).toBeLessThanOrEqual(metrics.innerWidth);
    };

    // Phase A: Empty-data state at 1024x768 (100% zoom) — Roster collapsed by default, zero horizontal overflow, zero page scroll.
    await page.setViewportSize({ width: 1024, height: 768 });
    await page.goto(info.baseURL);
    await expect
      .poll(() => heldEmptyGraphSockets.length, { timeout: 10_000 })
      .toBeGreaterThanOrEqual(1);
    heldEmptyGraphSockets.at(-1)!.send(
      JSON.stringify({
        type: "synced",
        bucket: "ENTITY_STATES",
        revision: 0,
      }),
    );

    await expect(page.getByTestId("fleet-health-badge")).toHaveText(
      "No vehicles detected",
    );
    await expect(page.getByTestId("cop-workspace")).toHaveAttribute(
      "data-layout-tier",
      "compact",
    );
    await expect(page.getByTestId("cop-roster-toggle")).toHaveAttribute(
      "aria-expanded",
      "false",
    );
    await expect(page.getByTestId("cop-inspector-toggle")).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    await expect(page.locator("[aria-modal]")).toHaveCount(0);
    await assertZeroHorizontalPageOverflow();
    expect(await page.evaluate(() => window.scrollY)).toBe(0);

    // Phase B: Connect live loopback fleet and verify scroll-free vehicle inspection + reachable switcher at 1024x768 and 1440x900.
    allowUpstream = true;
    for (const sock of heldEmptyGraphSockets) {
      await sock.close({ code: 1006, reason: "connect live graph" });
    }
    await expect(page.getByTestId("fleet-nominal-badge")).toHaveText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );

    // At 1024x768: Roster body is hidden (deduplicating FleetFilmstrip), while TacticalCanvas + Inspector + FleetFilmstrip all fit in [0, 768].
    await expect(page.locator("#cop-roster-body")).toBeHidden();
    await page
      .locator(
        `[data-testid="filmstrip-item"][data-entity-id="${info.vehicles.quadcopter}"]`,
      )
      .click();

    await expect(page.getByTestId("cop-summary-identity")).toContainText(
      "UXS #1",
    );
    await expect(page.getByTestId("cop-summary-authority")).toContainText(
      /P[1-4]/,
    );
    await expect(page.getByTestId("cop-summary-propulsion")).toHaveText(
      /^(ARMED|DISARMED)$/,
    );
    await expect(page.getByTestId("cop-summary-freshness")).toContainText(
      /^(LIVE|LAST KNOWN)/,
    );

    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    await assertZeroHorizontalPageOverflow();

    const topbar1024 = await assertInViewport(
      page.getByTestId("invariant-safety-topbar"),
      1024,
      768,
    );
    const canvas1024 = await assertInViewport(
      page.locator(".safety-hud-canvas"),
      1024,
      768,
    );
    const inspector1024 = await assertInViewport(
      page.getByTestId("cop-solo-rail"),
      1024,
      768,
    );
    await assertInViewport(page.getByTestId("fleet-filmstrip"), 1024, 768);
    await assertInViewport(page.getByTestId("cop-summary-identity"), 1024, 768);
    await assertInViewport(
      page.getByTestId("cop-summary-authority"),
      1024,
      768,
    );
    await assertInViewport(
      page.getByTestId("cop-summary-propulsion"),
      1024,
      768,
    );
    await assertInViewport(
      page.getByTestId("cop-summary-freshness"),
      1024,
      768,
    );
    expect(canvas1024.width).toBeGreaterThanOrEqual(450);
    expect(canvas1024.height).toBeGreaterThanOrEqual(350);
    expect(boxesOverlap(canvas1024, inspector1024)).toBe(false);
    expect(topbar1024.y).toBeGreaterThanOrEqual(0);

    // Resize to 1440x900: both Roster and Inspector default open side-by-side with TacticalCanvas and zero page scroll.
    await page.setViewportSize({ width: 1440, height: 900 });
    await expect(page.getByTestId("cop-workspace")).toHaveAttribute(
      "data-layout-tier",
      "wide",
    );
    await expect(page.getByTestId("cop-roster-toggle")).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    await expect(page.locator("#cop-roster-body")).toBeVisible();
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    await assertZeroHorizontalPageOverflow();

    const roster1440 = await assertInViewport(
      page.getByTestId("cop-roster"),
      1440,
      900,
    );
    const canvas1440 = await assertInViewport(
      page.locator(".safety-hud-canvas"),
      1440,
      900,
    );
    const inspector1440 = await assertInViewport(
      page.getByTestId("cop-solo-rail"),
      1440,
      900,
    );
    await assertInViewport(page.getByTestId("cop-summary-identity"), 1440, 900);
    await assertInViewport(
      page.getByTestId("cop-summary-authority"),
      1440,
      900,
    );
    await assertInViewport(
      page.getByTestId("cop-summary-propulsion"),
      1440,
      900,
    );
    await assertInViewport(
      page.getByTestId("cop-summary-freshness"),
      1440,
      900,
    );
    expect(boxesOverlap(roster1440, canvas1440)).toBe(false);
    expect(boxesOverlap(canvas1440, inspector1440)).toBe(false);

    // Phase C: Stage #40 Diagnostics draft + Analysis query, then verify keyboard open/close, focus return, scoped Escape vs camera tether, and Focus Map at 1024x768.
    await page.getByTestId("workspace-tab-bench").click();
    await page
      .locator('[data-param-key="uxs.control.heading_kp"] input[type="number"]')
      .fill("3.35");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");

    await page.getByTestId("workspace-tab-cortex").click();
    await page.getByTestId("cortex-tier-semantic").click();
    await page.getByTestId("cortex-query-input").fill("uxs.domain.kind");
    await page.getByTestId("cortex-query-submit").click();
    await expect(page.getByTestId("cortex-session-status")).toHaveAttribute(
      "data-status",
      "completed",
      { timeout: 10_000 },
    );

    await page.getByTestId("workspace-tab-cop").click();
    await page.setViewportSize({ width: 1024, height: 768 });
    await expect(page.getByTestId("cop-roster-toggle")).toHaveAttribute(
      "aria-expanded",
      "false",
    );

    // Keyboard open Roster via Enter, focus inside Roster body, and close via Escape -> focus returns to cop-roster-toggle!
    const rosterToggle = page.getByTestId("cop-roster-toggle");
    await rosterToggle.focus();
    await page.keyboard.press("Enter");
    await expect(rosterToggle).toHaveAttribute("aria-expanded", "true");
    await expect(page.locator("#cop-roster-body")).toBeVisible();

    await page.getByTestId("cop-roster-vehicle").first().focus();
    await page.keyboard.press("Escape");
    await expect(rosterToggle).toHaveAttribute("aria-expanded", "false");
    await expect(rosterToggle).toBeFocused();

    // Tether camera ("t"), focus inside Inspector body, press Escape -> 1st Escape closes Inspector + focuses toggle while preserving camera tether; 2nd Escape untethers!
    await page.keyboard.press("t");
    await expect
      .poll(() => page.evaluate(() => window.location.hash))
      .toContain("tether=");

    const inspectorToggle = page.getByTestId("cop-inspector-toggle");
    await page.getByTestId("cop-solo-tab-summary").focus();
    await page.keyboard.press("Escape");
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");
    await expect(inspectorToggle).toBeFocused();
    expect(await page.evaluate(() => window.location.hash)).toContain(
      "tether=",
    );

    await page.keyboard.press("Escape");
    expect(await page.evaluate(() => window.location.hash)).not.toContain(
      "tether=",
    );

    // Changing tactical layers, domain filters, declutter, or camera controls preserves manually collapsed Inspector, whereas picking UXS #1 on TacticalCanvas (even when already selected) reopens it!
    await expect
      .poll(() => new URL(page.url()).hash)
      .toContain(`sel=${info.vehicles.quadcopter}`);
    const trailsBtn = page
      .getByRole("group", { name: "Tactical layers" })
      .getByRole("button", { name: "trails" });
    await trailsBtn.click();
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");
    await trailsBtn.click();
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");

    const roverDomainBtn = page
      .getByRole("group", { name: "UXS domain filter" })
      .getByRole("button", { name: "rover" });
    await roverDomainBtn.click();
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");
    await roverDomainBtn.click();
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");

    const tetherToolbarBtn = page
      .getByRole("group", { name: "Camera tracking" })
      .getByRole("button", { name: /^Tether/ });
    await tetherToolbarBtn.click();
    await expect(tetherToolbarBtn).toHaveAttribute("aria-pressed", "true");
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");
    expect(new URL(page.url()).hash).toContain(
      `sel=${info.vehicles.quadcopter}`,
    );

    await page.evaluate(
      () => new Promise((resolve) => window.requestAnimationFrame(resolve)),
    );
    const compactCanvasSize = await page
      .locator(".safety-hud-canvas")
      .evaluate((el) => ({ w: el.clientWidth, h: el.clientHeight }));
    await page.locator(".safety-hud-canvas").click({
      position: {
        x: compactCanvasSize.w * 0.5,
        y: compactCanvasSize.h * 0.5,
      },
    });
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "true");
    await tetherToolbarBtn.click();
    await expect(tetherToolbarBtn).toHaveAttribute("aria-pressed", "false");

    // Collapse again, verify digit hotkey "1" also reopens collapsed Inspector, and Focus Map moves focus directly to TacticalCanvas!
    await inspectorToggle.click();
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "false");
    await page.keyboard.press("1");
    await expect(inspectorToggle).toHaveAttribute("aria-expanded", "true");
    await page.getByTestId("cop-focus-map-btn").click();
    await expect(page.locator(".safety-hud-canvas")).toBeFocused();

    // Phase D: Growing roster (15 ownship + 2 external contacts), multiple active faults, and 140-char long names — zero horizontal overflow + persistent topbar + unobstructed focus!
    const longContactTitle =
      "CONTACT-LONG-NAME-" + "COASTAL-RADAR-TRACK-ALPHA-".repeat(5);
    await copStack.upsertExternalContact({
      id: `${info.org}.${info.platform}.tak.cop.vessel.longtrack`,
      title: longContactTitle,
      contact_type: "surface-vessel",
      source: "ais-coastal-gateway-long-source-identifier",
      feed_status: "live",
      latitude: 30.2685,
      longitude: -97.7418,
      uncertainty_meters: 25,
    });

    const longVehicleTitle =
      "UXS-LONG-CALLSIGN-" + "AUTONOMOUS-MARITIME-PATROL-".repeat(5);
    const nowIso = new Date().toISOString();
    expect(activeGraphSocket).not.toBeNull();
    for (let sysid = 4; sysid <= 15; sysid += 1) {
      const entityId = `${info.org}.${info.platform}.robotics.uxs.vehicle.${sysid}`;
      const domain =
        sysid % 3 === 0
          ? "surface-boat"
          : sysid % 2 === 0
            ? "rover"
            : "quadcopter";
      const authority =
        sysid === 4
          ? "mcu-failsafe"
          : sysid === 5
            ? "direct-crsf-override"
            : "companion-teleop";
      const mk = (predicate: string, object: unknown) => ({
        subject: entityId,
        predicate,
        object,
        source: "mavlink_0xb1",
        timestamp: nowIso,
        confidence: 1,
      });
      activeGraphSocket!.send(
        JSON.stringify({
          type: "put",
          bucket: "ENTITY_STATES",
          key: entityId,
          revision: 10_000 + sysid,
          payload: {
            id: entityId,
            updated_at: nowIso,
            triples: [
              mk("uxs.domain.kind", domain),
              mk("uxs.authority.state", authority),
              mk("uxs.mode.companion", "teleop-velocity"),
              mk("uxs.link.tier", "mesh-high-rate"),
              mk("uxs.health.armed", sysid % 2 === 0),
              mk("uxs.health.flags", {
                clock_sync_locked: sysid !== 6,
                teleop_deadman_clamped: sysid === 4 || sysid === 6,
                crsf_link_healthy: sysid !== 4,
                companion_trunk_healthy: sysid !== 4,
              }),
              mk("uxs.health.failsafes", sysid <= 6 ? 4 : 0),
              mk("uxs.health.crcerrors", sysid <= 6 ? 12 : 0),
              mk("uxs.health.loopdtus", sysid <= 6 ? 2400 : 1000),
              mk("uxs.health.maxloopdtus", sysid <= 6 ? 4800 : 1200),
              mk("uxs.health.rttus", 650),
              mk("uxs.attitude.quaternion", [1, 0, 0, 0]),
              mk("uxs.attitude.rates", [0.01, -0.02, 0.03]),
              mk("uxs.kinematics.ned", [sysid * 4, -sysid * 2, -18]),
              mk("uxs.kinematics.vx", 1.5),
              mk("uxs.kinematics.vy", -0.4),
              mk("uxs.kinematics.vz", 0.0),
              mk("uxs.kinematics.headingdeg", (sysid * 22.5) % 360),
              mk("geo.location.latitude", 30.2672 + sysid * 0.0005),
              mk("geo.location.longitude", -97.7431 - sysid * 0.0005),
              mk("geo.location.altitude", 150 + sysid),
              mk(
                "dc.terms.title",
                sysid === 4 ? longVehicleTitle : `Patrol Unit ${sysid}`,
              ),
              mk("entity.indexing.profile", "signal"),
              mk("uxs.observation.timestamp", nowIso),
            ],
          },
        }),
      );
    }

    await expect(rosterToggle).toContainText("Roster (17)", {
      timeout: 10_000,
    });
    await expect(page.getByTestId("fleet-fault-badge")).toBeVisible();

    // Open Roster at 1024x768, select long-named faulted UXS #4, scroll #cop-roster-body to bottom, and verify topbar stays visible + focused item is unobstructed!
    await rosterToggle.click();
    await expect(page.locator("#cop-roster-body")).toBeVisible();
    await page
      .locator(
        `[data-testid="cop-roster-vehicle"][data-entity-id="${info.org}.${info.platform}.robotics.uxs.vehicle.4"]`,
      )
      .click();
    await expect(page.getByTestId("cop-summary-title")).toHaveAttribute(
      "title",
      longVehicleTitle,
    );
    await assertZeroHorizontalPageOverflow();

    const rosterScrollCheck = await page.evaluate(() => {
      const body = document.getElementById("cop-roster-body");
      if (!body) return null;
      body.scrollTop = body.scrollHeight;
      const buttons = body.querySelectorAll<HTMLButtonElement>(
        '[data-testid="cop-roster-vehicle"], [data-testid="cop-roster-contact"]',
      );
      const lastBtn = buttons[buttons.length - 1] ?? null;
      lastBtn?.focus();
      const topbarRect = document
        .querySelector('[data-testid="invariant-safety-topbar"]')!
        .getBoundingClientRect();
      const btnRect = lastBtn?.getBoundingClientRect() ?? null;
      const hit = btnRect
        ? document.elementFromPoint(
            btnRect.left + btnRect.width * 0.5,
            btnRect.top + btnRect.height * 0.5,
          )
        : null;
      return {
        windowScrollY: window.scrollY,
        rosterScrollTop: body.scrollTop,
        topbarTop: topbarRect.top,
        topbarBottom: topbarRect.bottom,
        btnTop: btnRect?.top ?? -1,
        unobstructed: Boolean(
          lastBtn && hit && (hit === lastBtn || lastBtn.contains(hit)),
        ),
      };
    });
    expect(rosterScrollCheck).not.toBeNull();
    expect(rosterScrollCheck!.windowScrollY).toBe(0);
    expect(rosterScrollCheck!.rosterScrollTop).toBeGreaterThan(0);
    expect(rosterScrollCheck!.topbarTop).toBeGreaterThanOrEqual(0);
    expect(rosterScrollCheck!.btnTop).toBeGreaterThanOrEqual(
      rosterScrollCheck!.topbarBottom - 1,
    );
    expect(rosterScrollCheck!.unobstructed).toBe(true);

    // Switch to Exceptions (multiple active faults) and select long-named external contact -> zero horizontal overflow at both 1024x768 and 1440x900!
    await page.getByTestId("cop-solo-tab-alerts").click();
    await assertZeroHorizontalPageOverflow();
    await page.getByTestId("cop-solo-tab-summary").click();
    await page
      .locator(
        `[data-testid="cop-roster-contact"][data-entity-id="${info.org}.${info.platform}.tak.cop.vessel.longtrack"]`,
      )
      .click();
    await expect(page.getByTestId("cop-contact-summary")).toBeVisible();
    await assertZeroHorizontalPageOverflow();

    await page.setViewportSize({ width: 1440, height: 900 });
    await assertZeroHorizontalPageOverflow();

    // Phase E: 200% zoom equivalents (720x450 and 512x384 CSS px) — vertical scroll reaches all controls without clipping or horizontal overflow, and sticky topbar preserves focus clearance!
    for (const [zoomW, zoomH] of [
      [720, 450],
      [512, 384],
    ] as const) {
      await page.setViewportSize({ width: zoomW, height: zoomH });
      await expect(page.getByTestId("cop-workspace")).toHaveAttribute(
        "data-layout-tier",
        "stacked",
      );
      await assertZeroHorizontalPageOverflow();

      const reachCheck = await page.evaluate(() => {
        const targets = [
          '[data-testid="invariant-safety-topbar"]',
          ".safety-hud-canvas",
          '[data-testid="cop-solo-rail"]',
          '[data-testid="fleet-filmstrip"]',
        ];
        for (const sel of targets) {
          const el = document.querySelector<HTMLElement>(sel);
          if (!el) return { ok: false, missing: sel };
          el.scrollIntoView({ block: "nearest" });
          const r = el.getBoundingClientRect();
          if (r.width < 40 || r.height < 24) {
            return { ok: false, clipped: sel, w: r.width, h: r.height };
          }
        }
        window.scrollTo(0, document.documentElement.scrollHeight);
        const focusBtn = document.querySelector<HTMLButtonElement>(
          '[data-testid="cop-focus-map-btn"]',
        );
        focusBtn?.focus({ preventScroll: true });
        focusBtn?.scrollIntoView({ block: "nearest" });
        const topbar = document.querySelector<HTMLElement>(
          '[data-testid="invariant-safety-topbar"]',
        )!;
        const topbarRect = topbar.getBoundingClientRect();
        const focusRect = focusBtn!.getBoundingClientRect();
        const hit = document.elementFromPoint(
          focusRect.left + focusRect.width * 0.5,
          focusRect.top + focusRect.height * 0.5,
        );
        const topbarSticky =
          window.getComputedStyle(topbar).position === "sticky";
        return {
          ok: true,
          scrollWidth: document.documentElement.scrollWidth,
          innerWidth: window.innerWidth,
          topbarVisibleWhenSticky: !topbarSticky || topbarRect.bottom > 0,
          focusClearOfTopbar:
            !topbarSticky || focusRect.top >= topbarRect.bottom - 1,
          focusUnobstructed: Boolean(
            focusBtn && hit && (hit === focusBtn || focusBtn.contains(hit)),
          ),
        };
      });

      expect(reachCheck).toEqual({
        ok: true,
        scrollWidth: expect.any(Number),
        innerWidth: zoomW,
        topbarVisibleWhenSticky: true,
        focusClearOfTopbar: true,
        focusUnobstructed: true,
      });
      expect(
        (reachCheck as { scrollWidth: number }).scrollWidth,
      ).toBeLessThanOrEqual(zoomW);

      // Select UXS #1 with SKG details open, scroll until Focus Map is at the top of the viewport (leaving TacticalCanvas offscreen above), press Enter, and verify TacticalCanvas scrolls into view unobscured!
      await page
        .locator(
          `[data-testid="filmstrip-item"][data-entity-id="${info.vehicles.quadcopter}"]`,
        )
        .click();
      await page.getByTestId("cop-solo-tab-skg").click();

      const beforeFocusMap = await page.evaluate(() => {
        const focusBtn = document.querySelector<HTMLButtonElement>(
          '[data-testid="cop-focus-map-btn"]',
        )!;
        const topbar = document.querySelector<HTMLElement>(
          '[data-testid="invariant-safety-topbar"]',
        )!;
        const canvas =
          document.querySelector<HTMLElement>(".safety-hud-canvas")!;
        const topbarSticky =
          window.getComputedStyle(topbar).position === "sticky";
        const clearanceTop = topbarSticky
          ? topbar.getBoundingClientRect().bottom + 4
          : 4;
        const btnAbsTop = window.scrollY + focusBtn.getBoundingClientRect().top;
        window.scrollTo(0, Math.max(0, btnAbsTop - clearanceTop));
        focusBtn.focus({ preventScroll: true });
        const canvasRect = canvas.getBoundingClientRect();
        return {
          scrolledOffscreenAbove: canvasRect.bottom <= clearanceTop + 2,
          activeIsFocusBtn: document.activeElement === focusBtn,
        };
      });
      expect(beforeFocusMap).toEqual({
        scrolledOffscreenAbove: true,
        activeIsFocusBtn: true,
      });

      await page.keyboard.press("Enter");
      await expect(page.locator(".safety-hud-canvas")).toBeFocused();

      const afterFocusMap = await page.evaluate(() => {
        const topbar = document.querySelector<HTMLElement>(
          '[data-testid="invariant-safety-topbar"]',
        )!;
        const canvas =
          document.querySelector<HTMLElement>(".safety-hud-canvas")!;
        const topbarSticky =
          window.getComputedStyle(topbar).position === "sticky";
        const clearanceTop = topbarSticky
          ? topbar.getBoundingClientRect().bottom
          : 0;
        const rect = canvas.getBoundingClientRect();
        const visibleTop = Math.max(rect.top, clearanceTop);
        const visibleBottom = Math.min(rect.bottom, window.innerHeight);
        const sampleX = rect.left + rect.width * 0.5;
        const sampleY = visibleTop + (visibleBottom - visibleTop) * 0.7;
        const hit = document.elementFromPoint(sampleX, sampleY);
        return {
          clearOfTopbar: rect.top >= clearanceTop - 1,
          withinBottom: rect.bottom <= window.innerHeight + 1,
          visibleHeight: visibleBottom - visibleTop,
          unobscuredCanvasHit: hit === canvas,
        };
      });
      expect(afterFocusMap.clearOfTopbar).toBe(true);
      expect(afterFocusMap.withinBottom).toBe(true);
      expect(afterFocusMap.visibleHeight).toBeGreaterThanOrEqual(180);
      expect(afterFocusMap.unobscuredCanvasHit).toBe(true);
    }

    // Verify #40 Diagnostics draft on UXS #1 and Analysis session survived all panel toggles and 1024x768 <-> 1440x900 <-> 200% zoom resizes!
    await page.setViewportSize({ width: 1024, height: 768 });
    await page
      .locator(
        `[data-testid="filmstrip-item"][data-entity-id="${info.vehicles.quadcopter}"]`,
      )
      .click();
    await page.getByTestId("workspace-tab-bench").click();
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    await expect(
      page.locator('[data-param-key="uxs.control.heading_kp"]'),
    ).toContainText("3.35 1/s");

    await page.getByTestId("workspace-tab-cortex").click();
    await expect(page.getByTestId("cortex-query-input")).toHaveValue(
      "uxs.domain.kind",
    );
    await expect(page.getByTestId("cortex-session-status")).toHaveAttribute(
      "data-status",
      "completed",
    );

    expect(copStack.pageErrors).toEqual([]);
    expect(copStack.consoleErrors).toEqual([]);
  });

  // spec: uxs-control / Pin-on-admission MAVLink v2 UDP command egress and 1 Hz GCS liveness
  // spec: uxs-control / Lease-bound at-most-once #84 velocity teleop with LinkTier and authority safety gates
  // spec: uxs-control / ADR-111 non-retrying #76 COMMAND_LONG execution, #77 COMMAND_ACK correlation, and preemption
  // spec: cop-ui / Guarded Vehicle ControlPanel, LinkTier supervisory collapse, sub-2ms hardware preemption release, and SITL fault lab
  test("drives closed-loop 20 Hz #84 velocity teleop, latches P1 CRSF preemption until explicit re-engage, reconciles dropped-ACK #76 commit_unknown without retries, collapses mesh-supervisory pad, gates SITL lab to sitl-local, and keeps #41/#43 non-actuating (#11)", async ({
    copStack,
  }) => {
    const { page, info } = copStack;
    const commandPosts: Array<Record<string, unknown>> = [];
    const teleopPosts: Array<Record<string, unknown>> = [];

    page.on("request", (req) => {
      if (req.method() !== "POST") {
        return;
      }
      const pathname = new URL(req.url()).pathname;
      const body = (req.postDataJSON() ?? {}) as Record<string, unknown>;
      if (pathname === "/control/command") {
        commandPosts.push(body);
      } else if (pathname === "/control/teleop") {
        teleopPosts.push(body);
      }
    });

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto(info.baseURL);

    await expect(page.getByTestId("fleet-nominal-badge")).toHaveText(
      "FLEET NOMINAL (3)",
      { timeout: 15_000 },
    );

    // 1. Global #41 workspace hotkeys and #43 Diagnostics parameter staging MUST trigger zero /control/* HTTP requests.
    for (const key of [
      "1",
      "d",
      "b",
      "k",
      "g",
      "2",
      "1",
      "c",
      "h",
      "t",
      "Escape",
    ]) {
      await page.keyboard.press(key);
    }
    await page.getByTestId("workspace-tab-bench").click();
    await expect(page.getByTestId("workspace-shell")).toHaveAttribute(
      "data-workspace",
      "bench",
    );
    await page
      .locator('[data-param-key="uxs.control.heading_kp"] input[type="number"]')
      .fill("3.4");
    await expect(page.getByTestId("bench-staged-count")).toHaveText("1 staged");
    expect(commandPosts).toHaveLength(0);
    expect(teleopPosts).toHaveLength(0);

    // 2. Open Vehicle [D] on Quadcopter (#1, sitl-local, armed, companion-teleop): unengaged pad ignores hold keys.
    await page.getByTestId("workspace-tab-pilot").click();
    const panel = page.getByTestId("control-panel");
    await expect(panel).toBeVisible();
    await expect(panel).toHaveAttribute("data-sysid", "1");
    await expect(panel).toHaveAttribute("data-link-tier", "sitl-local");
    await expect(panel).toHaveAttribute("data-authority", "companion-teleop");
    await expect(panel).toHaveAttribute("data-engaged", "false");
    await expect(page.getByTestId("sitl-lab-toggle")).toBeVisible();

    await page.keyboard.press("w");
    expect(teleopPosts).toHaveLength(0);

    const settlePilotPaint = async (): Promise<void> => {
      await page.waitForTimeout(250);
      await page.evaluate(
        () =>
          new Promise<void>((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
          ),
      );
    };

    // Let PilotWorkspace's DPR=2 SwiftShader canvases complete initial ResizeObserver allocation/paint/GC before starting the 240 ms teleop lease.
    await settlePilotPaint();

    // 2b. Held Space gesture on Quadcopter (#1) cancelled when workspace shortcut switches selection to SurfaceBoat (#3):
    // releasing Space emits zero Engage requests until a fresh activation on #3.
    await page.getByTestId("teleop-engage-btn").focus();
    await page.keyboard.down(" ");
    await page.keyboard.press("3");
    await expect(panel).toHaveAttribute("data-sysid", "3");
    await page.keyboard.up(" ");
    await page.waitForTimeout(80);
    await expect(panel).toHaveAttribute("data-engaged", "false");
    expect(teleopPosts.filter((p) => p.engage === true)).toHaveLength(0);

    await settlePilotPaint();
    await page.getByTestId("teleop-engage-btn").focus();
    await page.keyboard.press(" ");
    await expect(panel).toHaveAttribute("data-engaged", "true");
    expect(
      teleopPosts.filter((p) => p.sysid === 3 && p.engage === true),
    ).toHaveLength(1);
    expect(
      teleopPosts.filter((p) => p.sysid === 1 && p.engage === true),
    ).toHaveLength(0);
    await page.getByTestId("teleop-engage-btn").click();
    await expect(panel).toHaveAttribute("data-engaged", "false");
    await page.keyboard.press("1");
    await expect(panel).toHaveAttribute("data-sysid", "1");
    await settlePilotPaint();

    // 3. Explicit Engage Teleop focuses velocity pad, streams 20 Hz #84 nudges on hold "w", updates live vehicle groundspeed, and zeroes on release.
    await page.getByTestId("teleop-engage-btn").click();
    await expect(panel).toHaveAttribute("data-engaged", "true");
    await expect(page.getByTestId("teleop-velocity-pad")).toBeFocused();

    await page.keyboard.down("w");
    await expect(page.getByTestId("teleop-axes-readout")).toContainText(
      "vx: 4.0 m/s",
    );
    await expect
      .poll(
        () =>
          teleopPosts.filter(
            (p) => p.sysid === 1 && p.vx === 4 && p.stop !== true,
          ).length,
        { timeout: 5_000 },
      )
      .toBeGreaterThanOrEqual(4);
    await expect(page.getByTestId("hud-readout-gs").locator("dd")).toHaveText(
      "4.0 m/s",
      { timeout: 5_000 },
    );

    await page.keyboard.up("w");
    await expect(page.getByTestId("teleop-axes-readout")).toContainText(
      "vx: 0.0 m/s",
    );
    await expect(page.getByTestId("hud-readout-gs").locator("dd")).toHaveText(
      "0.0 m/s",
      { timeout: 5_000 },
    );

    if ((await panel.getAttribute("data-engaged")) === "true") {
      await page.getByTestId("teleop-engage-btn").click();
      await expect(panel).toHaveAttribute("data-engaged", "false");
    }

    // 4. Hardware P1 CRSF override while holding "w" zeroes sticks on the frame edge, latches PREEMPTED, and does NOT auto-resume when P2 returns.
    await page.getByTestId("teleop-engage-btn").click();
    await expect(panel).toHaveAttribute("data-engaged", "true");
    await expect(page.getByTestId("teleop-velocity-pad")).toBeFocused();
    await page.keyboard.down("w");
    await expect(page.getByTestId("teleop-axes-readout")).toContainText(
      "vx: 4.0 m/s",
    );

    await copStack.pulseCrsfOverride(1);
    await expect(panel).toHaveAttribute("data-engaged", "false");
    await expect(panel).toHaveAttribute(
      "data-authority",
      "direct-crsf-override",
    );
    await expect(page.getByTestId("teleop-axes-readout")).toContainText(
      "vx: 0.0 m/s",
    );
    const preemptBanner = page.getByTestId("control-preemption-banner");
    await expect(preemptBanner).toBeVisible();
    await expect(preemptBanner).toContainText("PREEMPTED");
    await page.keyboard.up("w");

    const teleopCountBeforeRestore = teleopPosts.length;
    await copStack.restoreCompanionTeleop(1);
    await expect(panel).toHaveAttribute("data-authority", "companion-teleop", {
      timeout: 5_000,
    });
    await expect(panel).toHaveAttribute("data-engaged", "false");
    await expect(preemptBanner).toBeVisible();
    await page.waitForTimeout(250);
    expect(teleopPosts.length).toBe(teleopCountBeforeRestore);

    // Explicit re-engagement clears preemption latch; holding Enter across auto-repeats on Disengage Teleop sends one stop and zero re-engages.
    const engageBtn = page.getByTestId("teleop-engage-btn");
    await engageBtn.click();
    await expect(panel).toHaveAttribute("data-engaged", "true");
    await expect(preemptBanner).toHaveCount(0);
    await engageBtn.focus();
    const engagesBeforeHoldEnter = teleopPosts.filter(
      (p) => p.sysid === 1 && p.engage === true,
    ).length;
    const stopsBeforeHoldEnter = teleopPosts.filter(
      (p) => p.sysid === 1 && p.stop === true,
    ).length;
    await page.keyboard.down("Enter");
    await expect(panel).toHaveAttribute("data-engaged", "false");
    await page.keyboard.down("Enter");
    await page.keyboard.down("Enter");
    await page.keyboard.up("Enter");
    await page.waitForTimeout(80);
    await expect(panel).toHaveAttribute("data-engaged", "false");
    expect(
      teleopPosts.filter((p) => p.sysid === 1 && p.stop === true && p.vx === 0)
        .length,
    ).toBe(stopsBeforeHoldEnter + 1);
    expect(
      teleopPosts.filter((p) => p.sysid === 1 && p.engage === true).length,
    ).toBe(engagesBeforeHoldEnter);

    // Holding Space on Disengage Teleop until the confirmed lease expires locally (with no successful renewal)
    // and then releasing Space in Chromium MUST send a scoped Stop for the captured session and zero new Engages.
    await engageBtn.click();
    await expect(panel).toHaveAttribute("data-engaged", "true");
    const spaceEngagedSession = teleopPosts
      .filter((p) => p.sysid === 1 && p.engage === true)
      .at(-1)?.session;
    expect(typeof spaceEngagedSession).toBe("string");

    let failSpaceRenewals = true;
    const stalledRenewals: Array<() => void> = [];
    const spaceRenewalRoute = async (
      route: Parameters<Parameters<typeof page.route>[1]>[0],
    ): Promise<void> => {
      const raw = route.request().postData();
      const parsed = raw
        ? (JSON.parse(raw) as { engage?: boolean; stop?: boolean })
        : {};
      if (failSpaceRenewals && !parsed.engage && !parsed.stop) {
        await new Promise<void>((resolve) => {
          stalledRenewals.push(resolve);
        });
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify({
            code: "lease_expired",
            message: "simulated renewal stall",
          }),
        });
        return;
      }
      await route.continue();
    };
    await page.route("**/control/teleop", spaceRenewalRoute);
    try {
      await engageBtn.focus();
      const engagesBeforeHoldSpace = teleopPosts.filter(
        (p) => p.sysid === 1 && p.engage === true,
      ).length;
      const stopsBeforeHoldSpace = teleopPosts.filter(
        (p) => p.sysid === 1 && p.stop === true,
      ).length;

      await page.keyboard.down(" ");
      await expect(panel).toHaveAttribute("data-engaged", "false", {
        timeout: 2_000,
      });
      await page.keyboard.up(" ");
      await page.waitForTimeout(80);

      await expect(panel).toHaveAttribute("data-engaged", "false");
      expect(
        teleopPosts.filter((p) => p.sysid === 1 && p.engage === true).length,
      ).toBe(engagesBeforeHoldSpace);
      const spaceStops = teleopPosts.filter(
        (p) => p.sysid === 1 && p.stop === true,
      );
      expect(spaceStops.length).toBe(stopsBeforeHoldSpace + 1);
      expect(spaceStops.at(-1)?.session).toBe(spaceEngagedSession);
    } finally {
      failSpaceRenewals = false;
      while (stalledRenewals.length > 0) {
        stalledRenewals.shift()?.();
      }
      await page.waitForTimeout(20);
      await page.unroute("**/control/teleop", spaceRenewalRoute);
    }

    // 5. ADR-111 Decision 3: dropped #77 COMMAND_ACK transitions pending -> commit_unknown with zero retries, then reconciles to confirmed on next #0 HEARTBEAT.
    await copStack.setDropCommandAck(1, 1);
    const cmdCountBeforeDrop = commandPosts.length;
    await page.getByTestId("cmd-hold-position").click();

    const cmdStatus = page.getByTestId("control-command-status");
    await expect(cmdStatus).toHaveAttribute("data-phase", "commit_unknown", {
      timeout: 5_000,
    });
    await expect(cmdStatus).toContainText(
      "Status unconfirmed — awaiting vehicle heartbeat",
    );
    expect(commandPosts.length).toBe(cmdCountBeforeDrop + 1);

    await expect(cmdStatus).toHaveAttribute("data-phase", "confirmed", {
      timeout: 6_000,
    });
    expect(commandPosts.length).toBe(cmdCountBeforeDrop + 1);

    // 6. SITL Fault & Current Lab is omitted on mesh-high-rate/direct-bench and exposed strictly on sitl-local.
    await expect(panel).toHaveAttribute("data-link-tier", "mesh-high-rate");
    await expect(page.getByTestId("sitl-lab-toggle")).toHaveCount(0);
    await copStack.setLinkTier(1, "sitl-local");
    await expect(panel).toHaveAttribute("data-link-tier", "sitl-local", {
      timeout: 5_000,
    });
    await page.getByTestId("sitl-lab-toggle").click();
    await expect(page.getByTestId("sitl-lab-drawer")).toBeVisible();
    await page.getByTestId("sitl-blackout-btn").click();
    await expect(panel).toHaveAttribute("data-authority", "mcu-failsafe", {
      timeout: 3_000,
    });
    await expect(panel).toHaveAttribute("data-authority", "companion-teleop", {
      timeout: 5_000,
    });

    await page
      .locator(
        `[data-testid="filmstrip-item"][data-entity-id="${info.vehicles.surfaceBoat}"]`,
      )
      .click();
    await expect(panel).toHaveAttribute("data-sysid", "3");
    await expect(panel).toHaveAttribute("data-link-tier", "direct-bench");
    await expect(page.getByTestId("sitl-lab-toggle")).toHaveCount(0);

    await copStack.setLinkTier(3, "sitl-local");
    await expect(panel).toHaveAttribute("data-link-tier", "sitl-local", {
      timeout: 5_000,
    });
    await page.getByTestId("sitl-lab-toggle").click();
    await page.getByTestId("sitl-current-north").fill("1.8");
    await page.getByTestId("sitl-current-east").fill("-1.4");
    await page.getByTestId("sitl-current-apply-btn").click();
    await expect(cmdStatus).toHaveAttribute("data-phase", "confirmed", {
      timeout: 5_000,
    });

    // 7. MeshSupervisory LinkTier collapses active 10-30 Hz velocity pad while keeping discrete 1 Hz supervisory commands operational.
    await page
      .locator(
        `[data-testid="filmstrip-item"][data-entity-id="${info.vehicles.rover}"]`,
      )
      .click();
    await expect(panel).toHaveAttribute("data-sysid", "2");
    await expect(panel).toHaveAttribute("data-link-tier", "mesh-high-rate");
    await expect(page.getByTestId("sitl-lab-toggle")).toHaveCount(0);

    await page.getByTestId("cmd-mode-teleop").click();
    await expect(panel).toHaveAttribute("data-authority", "companion-teleop", {
      timeout: 5_000,
    });
    await page.getByTestId("teleop-engage-btn").click();
    await expect(panel).toHaveAttribute("data-engaged", "true");

    await copStack.setLinkTier(2, "mesh-supervisory");
    await expect(panel).toHaveAttribute("data-link-tier", "mesh-supervisory", {
      timeout: 5_000,
    });
    await expect(panel).toHaveAttribute("data-engaged", "false");
    await expect(page.getByTestId("teleop-velocity-pad")).toHaveCount(0);
    await expect(page.getByTestId("control-supervisory-only")).toContainText(
      "SUPERVISORY LINK TIER (1 Hz)",
    );

    await page.getByTestId("cmd-disarm").click();
    await expect(cmdStatus).toHaveAttribute("data-phase", "confirmed", {
      timeout: 5_000,
    });
    await expect(page.getByTestId("armed-badge")).toContainText("DISARMED", {
      timeout: 5_000,
    });
    await page.getByTestId("cmd-arm").click();
    await expect(cmdStatus).toHaveAttribute("data-phase", "confirmed", {
      timeout: 5_000,
    });
    await expect(page.getByTestId("armed-badge")).toContainText("ARMED", {
      timeout: 5_000,
    });

    expect(copStack.pageErrors).toEqual([]);
    const unexpectedConsoleErrors = copStack.consoleErrors.filter(
      (msg) =>
        !/^Failed to load resource: the server responded with a status of 40[39] \((Conflict|Forbidden)\)$/.test(
          msg,
        ),
    );
    expect(unexpectedConsoleErrors).toEqual([]);
  });
});
