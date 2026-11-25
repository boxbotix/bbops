// SPDX-License-Identifier: MIT
// Copyright (c) 2026 boxbotix

import type { Locator, Page } from "@playwright/test";
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

test.describe("Full-stack Chromium COP workspace end-to-end verification", () => {
  // spec: cop-ui / Full-stack headless browser COP end-to-end verification and visual legibility
  test("streams live multi-domain dual-WebSocket telemetry and SKG state across Canvas2D, WebGL Sigma, non-overlapping HUD readouts, 2-hop pathSearch, hotkeys, and Lane 3 transitions", async ({
    copStack,
  }) => {
    const { page, info } = copStack;
    await page.goto(info.baseURL);

    // 1. Both live WebSockets converge against the embedded Go host + NATS/JetStream SKG.
    await expect(page.locator(".stream-pill").nth(0)).toContainText(
      "WS1 telemetry: connected (3)",
      { timeout: 15_000 },
    );
    await expect(page.locator(".stream-pill").nth(1)).toContainText(
      "WS2 SKG: connected",
      { timeout: 15_000 },
    );
    await expect(page.locator(".stream-pill").nth(1)).toContainText("synced", {
      timeout: 15_000,
    });

    // 2. Real WebGL Sigma topology mounts (never jsdom fallback) with DPR=2 canvases and settles ForceAtlas2 layout.
    await expect(page.getByTestId("skg-topology-fallback")).toHaveCount(0);
    const sigmaStage = page.getByTestId("skg-sigma-stage");
    await expect(sigmaStage).toBeVisible();
    await expect(sigmaStage).toHaveAttribute("data-sigma-ready", "true", {
      timeout: 10_000,
    });
    await expect(page.locator(".skg-topology-card .meta-count")).toContainText(
      "5N · 2E",
      { timeout: 10_000 },
    );
    await expect(
      page.locator(".skg-topology-card .meta-count"),
    ).not.toContainText("layout", { timeout: 10_000 });

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

    // 3. TacticalCanvas DPR=2 layers paint non-blank NED grid & vehicle glyphs, and concentric range-ring labels clear .viewport-toolbar.
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

    // 4. Hit-test Quadcopter #1 directly on the TacticalCanvas (NED [24, -14] at default 4 px/m zoom).
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

    // 5. AuthorityBanner, Triple Inspector, VehicleHud horizon canvas, and non-overlapping HTML readouts.
    const authBanner = page.getByTestId("vehicle-authority-banner");
    await expect(authBanner).toContainText("UXS #1 · quadcopter");
    await expect(page.getByTestId("authority-badge")).toHaveText(
      "P2 CompanionTeleop",
    );
    await expect(page.getByTestId("armed-badge")).toHaveText("ARMED");
    await expect(page.getByTestId("banner-link-tier")).toHaveText("SITL");
    await expect(page.getByTestId("skg-triple-inspector")).toContainText(
      "23 triples",
    );

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

    // Positively verify dark #0f172a hover pill + #f8fafc text and zero #ffffff white-on-white background on canvas.sigma-hovers.
    await expect(sigmaStage).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.quadcopter,
    );
    let quadHover = await sampleSigmaHoverLayer(sigmaStage);
    await expect
      .poll(async () => {
        quadHover = await sampleSigmaHoverLayer(sigmaStage);
        return (
          quadHover.darkPillPixels > 80 &&
          quadHover.lightTextPixels > 8 &&
          quadHover.pureWhitePixels === 0
        );
      })
      .toBe(true);

    // Click Rover #2 directly on the real WebGL Sigma stage and verify bidirectional selection + highlight shift.
    const roverSigmaPos = await locateSigmaNodeViaWebGLPicking(
      sigmaStage,
      info.vehicles.rover,
    );
    expect(roverSigmaPos).not.toBeNull();
    await sigmaStage
      .locator("canvas.sigma-mouse")
      .click({ position: roverSigmaPos! });
    // Move pointer away from Sigma so canvas.sigma-hovers reflects persistent clickNode selection rather than pointer hover.
    await page.mouse.move(8, 8);

    await expect
      .poll(() => new URL(page.url()).hash)
      .toContain(`sel=${info.vehicles.rover}`);
    await expect(authBanner).toContainText("UXS #2 · rover");
    await expect(page.getByTestId("authority-badge")).toHaveText(
      "P3 CompanionAutonomous",
    );
    await expect(page.getByTestId("skg-triple-inspector")).toContainText(
      "companion-autonomous",
    );
    await expect(sigmaStage).toHaveAttribute(
      "data-highlighted-node",
      info.vehicles.rover,
    );
    await expect
      .poll(async () => {
        const roverHover = await sampleSigmaHoverLayer(sigmaStage);
        return (
          roverHover.darkPillPixels > 80 &&
          roverHover.lightTextPixels > 8 &&
          roverHover.pureWhitePixels === 0 &&
          Math.hypot(
            roverHover.centroidX - quadHover.centroidX,
            roverHover.centroidY - quadHover.centroidY,
          ) > 15
        );
      })
      .toBe(true);

    // Click Quadcopter #1 back via the real WebGL Sigma stage before 2-Hop pathSearch expansion.
    const quadSigmaPos = await locateSigmaNodeViaWebGLPicking(
      sigmaStage,
      info.vehicles.quadcopter,
    );
    expect(quadSigmaPos).not.toBeNull();
    await sigmaStage
      .locator("canvas.sigma-mouse")
      .click({ position: quadSigmaPos! });
    await page.mouse.move(8, 8);
    await expect
      .poll(() => new URL(page.url()).hash)
      .toContain(`sel=${info.vehicles.quadcopter}`);
    await expect(authBanner).toContainText("UXS #1 · quadcopter");
    const expandButton = page.getByRole("button", { name: "2-Hop" });
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
    await expect(page.locator(".expand-error")).toHaveCount(0);
    await expect(page.locator(".skg-topology-card .meta-count")).toContainText(
      "5N · 2E",
    );

    // 7. Exercise canvas double-click tether plus keyboard hotkeys T (Tether), C (Chase), H (Declutter), and Escape (Clear).
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

    // Toggle tether off then back on via hotkey T.
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

    // 8. Select SurfaceBoat #3 and verify signed Fossen Ch. 2 crab-angle readout (-30.1°) with zero cell overlap.
    await page.goto(
      `${info.baseURL}/#sel=${encodeURIComponent(info.vehicles.surfaceBoat)}`,
    );
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

    // 9. Inject live Lane 3 P1 CRSF override on SYS#1 and STATUSTEXT on SYS#3; verify timeline + click selection.
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
  test("preserves strict UXS authority and HUD isolation when selecting an external COP contact with unknown optional provenance", async ({
    copStack,
  }) => {
    const { page, info } = copStack;
    await page.goto(
      `${info.baseURL}/#sel=${encodeURIComponent(info.externalContactId)}`,
    );

    await expect(page.locator(".stream-pill").nth(1)).toContainText("synced", {
      timeout: 15_000,
    });

    const contactBanner = page.getByTestId("external-contact-banner");
    await expect(contactBanner).toBeVisible();
    await expect(contactBanner).toContainText("AIS Vessel Harbor-7");
    await expect(contactBanner).toContainText("type: surface-vessel");
    await expect(contactBanner).toContainText("source: ais-receiver-1");
    await expect(contactBanner).toContainText("expires: unknown");
    await expect(contactBanner).toContainText("uncertainty: unknown");

    // Non-UXS external contact MUST NEVER expose vehicle P1-P4 authority, arming, link-tier, or HUD attitude readouts.
    await expect(page.getByTestId("vehicle-hud-standby")).toBeVisible();
    await expect(page.getByTestId("vehicle-authority-banner")).toHaveCount(0);
    await expect(page.getByTestId("authority-badge")).toHaveCount(0);
    await expect(page.getByTestId("armed-badge")).toHaveCount(0);
    await expect(page.getByTestId("hud-link-tier")).toHaveCount(0);
    await expect(page.getByTestId("hud-readout-strip")).toHaveCount(0);

    expect(copStack.pageErrors).toEqual([]);
    expect(copStack.consoleErrors).toEqual([]);
  });
});
