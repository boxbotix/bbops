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

    // 3. Fleet inspector tablist: single Tab stop, Arrow/Home/End activation, and panel unmount focus recovery.
    await page.keyboard.press("1");
    const summaryTab = page.getByTestId("cop-solo-tab-summary");
    const alertsTab = page.getByTestId("cop-solo-tab-alerts");
    const skgTab = page.getByTestId("cop-solo-tab-skg");

    await summaryTab.focus();
    await expect(summaryTab).toHaveAttribute("tabindex", "0");
    await expect(alertsTab).toHaveAttribute("tabindex", "-1");
    await expect(skgTab).toHaveAttribute("tabindex", "-1");

    // Single Tab stop: pressing Tab from active Summary tab skips inactive tabs and lands inside Summary tabpanel.
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

    await page.keyboard.press("1");
    await summaryTab.focus();
    await page.keyboard.press("ArrowRight");
    await expect(alertsTab).toBeFocused();
    await expect(alertsTab).toHaveAttribute("aria-selected", "true");
    await expect(page.getByTestId("cop-solo-panel-alerts")).toHaveAttribute(
      "role",
      "tabpanel",
    );

    await page.keyboard.press("End");
    await expect(skgTab).toBeFocused();
    await expect(skgTab).toHaveAttribute("aria-selected", "true");

    await page.keyboard.press("Home");
    await expect(summaryTab).toBeFocused();
    await expect(summaryTab).toHaveAttribute("aria-selected", "true");

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

    // 5. Direct topbar click preserves button focus; disabling character shortcuts persists across reload while keeping Escape active.
    await fleetNavBtn.click();
    await expect(fleetNavBtn).toBeFocused();
    await expect(fleetNavBtn).toHaveAttribute("aria-current", "page");

    await page.keyboard.press("1");
    await page.keyboard.press("t");
    const tetherRegularBtn = page
      .getByTestId("viewport-toolbar")
      .getByRole("button", { name: /^Tether\b/ });
    await expect(tetherRegularBtn).toHaveAttribute("aria-pressed", "true");

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
});
