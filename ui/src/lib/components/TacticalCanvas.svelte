<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 Complete 360 Studio
  Copyright (c) 2026 boxbotix

  Provenance: Adapted from imperative DPR-aware requestAnimationFrame 2D canvas
  lifecycle in C360Studio/semboids ui/src/lib/components/FlockCanvas.svelte
  @ 37dbdb088438a2601c8556d012ab2c33bec85da2 (MIT).
-->
<script lang="ts">
  import { onMount, type Snippet } from "svelte";
  import { quaternionToEulerDeg } from "$lib/hud/view-model";
  import type { SkgStore, VehicleEntityState } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import {
    COP_LAYERS,
    DECLUTTER_LEVELS,
    type ViewportStore,
  } from "$lib/stores/viewport.svelte";
  import { UXS_DOMAIN_KINDS, isValidLatLon } from "$lib/uxs";
  import type { ViewportAdapter, WorldPoint } from "$lib/viewport/adapter";
  import {
    NedGridAdapter,
    WakeTrailBook,
    computeCrabAngleVectors,
    resolveContactPalette,
    resolveDomainGlyphKind,
    resolveVehicleSafetyTone,
  } from "$lib/viewport/ned-grid";

  interface Props {
    telemetryStore: TelemetryStore;
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    adapter?: ViewportAdapter;
    surfaceSlot?: Snippet;
    onSyncHash?: (hash: string) => void;
  }

  let {
    telemetryStore,
    skgStore,
    viewportStore,
    adapter = new NedGridAdapter(),
    surfaceSlot,
    onSyncHash,
  }: Props = $props();

  let containerEl: HTMLDivElement | undefined = $state();
  let surfaceCanvasEl: HTMLCanvasElement | undefined = $state();
  let overlayCanvasEl: HTMLCanvasElement | undefined = $state();

  const wakeTrails = new WakeTrailBook();

  interface RenderedHitTarget {
    entityId: string;
    x: number;
    y: number;
    radiusPx: number;
  }

  let hitTargets: RenderedHitTarget[] = [];
  let isDragging = false;
  let dragMoved = false;
  let lastPointerX = 0;
  let lastPointerY = 0;

  const DEG_TO_RAD = Math.PI / 180;

  function resolveVehicleKinematicsSnapshot(v: VehicleEntityState): {
    ned: [number, number, number];
    vel: [number, number, number];
    headingRad: number;
    lla: [number, number, number] | null;
  } {
    const fast = telemetryStore.getKinematics(v.sysid);
    if (fast) {
      const euler = quaternionToEulerDeg(fast.quat_wxyz);
      const headingRad = Number.isFinite(euler.yawRad)
        ? euler.yawRad
        : fast.hdg_deg * DEG_TO_RAD;
      const lla: [number, number, number] | null =
        fast.has_global_pos &&
        isValidLatLon(fast.lat_lon_alt[0], fast.lat_lon_alt[1])
          ? [fast.lat_lon_alt[0], fast.lat_lon_alt[1], fast.lat_lon_alt[2]]
          : null;
      return {
        ned: [fast.ned_xyz[0], fast.ned_xyz[1], fast.ned_xyz[2]],
        vel: [fast.ned_vel[0], fast.ned_vel[1], fast.ned_vel[2]],
        headingRad,
        lla,
      };
    }
    return {
      ned: [v.ned[0], v.ned[1], v.ned[2]],
      vel: [v.velocityNed[0], v.velocityNed[1], v.velocityNed[2]],
      headingRad: v.headingDeg * DEG_TO_RAD,
      lla: isValidLatLon(v.location.latitude, v.location.longitude)
        ? [v.location.latitude, v.location.longitude, v.location.altitude]
        : null,
    };
  }

  function ensureOriginAnchored(vehicles: readonly VehicleEntityState[]): void {
    const cam = adapter.getCamera();
    if (cam.originLla !== null) {
      return;
    }
    for (const v of vehicles) {
      const snap = resolveVehicleKinematicsSnapshot(v);
      if (
        snap.lla &&
        (snap.lla[0] !== 0 || snap.lla[1] !== 0) &&
        Math.hypot(snap.ned[0], snap.ned[1]) < 10_000
      ) {
        adapter.setCamera({ originLla: snap.lla });
        return;
      }
    }
    for (const contact of skgStore.externalContacts) {
      if (
        contact.location &&
        isValidLatLon(contact.location.latitude, contact.location.longitude) &&
        (contact.location.latitude !== 0 || contact.location.longitude !== 0)
      ) {
        adapter.setCamera({
          originLla: [
            contact.location.latitude,
            contact.location.longitude,
            contact.location.altitude ?? 0,
          ],
        });
        return;
      }
    }
  }

  function applyTetherAndChase(vehicles: readonly VehicleEntityState[]): void {
    const tetherId = viewportStore.tetheredEntityId;
    if (!tetherId) {
      if (!viewportStore.chaseLock && adapter.getCamera().bearingRad !== 0) {
        adapter.setCamera({ bearingRad: 0 });
      }
      return;
    }

    const vehicle = vehicles.find((item) => item.id === tetherId);
    if (vehicle) {
      const snap = resolveVehicleKinematicsSnapshot(vehicle);
      adapter.setCamera({
        centerNed: [snap.ned[0], snap.ned[1]],
        bearingRad: viewportStore.chaseLock ? snap.headingRad : 0,
      });
      return;
    }

    const contact = skgStore.externalContacts.find(
      (item) => item.id === tetherId,
    );
    if (contact?.location) {
      const worldPt: WorldPoint = {
        lla: [
          contact.location.latitude,
          contact.location.longitude,
          contact.location.altitude,
        ],
      };
      adapter.flyTo(worldPt);
      if (!viewportStore.chaseLock) {
        adapter.setCamera({ bearingRad: 0 });
      }
    }
  }

  function drawSurfaceGrid(
    ctx: CanvasRenderingContext2D,
    widthPx: number,
    heightPx: number,
  ): void {
    ctx.clearRect(0, 0, widthPx, heightPx);
    ctx.fillStyle = "#090d16";
    ctx.fillRect(0, 0, widthPx, heightPx);

    if (!viewportStore.isLayerVisible("grid")) {
      return;
    }

    const cam = adapter.getCamera();
    const rawStepM = 90 / cam.zoomPxPerM;
    const mag = Math.pow(10, Math.floor(Math.log10(Math.max(1, rawStepM))));
    const norm = rawStepM / mag;
    const ringStepM = norm < 2 ? mag : norm < 5 ? 2 * mag : 5 * mag;

    const cx = widthPx * 0.5;
    const cy = heightPx * 0.5;
    const maxRadiusPx = Math.hypot(cx, cy);

    ctx.save();
    ctx.strokeStyle = "rgba(51, 65, 85, 0.45)";
    ctx.lineWidth = 1;
    ctx.font = "11px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.fillStyle = "rgba(148, 163, 184, 0.72)";

    for (
      let ringM = ringStepM;
      ringM * cam.zoomPxPerM < maxRadiusPx;
      ringM += ringStepM
    ) {
      const rPx = ringM * cam.zoomPxPerM;
      ctx.beginPath();
      ctx.arc(cx, cy, rPx, 0, Math.PI * 2);
      ctx.stroke();
      ctx.fillText(`${ringM}m`, cx + 6, cy - rPx + 13);
    }

    // Cardinal North/East crosshair rotated by camera bearing.
    const northCourse = adapter.screenCourseRad(0);
    const eastCourse = adapter.screenCourseRad(Math.PI * 0.5);
    const axisLen = Math.min(widthPx, heightPx) * 0.46;

    ctx.strokeStyle = "rgba(96, 165, 250, 0.28)";
    ctx.beginPath();
    ctx.moveTo(
      cx - Math.sin(northCourse) * axisLen,
      cy + Math.cos(northCourse) * axisLen,
    );
    ctx.lineTo(
      cx + Math.sin(northCourse) * axisLen,
      cy - Math.cos(northCourse) * axisLen,
    );
    ctx.moveTo(
      cx - Math.sin(eastCourse) * axisLen,
      cy + Math.cos(eastCourse) * axisLen,
    );
    ctx.lineTo(
      cx + Math.sin(eastCourse) * axisLen,
      cy - Math.cos(eastCourse) * axisLen,
    );
    ctx.stroke();

    // North arrow marker on outer ring.
    const nx = cx + Math.sin(northCourse) * (axisLen - 10);
    const ny = cy - Math.cos(northCourse) * (axisLen - 10);
    ctx.fillStyle = "#60a5fa";
    ctx.font = "bold 12px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    ctx.fillText("N", nx, ny);
    ctx.restore();
  }

  function drawDomainGlyph(
    ctx: CanvasRenderingContext2D,
    kind: ReturnType<typeof resolveDomainGlyphKind>,
    screenHeadingRad: number,
    strokeHex: string,
    fillHex: string,
  ): void {
    ctx.save();
    ctx.rotate(screenHeadingRad);
    ctx.strokeStyle = strokeHex;
    ctx.fillStyle = fillHex;
    ctx.lineWidth = 2;

    if (kind === "quadcopter") {
      const arm = 9;
      ctx.beginPath();
      ctx.moveTo(-arm, -arm);
      ctx.lineTo(arm, arm);
      ctx.moveTo(arm, -arm);
      ctx.lineTo(-arm, arm);
      ctx.stroke();
      for (const [rx, ry] of [
        [-arm, -arm],
        [arm, -arm],
        [-arm, arm],
        [arm, arm],
      ] as const) {
        ctx.beginPath();
        ctx.arc(rx, ry, 4, 0, Math.PI * 2);
        ctx.fill();
        ctx.stroke();
      }
      // Forward nose chevron.
      ctx.beginPath();
      ctx.moveTo(0, -15);
      ctx.lineTo(4, -8);
      ctx.lineTo(-4, -8);
      ctx.closePath();
      ctx.fill();
      ctx.stroke();
    } else if (kind === "rover") {
      ctx.beginPath();
      ctx.rect(-8, -11, 16, 22);
      ctx.fill();
      ctx.stroke();
      // Left & right treads + forward turret line.
      ctx.beginPath();
      ctx.moveTo(-11, -10);
      ctx.lineTo(-11, 10);
      ctx.moveTo(11, -10);
      ctx.lineTo(11, 10);
      ctx.moveTo(0, 0);
      ctx.lineTo(0, -16);
      ctx.stroke();
    } else if (kind === "surface-boat") {
      // Pointed displacement hull (bow at -y).
      ctx.beginPath();
      ctx.moveTo(0, -15);
      ctx.lineTo(8, -2);
      ctx.lineTo(7, 12);
      ctx.lineTo(-7, 12);
      ctx.lineTo(-8, -2);
      ctx.closePath();
      ctx.fill();
      ctx.stroke();
    }
    ctx.restore();
  }

  function drawReticle(
    ctx: CanvasRenderingContext2D,
    x: number,
    y: number,
    isSelected: boolean,
    isTethered: boolean,
  ): void {
    ctx.save();
    if (isSelected) {
      ctx.strokeStyle = "#f8fafc";
      ctx.lineWidth = 1.6;
      const r = 20;
      const tick = 6;
      ctx.beginPath();
      // Corner brackets
      ctx.moveTo(x - r, y - r + tick);
      ctx.lineTo(x - r, y - r);
      ctx.lineTo(x - r + tick, y - r);
      ctx.moveTo(x + r - tick, y - r);
      ctx.lineTo(x + r, y - r);
      ctx.lineTo(x + r, y - r + tick);
      ctx.moveTo(x - r, y + r - tick);
      ctx.lineTo(x - r, y + r);
      ctx.lineTo(x - r + tick, y + r);
      ctx.moveTo(x + r - tick, y + r);
      ctx.lineTo(x + r, y + r);
      ctx.lineTo(x + r, y + r - tick);
      ctx.stroke();
    }
    if (isTethered) {
      ctx.strokeStyle = "#38bdf8";
      ctx.lineWidth = 1.4;
      ctx.setLineDash([4, 3]);
      ctx.beginPath();
      ctx.arc(x, y, 25, 0, Math.PI * 2);
      ctx.stroke();
    }
    ctx.restore();
  }

  function drawSafetyHudOverlay(
    ctx: CanvasRenderingContext2D,
    widthPx: number,
    heightPx: number,
  ): void {
    ctx.clearRect(0, 0, widthPx, heightPx);
    const nextHits: RenderedHitTarget[] = [];
    // eslint-disable-next-line svelte/prefer-svelte-reactivity
    const activeIds = new Set<string>();

    const vehicles = skgStore.vehicles;
    ensureOriginAnchored(vehicles);
    applyTetherAndChase(vehicles);

    const declutter = viewportStore.declutter;
    const showTrails = viewportStore.isLayerVisible("trails");
    const showContacts = viewportStore.isLayerVisible("contacts");
    const showVehicles = viewportStore.isLayerVisible("vehicles");
    const showAlerts = viewportStore.isLayerVisible("alerts");
    const cam = adapter.getCamera();

    // 1. External COP contacts (non-vehicle diamond markers + uncertainty ring).
    if (showContacts) {
      for (const contact of skgStore.externalContacts) {
        activeIds.add(contact.id);
        if (!contact.location) {
          continue;
        }
        const proj = adapter.project({
          lla: [
            contact.location.latitude,
            contact.location.longitude,
            contact.location.altitude,
          ],
        });
        if (!proj.visible) {
          continue;
        }
        const palette = resolveContactPalette(contact.feedStatus);
        ctx.save();
        if (
          contact.uncertaintyMeters !== null &&
          contact.uncertaintyMeters > 0
        ) {
          ctx.strokeStyle = "rgba(34, 211, 238, 0.25)";
          ctx.lineWidth = 1;
          ctx.setLineDash([3, 3]);
          ctx.beginPath();
          ctx.arc(
            proj.x,
            proj.y,
            Math.max(12, contact.uncertaintyMeters * cam.zoomPxPerM),
            0,
            Math.PI * 2,
          );
          ctx.stroke();
          ctx.setLineDash([]);
        }

        ctx.strokeStyle = palette.strokeHex;
        ctx.fillStyle = palette.fillHex;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.moveTo(proj.x, proj.y - 10);
        ctx.lineTo(proj.x + 10, proj.y);
        ctx.lineTo(proj.x, proj.y + 10);
        ctx.lineTo(proj.x - 10, proj.y);
        ctx.closePath();
        ctx.fill();
        ctx.stroke();

        const isSelected = viewportStore.selectedEntityId === contact.id;
        const isTethered = viewportStore.tetheredEntityId === contact.id;
        drawReticle(ctx, proj.x, proj.y, isSelected, isTethered);

        if (declutter !== "tactical" || isSelected) {
          ctx.fillStyle = "#cbd5e1";
          ctx.font = "11px ui-monospace, SFMono-Regular, Menlo, monospace";
          const label = contact.title ?? contact.contactType ?? "CONTACT";
          ctx.fillText(label, proj.x + 14, proj.y + 4);
        }
        ctx.restore();

        nextHits.push({
          entityId: contact.id,
          x: proj.x,
          y: proj.y,
          radiusPx: 18,
        });
      }
    }

    // 2. Controllable UXS vehicles (wake trail, glyph, USV crab-angle split, safety ring).
    if (showVehicles) {
      for (const vehicle of vehicles) {
        if (!viewportStore.isDomainVisible(vehicle.domain)) {
          continue;
        }
        activeIds.add(vehicle.id);
        const snap = resolveVehicleKinematicsSnapshot(vehicle);
        wakeTrails.record(vehicle.id, snap.ned);

        if (showTrails) {
          const trail = wakeTrails.getTrail(vehicle.id);
          if (trail.length >= 2) {
            ctx.save();
            ctx.strokeStyle = "rgba(96, 165, 250, 0.36)";
            ctx.lineWidth = 1.5;
            ctx.beginPath();
            let started = false;
            for (const pt of trail) {
              const tp = adapter.project({
                ned: [pt.northM, pt.eastM, pt.downM],
              });
              if (!started) {
                ctx.moveTo(tp.x, tp.y);
                started = true;
              } else {
                ctx.lineTo(tp.x, tp.y);
              }
            }
            ctx.stroke();
            ctx.restore();
          }
        }

        const proj = adapter.project({ ned: snap.ned });
        if (!proj.visible) {
          continue;
        }

        const tone = resolveVehicleSafetyTone(vehicle);
        const bowScreenRad = adapter.screenCourseRad(snap.headingRad);
        const crab = computeCrabAngleVectors(
          snap.headingRad,
          snap.vel[0],
          snap.vel[1],
        );

        // Bow heading solid vector + USV/cross-wind velocity-over-ground split vector.
        ctx.save();
        const bowLenPx = 28;
        ctx.strokeStyle = tone.strokeHex;
        ctx.lineWidth = 1.6;
        ctx.beginPath();
        ctx.moveTo(proj.x, proj.y);
        ctx.lineTo(
          proj.x + Math.sin(bowScreenRad) * bowLenPx,
          proj.y - Math.cos(bowScreenRad) * bowLenPx,
        );
        ctx.stroke();

        if (
          crab.hasCourseSplit &&
          (vehicle.domain === "surface-boat" ||
            Math.abs(crab.crabAngleRad) > 0.05)
        ) {
          const cogScreenRad = adapter.screenCourseRad(
            crab.courseOverGroundRad,
          );
          const cogLenPx = Math.min(48, 22 + crab.groundspeedMps * 4);
          ctx.strokeStyle = "#38bdf8";
          ctx.lineWidth = 1.6;
          ctx.setLineDash([4, 3]);
          ctx.beginPath();
          ctx.moveTo(proj.x, proj.y);
          ctx.lineTo(
            proj.x + Math.sin(cogScreenRad) * cogLenPx,
            proj.y - Math.cos(cogScreenRad) * cogLenPx,
          );
          ctx.stroke();
          ctx.setLineDash([]);
        }
        ctx.restore();

        // Alert halo when failsafe / preemption is active and alerts layer is visible.
        if (showAlerts && (tone.tone === "error" || tone.tone === "warning")) {
          ctx.save();
          ctx.strokeStyle = tone.strokeHex;
          ctx.globalAlpha = 0.55;
          ctx.lineWidth = 2;
          ctx.beginPath();
          ctx.arc(proj.x, proj.y, 22, 0, Math.PI * 2);
          ctx.stroke();
          ctx.restore();
        }

        ctx.save();
        ctx.translate(proj.x, proj.y);
        drawDomainGlyph(
          ctx,
          resolveDomainGlyphKind(vehicle.domain),
          bowScreenRad,
          tone.strokeHex,
          tone.fillHex,
        );
        ctx.restore();

        const isSelected = viewportStore.selectedEntityId === vehicle.id;
        const isTethered = viewportStore.tetheredEntityId === vehicle.id;
        drawReticle(ctx, proj.x, proj.y, isSelected, isTethered);

        if (
          declutter === "none" ||
          (declutter === "minimal" && true) ||
          isSelected ||
          tone.tone === "error"
        ) {
          ctx.save();
          ctx.fillStyle = "#e2e8f0";
          ctx.font = "11px ui-monospace, SFMono-Regular, Menlo, monospace";
          const callsign = `#${vehicle.sysid} ${vehicle.domain}`;
          ctx.fillText(callsign, proj.x + 16, proj.y - 4);
          if (declutter === "none") {
            ctx.fillStyle = "#94a3b8";
            const altM = (-snap.ned[2]).toFixed(1);
            const spd = crab.groundspeedMps.toFixed(1);
            ctx.fillText(`${spd}m/s · ${altM}m`, proj.x + 16, proj.y + 10);
          }
          ctx.restore();
        }

        nextHits.push({
          entityId: vehicle.id,
          x: proj.x,
          y: proj.y,
          radiusPx: 20,
        });
      }
    }

    wakeTrails.pruneExcept(activeIds);
    hitTargets = nextHits;
  }

  function get2dContext(
    canvas: HTMLCanvasElement,
  ): CanvasRenderingContext2D | null {
    if (
      typeof navigator !== "undefined" &&
      /jsdom/i.test(navigator.userAgent)
    ) {
      return null;
    }
    try {
      return canvas.getContext("2d");
    } catch {
      return null;
    }
  }

  function resizeCanvases(): void {
    if (!containerEl || !surfaceCanvasEl || !overlayCanvasEl) {
      return;
    }
    const rect = containerEl.getBoundingClientRect();
    const widthPx = Math.max(1, Math.floor(rect.width || 640));
    const heightPx = Math.max(1, Math.floor(rect.height || 420));
    const dpr = Math.max(1, window.devicePixelRatio || 1);

    adapter.setViewportSize(widthPx, heightPx);

    for (const canvas of [surfaceCanvasEl, overlayCanvasEl]) {
      const targetW = Math.floor(widthPx * dpr);
      const targetH = Math.floor(heightPx * dpr);
      if (canvas.width !== targetW || canvas.height !== targetH) {
        canvas.width = targetW;
        canvas.height = targetH;
      }
      get2dContext(canvas)?.setTransform(dpr, 0, 0, dpr, 0, 0);
    }
  }

  onMount(() => {
    resizeCanvases();
    const observer =
      typeof ResizeObserver !== "undefined" && containerEl
        ? new ResizeObserver(() => resizeCanvases())
        : null;
    if (observer && containerEl) {
      observer.observe(containerEl);
    }

    let rafId = 0;
    const renderTick = (): void => {
      if (surfaceCanvasEl && overlayCanvasEl) {
        const { widthPx, heightPx } = adapter.getViewportSize();
        const surfaceCtx = get2dContext(surfaceCanvasEl);
        const overlayCtx = get2dContext(overlayCanvasEl);
        if (surfaceCtx) {
          drawSurfaceGrid(surfaceCtx, widthPx, heightPx);
        }
        if (overlayCtx) {
          drawSafetyHudOverlay(overlayCtx, widthPx, heightPx);
        }
      }
      rafId = window.requestAnimationFrame(renderTick);
    };
    rafId = window.requestAnimationFrame(renderTick);

    return () => {
      window.cancelAnimationFrame(rafId);
      observer?.disconnect();
    };
  });

  function findHitEntity(localX: number, localY: number): string | null {
    let bestId: string | null = null;
    let bestDist = Number.POSITIVE_INFINITY;
    for (const target of hitTargets) {
      const d = Math.hypot(localX - target.x, localY - target.y);
      if (d <= target.radiusPx && d < bestDist) {
        bestDist = d;
        bestId = target.entityId;
      }
    }
    return bestId;
  }

  function handlePointerDown(ev: PointerEvent): void {
    if (ev.button !== 0 || !overlayCanvasEl) {
      return;
    }
    isDragging = true;
    dragMoved = false;
    lastPointerX = ev.clientX;
    lastPointerY = ev.clientY;
    overlayCanvasEl.setPointerCapture?.(ev.pointerId);
  }

  function handlePointerMove(ev: PointerEvent): void {
    if (!isDragging) {
      return;
    }
    const dx = ev.clientX - lastPointerX;
    const dy = ev.clientY - lastPointerY;
    if (Math.hypot(dx, dy) > 3) {
      dragMoved = true;
      if (viewportStore.tetheredEntityId !== null) {
        viewportStore.tetherEntity(null);
        onSyncHash?.(viewportStore.toUrlHash());
      }
    }
    lastPointerX = ev.clientX;
    lastPointerY = ev.clientY;

    const { widthPx, heightPx } = adapter.getViewportSize();
    const nextCenter = adapter.unproject(
      widthPx * 0.5 - dx,
      heightPx * 0.5 - dy,
    );
    adapter.setCamera({ centerNed: [nextCenter.ned[0], nextCenter.ned[1]] });
  }

  function handlePointerUp(ev: PointerEvent): void {
    if (!isDragging || !overlayCanvasEl) {
      return;
    }
    isDragging = false;
    overlayCanvasEl.releasePointerCapture?.(ev.pointerId);
    if (dragMoved) {
      return;
    }
    const rect = overlayCanvasEl.getBoundingClientRect();
    const hitId = findHitEntity(ev.clientX - rect.left, ev.clientY - rect.top);
    viewportStore.selectEntity(hitId);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function handleDoubleClick(ev: MouseEvent): void {
    if (!overlayCanvasEl) {
      return;
    }
    const rect = overlayCanvasEl.getBoundingClientRect();
    const hitId = findHitEntity(ev.clientX - rect.left, ev.clientY - rect.top);
    if (hitId) {
      viewportStore.selectEntity(hitId);
      viewportStore.tetherEntity(hitId);
      onSyncHash?.(viewportStore.toUrlHash());
    }
  }

  function handleWheel(ev: WheelEvent): void {
    ev.preventDefault();
    const cam = adapter.getCamera();
    const factor = ev.deltaY < 0 ? 1.15 : 1 / 1.15;
    adapter.setCamera({ zoomPxPerM: cam.zoomPxPerM * factor });
  }

  function toggleDomain(domain: (typeof UXS_DOMAIN_KINDS)[number]): void {
    const current = viewportStore.domainFilter;
    const next = current.includes(domain)
      ? current.filter((d) => d !== domain)
      : [...current, domain];
    viewportStore.setDomainFilter(next);
    onSyncHash?.(viewportStore.toUrlHash());
  }
</script>

<div
  class="tactical-viewport"
  bind:this={containerEl}
  data-testid="tactical-viewport"
>
  <div class="viewport-surface" data-layer="viewport-surface">
    <canvas
      bind:this={surfaceCanvasEl}
      class="surface-grid-canvas"
      aria-hidden="true"
    ></canvas>
    {#if surfaceSlot}
      {@render surfaceSlot()}
    {/if}
  </div>

  <canvas
    bind:this={overlayCanvasEl}
    class="safety-hud-canvas"
    data-layer="safety-hud-overlay"
    aria-label="Multi-domain tactical COP canvas"
    onpointerdown={handlePointerDown}
    onpointermove={handlePointerMove}
    onpointerup={handlePointerUp}
    ondblclick={handleDoubleClick}
    onwheel={handleWheel}
  ></canvas>

  <div
    class="viewport-toolbar"
    role="toolbar"
    aria-label="COP tactical controls"
  >
    <div class="chip-group" role="group" aria-label="UXS domain filter">
      {#each UXS_DOMAIN_KINDS as domain (domain)}
        <button
          type="button"
          class="chip"
          aria-pressed={viewportStore.isDomainVisible(domain)}
          onclick={() => toggleDomain(domain)}
        >
          {domain}
        </button>
      {/each}
    </div>

    <div class="chip-group" role="group" aria-label="Tactical layers">
      {#each COP_LAYERS as layer (layer)}
        <button
          type="button"
          class="chip"
          aria-pressed={viewportStore.isLayerVisible(layer)}
          onclick={() => {
            viewportStore.toggleLayer(layer);
            onSyncHash?.(viewportStore.toUrlHash());
          }}
        >
          {layer}
        </button>
      {/each}
    </div>

    <div class="chip-group" role="group" aria-label="Camera and declutter">
      <button
        type="button"
        class="chip"
        aria-pressed={viewportStore.tetheredEntityId !== null}
        disabled={viewportStore.selectedEntityId === null &&
          viewportStore.tetheredEntityId === null}
        onclick={() => {
          viewportStore.tetherEntity(
            viewportStore.tetheredEntityId !== null
              ? null
              : viewportStore.selectedEntityId,
          );
          onSyncHash?.(viewportStore.toUrlHash());
        }}
      >
        Tether [T]
      </button>
      <button
        type="button"
        class="chip"
        aria-pressed={viewportStore.chaseLock}
        disabled={viewportStore.selectedEntityId === null &&
          viewportStore.tetheredEntityId === null}
        onclick={() => {
          viewportStore.setChaseLock(!viewportStore.chaseLock);
          onSyncHash?.(viewportStore.toUrlHash());
        }}
      >
        Chase [C]
      </button>
      {#each DECLUTTER_LEVELS as level (level)}
        <button
          type="button"
          class="chip"
          aria-pressed={viewportStore.declutter === level}
          onclick={() => {
            viewportStore.setDeclutter(level);
            onSyncHash?.(viewportStore.toUrlHash());
          }}
        >
          {level}
        </button>
      {/each}
    </div>
  </div>
</div>

<style>
  .tactical-viewport {
    position: relative;
    width: 100%;
    height: 100%;
    min-height: 360px;
    overflow: hidden;
    background: var(--ui-surface-primary, #090d16);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
  }

  .viewport-surface {
    position: absolute;
    inset: 0;
    z-index: 1;
  }

  .surface-grid-canvas,
  .safety-hud-canvas {
    position: absolute;
    inset: 0;
    width: 100%;
    height: 100%;
    display: block;
  }

  .safety-hud-canvas {
    z-index: 2;
    cursor: grab;
    touch-action: none;
  }

  .safety-hud-canvas:active {
    cursor: grabbing;
  }

  .viewport-toolbar {
    position: absolute;
    top: 8px;
    left: 8px;
    right: 8px;
    z-index: 3;
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    pointer-events: none;
  }

  .chip-group {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 4px;
    padding: 4px;
    background: var(--ui-surface-overlay, rgba(16, 23, 38, 0.88));
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    pointer-events: auto;
  }

  .chip {
    background: transparent;
    color: var(--ui-text-secondary, #94a3b8);
    border: 1px solid transparent;
    border-radius: 4px;
    padding: 2px 7px;
    font-size: 11px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    cursor: pointer;
  }

  .chip[aria-pressed="true"] {
    color: var(--ui-text-primary, #e2e8f0);
    background: rgba(59, 130, 246, 0.22);
    border-color: var(--ui-border-interactive, #3b82f6);
  }

  .chip:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
