<!--
  SPDX-License-Identifier: MIT
  Copyright (c) 2026 boxbotix
-->
<script lang="ts">
  import { onMount } from "svelte";
  import TacticalCanvas from "$lib/components/TacticalCanvas.svelte";
  import {
    COP_SOLO_SECTIONS,
    type CopPanelName,
    type CopPanelOverrides,
    type CopSoloSection,
    type FleetFreshnessContext,
    INITIAL_COP_PANEL_OVERRIDES,
    type VehicleFreshnessView,
    buildExternalContactSummary,
    buildFaultedOwnshipEntries,
    buildVehicleBannerView,
    closeCopPanelOnEscape,
    computeFlightMetrics,
    evaluateVehicleFreshness,
    evaluateVehicleSafetySeverity,
    resolveCopPanelLayout,
    resolveLiveVehiclePose,
    revealCopInspectorOnSelect,
    selectBannerPresentation,
    selectCopSoloSection,
    stepTablistIndex,
    summarizeFleetHealth,
    toggleCopPanel,
  } from "$lib/hud/view-model";
  import {
    formatEntityTriples,
    formatTransitionEvent,
  } from "$lib/skg/graphology-adapter";
  import {
    type SkgStore,
    type VehicleEntityState,
    isExternalContactEntity,
    isVehicleEntity,
  } from "$lib/stores/skg.svelte";
  import type { TelemetryStore } from "$lib/stores/telemetry.svelte";
  import type {
    ViewportStore,
    WorkspaceMode,
  } from "$lib/stores/viewport.svelte";

  interface Props {
    telemetryStore: TelemetryStore;
    skgStore: SkgStore;
    viewportStore: ViewportStore;
    freshnessContext?: FleetFreshnessContext;
    now?: () => number;
    onSyncHash?: (hash: string) => void;
    viewportWidth?: number;
    viewportHeight?: number;
    panelOverrides?: CopPanelOverrides;
    onUpdatePanelOverrides?: (next: CopPanelOverrides) => void;
    soloSection?: CopSoloSection;
    onSelectSoloSection?: (next: CopSoloSection) => void;
  }

  let {
    telemetryStore,
    skgStore,
    viewportStore,
    freshnessContext,
    now,
    onSyncHash,
    viewportWidth,
    viewportHeight,
    panelOverrides,
    onUpdatePanelOverrides,
    soloSection,
    onSelectSoloSection,
  }: Props = $props();

  let localViewportWidth = $state(
    typeof window !== "undefined" ? window.innerWidth : 1440,
  );
  let localViewportHeight = $state(
    typeof window !== "undefined" ? window.innerHeight : 900,
  );
  let localPanelOverrides = $state<CopPanelOverrides>(
    INITIAL_COP_PANEL_OVERRIDES,
  );
  let localSoloSection = $state<CopSoloSection>("summary");

  const resolvedWidth = $derived(viewportWidth ?? localViewportWidth);
  const resolvedHeight = $derived(viewportHeight ?? localViewportHeight);
  const resolvedOverrides = $derived(panelOverrides ?? localPanelOverrides);
  const activeSection = $derived(soloSection ?? localSoloSection);
  const panelLayout = $derived(
    resolveCopPanelLayout(resolvedWidth, resolvedHeight, resolvedOverrides),
  );

  let stageEl: HTMLDivElement | undefined = $state();
  let rosterToggleEl: HTMLButtonElement | undefined = $state();
  let rosterBodyEl: HTMLDivElement | undefined = $state();
  let inspectorToggleEl: HTMLButtonElement | undefined = $state();
  let inspectorBodyEl: HTMLDivElement | undefined = $state();
  let summaryTabEl: HTMLButtonElement | undefined = $state();
  let alertsTabEl: HTMLButtonElement | undefined = $state();
  let skgTabEl: HTMLButtonElement | undefined = $state();
  let panelHostEl: HTMLDivElement | undefined = $state();
  let hadPanelFocusBeforeUpdate = false;
  let hadRosterBodyFocusBeforeUpdate = false;
  let hadInspectorBodyFocusBeforeUpdate = false;

  onMount(() => {
    if (viewportWidth !== undefined && viewportHeight !== undefined) {
      return;
    }
    const syncLocalViewport = (): void => {
      localViewportWidth = window.innerWidth;
      localViewportHeight = window.innerHeight;
    };
    syncLocalViewport();
    window.addEventListener("resize", syncLocalViewport);
    return () => {
      window.removeEventListener("resize", syncLocalViewport);
    };
  });

  function updatePanelOverrides(next: CopPanelOverrides): void {
    if (onUpdatePanelOverrides) {
      onUpdatePanelOverrides(next);
    } else {
      localPanelOverrides = next;
    }
  }

  let lastStandaloneSelectedId: string | null | undefined = undefined;
  $effect(() => {
    const selectedId = viewportStore.selectedEntityId;
    if (panelOverrides !== undefined) {
      lastStandaloneSelectedId = selectedId;
      return;
    }
    if (lastStandaloneSelectedId === undefined) {
      lastStandaloneSelectedId = selectedId;
      return;
    }
    if (selectedId !== lastStandaloneSelectedId) {
      lastStandaloneSelectedId = selectedId;
      if (selectedId) {
        localPanelOverrides = revealCopInspectorOnSelect(
          resolvedWidth,
          resolvedHeight,
          localPanelOverrides,
          selectedId,
        );
      }
    }
  });

  const resolvedFreshnessContext = $derived<FleetFreshnessContext | undefined>(
    freshnessContext
      ? {
          telemetryStatus:
            freshnessContext.telemetryStatus ?? telemetryStore.status,
          telemetryLink: freshnessContext.telemetryLink,
          getTelemetryLink: (sysid) =>
            freshnessContext.getTelemetryLink?.(sysid) ??
            telemetryStore.getLinkState(sysid),
          skgStatus: freshnessContext.skgStatus ?? skgStore.status,
          skgSynced: freshnessContext.skgSynced ?? skgStore.synced,
          skgLastSyncedRevision:
            freshnessContext.skgLastSyncedRevision !== undefined
              ? freshnessContext.skgLastSyncedRevision
              : skgStore.lastSyncedRevision,
          nowMs: freshnessContext.nowMs,
        }
      : undefined,
  );

  function getVehicleFreshness(
    vehicle: VehicleEntityState,
  ): VehicleFreshnessView | null {
    if (!resolvedFreshnessContext) {
      return null;
    }
    return evaluateVehicleFreshness(vehicle, {
      telemetryLink:
        resolvedFreshnessContext.telemetryLink ??
        resolvedFreshnessContext.getTelemetryLink?.(vehicle.sysid),
      telemetryStatus: resolvedFreshnessContext.telemetryStatus,
      skgStatus: resolvedFreshnessContext.skgStatus,
      skgSynced: resolvedFreshnessContext.skgSynced,
      nowMs: resolvedFreshnessContext.nowMs,
    });
  }

  const selectedEntity = $derived(viewportStore.selectedEntity);
  const selectedVehicle = $derived(
    isVehicleEntity(selectedEntity) ? selectedEntity : null,
  );
  const selectedContact = $derived(
    isExternalContactEntity(selectedEntity) ? selectedEntity : null,
  );
  const selectedFreshness = $derived(
    selectedVehicle ? getVehicleFreshness(selectedVehicle) : null,
  );

  const vehicleSummary = $derived(
    selectedVehicle
      ? buildVehicleBannerView(selectedVehicle, selectedFreshness)
      : null,
  );
  const poisonedSummary = $derived.by(() => {
    if (selectedVehicle || selectedContact || !selectedEntity) {
      return null;
    }
    const banner = selectBannerPresentation(
      selectedEntity,
      resolvedFreshnessContext,
    );
    return banner?.kind === "poisoned-uxs-vehicle" ? banner : null;
  });
  const contactSummary = $derived(
    selectedContact ? buildExternalContactSummary(selectedContact) : null,
  );

  const livePose = $derived.by(() => {
    if (!selectedVehicle) {
      return null;
    }
    const fast = telemetryStore.getKinematics(selectedVehicle.sysid);
    return resolveLiveVehiclePose(selectedVehicle, {
      fast,
      telemetryLink: telemetryStore.getLinkState(selectedVehicle.sysid),
      telemetryStatus: telemetryStore.status,
      skgStatus: skgStore.status,
      nowMs: freshnessContext?.nowMs,
    });
  });

  const liveMetrics = $derived(
    livePose && livePose.hasLocalPos
      ? computeFlightMetrics(livePose.ned, livePose.vel)
      : null,
  );

  const fleetHealth = $derived(
    summarizeFleetHealth(
      skgStore.entities.values(),
      resolvedFreshnessContext ?? {
        telemetryStatus: telemetryStore.status,
        getTelemetryLink: (sysid) => telemetryStore.getLinkState(sysid),
        skgStatus: skgStore.status,
        skgSynced: skgStore.synced,
        skgLastSyncedRevision: skgStore.lastSyncedRevision,
      },
    ),
  );
  const faultedOwnshipEntries = $derived(
    buildFaultedOwnshipEntries(
      skgStore.entities.values(),
      resolvedFreshnessContext,
    ),
  );

  const recentEvents = $derived(
    [...telemetryStore.events]
      .reverse()
      .slice(0, 12)
      .map(formatTransitionEvent),
  );
  const tripleRows = $derived(formatEntityTriples(selectedEntity).slice(0, 16));
  const totalRosterCount = $derived(
    skgStore.vehicles.length + skgStore.externalContacts.length,
  );

  const summaryHasFocusableContent = $derived(
    Boolean((vehicleSummary && selectedVehicle) || contactSummary),
  );
  const alertsHasFocusableContent = $derived(faultedOwnshipEntries.length > 0);
  const skgHasFocusableContent = $derived(Boolean(selectedEntity));

  const panelFocusDeps = $derived(
    `${activeSection}|${selectedEntity?.id ?? ""}|${selectedEntity?.poisoned ? "1" : "0"}|${faultedOwnshipEntries.map((entry) => entry.id).join(",")}|${tripleRows.length}|${summaryHasFocusableContent ? "1" : "0"}|${alertsHasFocusableContent ? "1" : "0"}|${skgHasFocusableContent ? "1" : "0"}`,
  );

  function getSoloTabElement(
    section: CopSoloSection,
  ): HTMLButtonElement | undefined {
    if (section === "summary") return summaryTabEl;
    if (section === "alerts") return alertsTabEl;
    return skgTabEl;
  }

  function handleSelectEntity(entityId: string): void {
    viewportStore.selectEntity(entityId);
    updatePanelOverrides(
      revealCopInspectorOnSelect(
        resolvedWidth,
        resolvedHeight,
        resolvedOverrides,
        entityId,
      ),
    );
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function handleCanvasPickEntity(entityId: string | null): void {
    if (!entityId) {
      return;
    }
    updatePanelOverrides(
      revealCopInspectorOnSelect(
        resolvedWidth,
        resolvedHeight,
        resolvedOverrides,
        entityId,
      ),
    );
  }

  function handleTogglePanel(panel: CopPanelName): void {
    updatePanelOverrides(
      toggleCopPanel(resolvedWidth, resolvedHeight, resolvedOverrides, panel),
    );
  }

  function focusTacticalCanvas(): void {
    const canvasEl = stageEl?.querySelector<HTMLElement>(".safety-hud-canvas");
    if (!canvasEl) {
      return;
    }
    canvasEl.scrollIntoView?.({ block: "nearest", inline: "nearest" });
    if (typeof window !== "undefined" && typeof document !== "undefined") {
      const topbarEl = document.querySelector<HTMLElement>(
        '[data-testid="invariant-safety-topbar"]',
      );
      const topbarSticky =
        topbarEl !== null &&
        window.getComputedStyle(topbarEl).position === "sticky";
      const clearanceTop = topbarSticky
        ? Math.max(0, topbarEl.getBoundingClientRect().bottom + 4)
        : 0;
      const rect = canvasEl.getBoundingClientRect();
      if (rect.top < clearanceTop && window.scrollY > 0) {
        window.scrollBy(0, rect.top - clearanceTop);
      } else if (rect.top >= window.innerHeight) {
        window.scrollBy(0, rect.bottom - window.innerHeight + 8);
      }
    }
    canvasEl.focus({ preventScroll: true });
  }

  function handlePanelEscape(panel: CopPanelName, ev: KeyboardEvent): void {
    if (
      ev.key !== "Escape" ||
      ev.defaultPrevented ||
      ev.repeat ||
      ev.ctrlKey ||
      ev.metaKey ||
      ev.altKey ||
      ev.shiftKey
    ) {
      return;
    }
    const targetNode = ev.target instanceof Node ? ev.target : null;
    const inBody =
      panel === "roster"
        ? Boolean(targetNode && rosterBodyEl?.contains(targetNode))
        : Boolean(targetNode && inspectorBodyEl?.contains(targetNode));
    const result = closeCopPanelOnEscape(
      resolvedWidth,
      resolvedHeight,
      resolvedOverrides,
      inBody ? panel : null,
    );
    if (result.closedPanel !== null) {
      ev.preventDefault();
      updatePanelOverrides(result.overrides);
      const toggleEl =
        result.closedPanel === "roster" ? rosterToggleEl : inspectorToggleEl;
      toggleEl?.focus({ preventScroll: true });
    }
  }

  function switchWorkspace(mode: WorkspaceMode): void {
    viewportStore.setWorkspace(mode);
    onSyncHash?.(viewportStore.toUrlHash());
  }

  function setSoloSection(section: CopSoloSection): void {
    const next = selectCopSoloSection(activeSection, section);
    if (onSelectSoloSection) {
      onSelectSoloSection(next);
    } else {
      localSoloSection = next;
    }
  }

  function handleSoloTabKeydown(
    section: CopSoloSection,
    ev: KeyboardEvent,
  ): void {
    if (ev.altKey || ev.ctrlKey || ev.metaKey) {
      return;
    }
    if (ev.key === "Enter" || ev.key === " ") {
      ev.preventDefault();
      setSoloSection(section);
      return;
    }
    const currentIdx = COP_SOLO_SECTIONS.indexOf(section);
    const nextIdx = stepTablistIndex(
      currentIdx,
      ev.key,
      COP_SOLO_SECTIONS.length,
    );
    if (nextIdx === null) {
      return;
    }
    ev.preventDefault();
    const nextSection = COP_SOLO_SECTIONS[nextIdx] ?? "summary";
    setSoloSection(nextSection);
    getSoloTabElement(nextSection)?.focus({ preventScroll: true });
  }

  $effect.pre(() => {
    void panelLayout.rosterOpen;
    void panelLayout.inspectorOpen;
    const activeEl =
      typeof document !== "undefined" ? document.activeElement : null;
    hadRosterBodyFocusBeforeUpdate = Boolean(
      activeEl &&
      activeEl !== document.body &&
      activeEl !== document.documentElement &&
      rosterBodyEl?.contains(activeEl),
    );
    hadInspectorBodyFocusBeforeUpdate = Boolean(
      activeEl &&
      activeEl !== document.body &&
      activeEl !== document.documentElement &&
      inspectorBodyEl?.contains(activeEl),
    );
  });

  $effect(() => {
    const rosterOpen = panelLayout.rosterOpen;
    const inspectorOpen = panelLayout.inspectorOpen;
    const checkRoster = hadRosterBodyFocusBeforeUpdate;
    const checkInspector = hadInspectorBodyFocusBeforeUpdate;
    hadRosterBodyFocusBeforeUpdate = false;
    hadInspectorBodyFocusBeforeUpdate = false;
    if (viewportStore.workspace !== "cop" || viewportStore.lightsOut) {
      return;
    }
    if (checkRoster && !rosterOpen) {
      rosterToggleEl?.focus({ preventScroll: true });
      return;
    }
    if (checkInspector && !inspectorOpen) {
      inspectorToggleEl?.focus({ preventScroll: true });
    }
  });

  $effect.pre(() => {
    void panelFocusDeps;
    const activeEl =
      typeof document !== "undefined" ? document.activeElement : null;
    hadPanelFocusBeforeUpdate = Boolean(
      activeEl &&
      activeEl !== document.body &&
      activeEl !== document.documentElement &&
      panelHostEl?.contains(activeEl),
    );
  });

  $effect(() => {
    void panelFocusDeps;
    const shouldCheck = hadPanelFocusBeforeUpdate;
    hadPanelFocusBeforeUpdate = false;
    if (
      !shouldCheck ||
      !panelLayout.inspectorOpen ||
      viewportStore.workspace !== "cop" ||
      viewportStore.lightsOut
    ) {
      return;
    }
    const activeEl =
      typeof document !== "undefined" ? document.activeElement : null;
    if (
      !activeEl ||
      activeEl === document.body ||
      activeEl === document.documentElement ||
      !activeEl.isConnected ||
      activeEl.closest("[hidden]") !== null ||
      (activeEl.getAttribute("role") === "tabpanel" &&
        !activeEl.hasAttribute("tabindex"))
    ) {
      getSoloTabElement(activeSection)?.focus({ preventScroll: true });
    }
  });
</script>

<div
  bind:this={stageEl}
  class="cop-stage"
  class:lights-out={viewportStore.lightsOut}
  data-testid="cop-workspace"
  data-workspace-stage="cop"
  data-layout-tier={panelLayout.tier}
  data-roster-open={panelLayout.rosterOpen ? "true" : "false"}
  data-inspector-open={panelLayout.inspectorOpen ? "true" : "false"}
  role="region"
  aria-label="Fleet workspace"
  tabindex="-1"
>
  {#if !viewportStore.lightsOut}
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <aside
      class="cop-roster"
      class:collapsed={!panelLayout.rosterOpen}
      data-testid="cop-roster"
      aria-label="Fleet and external contact roster"
      onkeydown={(ev) => handlePanelEscape("roster", ev)}
    >
      <div class="rail-toolbar">
        <button
          bind:this={rosterToggleEl}
          type="button"
          class="panel-toggle-btn"
          data-testid="cop-roster-toggle"
          aria-expanded={panelLayout.rosterOpen}
          aria-controls="cop-roster-body"
          onclick={() => handleTogglePanel("roster")}
        >
          <span class="panel-toggle-label">Roster ({totalRosterCount})</span>
          <span class="panel-toggle-state">
            {panelLayout.rosterOpen ? "Hide" : "Show"}
          </span>
        </button>
      </div>

      <div
        id="cop-roster-body"
        bind:this={rosterBodyEl}
        class="roster-body"
        hidden={!panelLayout.rosterOpen}
      >
        <div class="roster-group">
          <h2 class="rail-heading">OWNSHIP UXS ({skgStore.vehicles.length})</h2>
          {#if skgStore.vehicles.length === 0}
            <p class="rail-empty">
              {#if fleetHealth.state === "connecting"}
                Waiting for Fleet data sync…
              {:else if fleetHealth.state === "unavailable"}
                Fleet data stream offline.
              {:else}
                No vehicles detected.
              {/if}
            </p>
          {:else}
            <ul class="roster-list">
              {#each skgStore.vehicles as vehicle (vehicle.id)}
                {@const vFresh = getVehicleFreshness(vehicle)}
                {@const isStale = Boolean(vFresh && !vFresh.fresh)}
                {@const sev = evaluateVehicleSafetySeverity(vehicle, {
                  isStale,
                })}
                <li>
                  <button
                    type="button"
                    class="roster-btn"
                    class:selected={viewportStore.selectedEntityId ===
                      vehicle.id}
                    data-testid="cop-roster-vehicle"
                    data-entity-id={vehicle.id}
                    data-stale={isStale ? "true" : "false"}
                    title={`#${vehicle.sysid} ${vehicle.title ?? vehicle.domain}`}
                    onclick={() => handleSelectEntity(vehicle.id)}
                  >
                    <span class="dot vehicle-dot severity-{sev}"></span>
                    <span class="roster-title">
                      #{vehicle.sysid}
                      {vehicle.title ?? vehicle.domain}
                    </span>
                    <span class="roster-sub">
                      {vehicle.domain} · {vehicle.armed
                        ? "ARMED"
                        : "DISARMED"}{isStale && vFresh
                        ? ` · LAST KNOWN (${vFresh.ageLabel})`
                        : ""}
                    </span>
                  </button>
                </li>
              {/each}
            </ul>
          {/if}
        </div>

        <div class="roster-group">
          <h2 class="rail-heading">
            EXTERNAL CONTACTS ({skgStore.externalContacts.length})
          </h2>
          {#if skgStore.externalContacts.length === 0}
            <p class="rail-empty">No external contacts ingested.</p>
          {:else}
            <ul class="roster-list">
              {#each skgStore.externalContacts as contact (contact.id)}
                <li>
                  <button
                    type="button"
                    class="roster-btn"
                    class:selected={viewportStore.selectedEntityId ===
                      contact.id}
                    data-testid="cop-roster-contact"
                    data-entity-id={contact.id}
                    title={contact.title ?? contact.id}
                    onclick={() => handleSelectEntity(contact.id)}
                  >
                    <span class="dot contact-dot"></span>
                    <span class="roster-title"
                      >{contact.title ?? contact.id}</span
                    >
                    <span class="roster-sub">{contact.feedStatus}</span>
                  </button>
                </li>
              {/each}
            </ul>
          {/if}
        </div>
      </div>
    </aside>
  {/if}

  <div class="cop-canvas-wrap">
    <TacticalCanvas
      {telemetryStore}
      {skgStore}
      {viewportStore}
      {now}
      {onSyncHash}
      onPickEntity={handleCanvasPickEntity}
    />
  </div>

  {#if !viewportStore.lightsOut}
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <aside
      class="cop-solo-rail"
      class:collapsed={!panelLayout.inspectorOpen}
      data-testid="cop-solo-rail"
      aria-label="Fleet Solo-Mode inspector"
      onkeydown={(ev) => handlePanelEscape("inspector", ev)}
    >
      <div class="rail-toolbar">
        <button
          bind:this={inspectorToggleEl}
          type="button"
          class="panel-toggle-btn"
          data-testid="cop-inspector-toggle"
          aria-expanded={panelLayout.inspectorOpen}
          aria-controls="cop-inspector-body"
          onclick={() => handleTogglePanel("inspector")}
        >
          <span class="panel-toggle-label">Inspector</span>
          <span class="panel-toggle-state">
            {panelLayout.inspectorOpen ? "Hide" : "Show"}
          </span>
        </button>
        <button
          type="button"
          class="focus-map-btn"
          data-testid="cop-focus-map-btn"
          onclick={focusTacticalCanvas}
        >
          Focus Map
        </button>
      </div>

      <div
        id="cop-inspector-body"
        bind:this={inspectorBodyEl}
        class="inspector-body"
        hidden={!panelLayout.inspectorOpen}
      >
        <div
          class="solo-tabs"
          role="tablist"
          aria-label="Fleet inspector sections"
        >
          <button
            bind:this={summaryTabEl}
            id="cop-solo-tab-summary"
            type="button"
            role="tab"
            class="solo-tab"
            class:active={activeSection === "summary"}
            aria-selected={activeSection === "summary"}
            aria-controls="cop-solo-panel-summary"
            tabindex={activeSection === "summary" ? 0 : -1}
            data-testid="cop-solo-tab-summary"
            onclick={() => setSoloSection("summary")}
            onkeydown={(ev) => handleSoloTabKeydown("summary", ev)}
          >
            Summary
          </button>
          <button
            bind:this={alertsTabEl}
            id="cop-solo-tab-alerts"
            type="button"
            role="tab"
            class="solo-tab"
            class:active={activeSection === "alerts"}
            aria-selected={activeSection === "alerts"}
            aria-controls="cop-solo-panel-alerts"
            tabindex={activeSection === "alerts" ? 0 : -1}
            data-testid="cop-solo-tab-alerts"
            onclick={() => setSoloSection("alerts")}
            onkeydown={(ev) => handleSoloTabKeydown("alerts", ev)}
          >
            Exceptions ({fleetHealth.faultedCount})
          </button>
          <button
            bind:this={skgTabEl}
            id="cop-solo-tab-skg"
            type="button"
            role="tab"
            class="solo-tab"
            class:active={activeSection === "skg"}
            aria-selected={activeSection === "skg"}
            aria-controls="cop-solo-panel-skg"
            tabindex={activeSection === "skg" ? 0 : -1}
            data-testid="cop-solo-tab-skg"
            onclick={() => setSoloSection("skg")}
            onkeydown={(ev) => handleSoloTabKeydown("skg", ev)}
          >
            SKG ({tripleRows.length})
          </button>
        </div>

        <div class="solo-panel-host" bind:this={panelHostEl}>
          <div
            id="cop-solo-panel-summary"
            role="tabpanel"
            aria-labelledby="cop-solo-tab-summary"
            class="solo-panel"
            hidden={activeSection !== "summary"}
            tabindex={activeSection === "summary" && !summaryHasFocusableContent
              ? 0
              : undefined}
            data-testid="cop-solo-panel-summary"
          >
            {#if activeSection === "summary"}
              {#if vehicleSummary && selectedVehicle}
                <div class="summary-header" data-testid="cop-summary-identity">
                  <strong>UXS #{vehicleSummary.sysid}</strong>
                  <span class="muted">{vehicleSummary.domain}</span>
                </div>
                {#if selectedVehicle.title}
                  <p
                    class="summary-entity-title"
                    data-testid="cop-summary-title"
                    title={selectedVehicle.title}
                  >
                    {selectedVehicle.title}
                  </p>
                {/if}

                <dl class="kv-grid">
                  <div>
                    <dt>Authority</dt>
                    <dd data-testid="cop-summary-authority">
                      {vehicleSummary.authorityBadge.label}
                    </dd>
                  </div>
                  <div>
                    <dt>Propulsion</dt>
                    <dd data-testid="cop-summary-propulsion">
                      {vehicleSummary.armedBadge.label}
                    </dd>
                  </div>
                  <div>
                    <dt>Link Tier</dt>
                    <dd>{vehicleSummary.linkBadge.label}</dd>
                  </div>
                  <div>
                    <dt>Freshness</dt>
                    <dd data-testid="cop-summary-freshness">
                      {selectedFreshness && !selectedFreshness.fresh
                        ? selectedFreshness.statusLabel
                        : "LIVE"}
                    </dd>
                  </div>
                  <div>
                    <dt>Kinematics</dt>
                    <dd>
                      {#if liveMetrics}
                        GS {liveMetrics.groundspeedMps.toFixed(1)} m/s · ALT {liveMetrics.altitudeRelM.toFixed(
                          1,
                        )} m
                      {:else}
                        NO POS
                      {/if}
                    </dd>
                  </div>
                </dl>

                <div class="jump-actions">
                  <button
                    type="button"
                    class="jump-btn"
                    aria-keyshortcuts={viewportStore.characterShortcutsEnabled
                      ? "D"
                      : undefined}
                    onclick={() => switchWorkspace("pilot")}
                  >
                    {viewportStore.characterShortcutsEnabled
                      ? "Vehicle [D]"
                      : "Vehicle"}
                  </button>
                  <button
                    type="button"
                    class="jump-btn"
                    aria-keyshortcuts={viewportStore.characterShortcutsEnabled
                      ? "B"
                      : undefined}
                    onclick={() => switchWorkspace("bench")}
                  >
                    {viewportStore.characterShortcutsEnabled
                      ? "Diagnostics [B]"
                      : "Diagnostics"}
                  </button>
                  <button
                    type="button"
                    class="jump-btn"
                    aria-keyshortcuts={viewportStore.characterShortcutsEnabled
                      ? "K"
                      : undefined}
                    onclick={() => switchWorkspace("cortex")}
                  >
                    {viewportStore.characterShortcutsEnabled
                      ? "Analysis [K]"
                      : "Analysis"}
                  </button>
                </div>
              {:else if poisonedSummary}
                <div class="summary-header" data-testid="cop-summary-identity">
                  <strong>UXS #{poisonedSummary.sysid}</strong>
                  <span class="chip-error">POISONED</span>
                </div>
                <dl class="kv-grid">
                  <div>
                    <dt>Status</dt>
                    <dd>{poisonedSummary.poisonError ?? "decode-error"}</dd>
                  </div>
                  <div>
                    <dt>Freshness</dt>
                    <dd data-testid="cop-summary-freshness">
                      {poisonedSummary.freshness?.statusLabel ??
                        "LAST KNOWN · age unknown"}
                    </dd>
                  </div>
                </dl>
              {:else if contactSummary}
                <div
                  class="contact-summary-card"
                  data-testid="cop-contact-summary"
                >
                  <div class="summary-header">
                    <span class="contact-badge">EXTERNAL CONTACT</span>
                    <strong
                      class="summary-contact-title"
                      title={contactSummary.title}
                    >
                      {contactSummary.title}
                    </strong>
                  </div>
                  <dl class="kv-grid">
                    <div>
                      <dt>Type</dt>
                      <dd>{contactSummary.contactType}</dd>
                    </div>
                    <div>
                      <dt>Source</dt>
                      <dd>{contactSummary.source}</dd>
                    </div>
                    <div>
                      <dt>Feed</dt>
                      <dd>{contactSummary.feedStatus}</dd>
                    </div>
                    <div>
                      <dt>Uncertainty</dt>
                      <dd>{contactSummary.uncertaintyLabel}</dd>
                    </div>
                    <div>
                      <dt>Observed</dt>
                      <dd>{contactSummary.observedAt}</dd>
                    </div>
                    <div>
                      <dt>Received</dt>
                      <dd>{contactSummary.receivedAt}</dd>
                    </div>
                    <div>
                      <dt>Expires</dt>
                      <dd>{contactSummary.expiresAt}</dd>
                    </div>
                  </dl>
                  <div class="jump-actions">
                    <button
                      type="button"
                      class="jump-btn"
                      aria-keyshortcuts={viewportStore.characterShortcutsEnabled
                        ? "K"
                        : undefined}
                      onclick={() => switchWorkspace("cortex")}
                    >
                      {viewportStore.characterShortcutsEnabled
                        ? "Inspect Provenance in Analysis [K]"
                        : "Inspect Provenance in Analysis"}
                    </button>
                  </div>
                </div>
              {:else}
                <p class="rail-empty">
                  Select a UXS vehicle or external contact on the canvas,
                  roster, or filmstrip.
                </p>
              {/if}
            {/if}
          </div>

          <div
            id="cop-solo-panel-alerts"
            role="tabpanel"
            aria-labelledby="cop-solo-tab-alerts"
            class="solo-panel"
            hidden={activeSection !== "alerts"}
            tabindex={activeSection === "alerts" && !alertsHasFocusableContent
              ? 0
              : undefined}
            data-testid="cop-solo-panel-alerts"
          >
            {#if activeSection === "alerts"}
              {#if faultedOwnshipEntries.length === 0}
                <p class="rail-empty" data-testid="cop-exceptions-empty">
                  {fleetHealth.exceptionsEmptyCopy}
                </p>
              {:else}
                <ul class="exception-list">
                  {#each faultedOwnshipEntries as entry (entry.id)}
                    <li class="exception-card">
                      <button
                        type="button"
                        class="exception-select"
                        onclick={() => handleSelectEntity(entry.id)}
                      >
                        <strong>UXS #{entry.sysid}</strong>
                        <span>{entry.statusLabel}</span>
                      </button>
                      {#if entry.staleLabel}
                        <span
                          class="chip-warning"
                          data-testid="cop-exception-stale"
                        >
                          {entry.staleLabel}
                        </span>
                      {/if}
                      {#if entry.poisoned}
                        <span class="chip-error">POISONED</span>
                      {/if}
                      <div class="chip-wrap">
                        {#each entry.chips as chip (chip.key)}
                          <span class="chip-{chip.severity}">
                            {chip.label}: {chip.value}
                          </span>
                        {/each}
                      </div>
                    </li>
                  {/each}
                </ul>
              {/if}

              <h3 class="sub-heading">Recent Lane 3 Transitions</h3>
              {#if recentEvents.length === 0}
                <p class="rail-empty">No transition events recorded.</p>
              {:else}
                <ul class="event-list">
                  {#each recentEvents as row (row.key)}
                    <li class="event-item">
                      <span>{row.headline}</span>
                      <span class="muted">{row.detail}</span>
                    </li>
                  {/each}
                </ul>
              {/if}
            {/if}
          </div>

          <div
            id="cop-solo-panel-skg"
            role="tabpanel"
            aria-labelledby="cop-solo-tab-skg"
            class="solo-panel"
            hidden={activeSection !== "skg"}
            tabindex={activeSection === "skg" && !skgHasFocusableContent
              ? 0
              : undefined}
            data-testid="cop-solo-panel-skg"
          >
            {#if activeSection === "skg"}
              {#if !selectedEntity}
                <p class="rail-empty">
                  Select an entity to inspect SKG triples.
                </p>
              {:else}
                <div class="skg-meta">
                  <span>rev #{selectedEntity.kvRevision}</span>
                  <button
                    type="button"
                    class="jump-btn"
                    aria-keyshortcuts={viewportStore.characterShortcutsEnabled
                      ? "K"
                      : undefined}
                    onclick={() => switchWorkspace("cortex")}
                  >
                    {viewportStore.characterShortcutsEnabled
                      ? "Open Analysis [K]"
                      : "Open Analysis"}
                  </button>
                </div>
                <ul class="triple-list">
                  {#each tripleRows as row, idx (`${row.predicate}:${idx}`)}
                    <li class="triple-row">
                      <span class="triple-pred">{row.predicate}</span>
                      <strong class="triple-obj">{row.objectText}</strong>
                    </li>
                  {/each}
                </ul>
              {/if}
            {/if}
          </div>
        </div>
      </div>
    </aside>
  {/if}
</div>

<style>
  .cop-stage {
    flex: 1 0 auto;
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 8px;
    min-height: 0;
    min-width: 0;
    max-width: 100%;
  }

  .cop-stage.lights-out {
    grid-template-columns: minmax(0, 1fr);
  }

  .cop-roster,
  .cop-solo-rail {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 8px;
    background: var(--ui-surface-secondary, #101726);
    border: 1px solid var(--ui-border-subtle, #1e293b);
    border-radius: 6px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
    min-width: 0;
    max-width: 100%;
  }

  .cop-roster.collapsed,
  .cop-solo-rail.collapsed {
    gap: 0;
    padding: 6px 8px;
  }

  .rail-toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    flex-shrink: 0;
    min-width: 0;
  }

  .panel-toggle-btn {
    flex: 1 1 auto;
    display: inline-flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    font-size: 10px;
    font-weight: 700;
    cursor: pointer;
    min-width: 0;
    max-width: 100%;
  }

  .panel-toggle-label {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .panel-toggle-state {
    padding: 1px 5px;
    border-radius: 999px;
    background: rgba(59, 130, 246, 0.16);
    color: var(--ui-text-secondary, #94a3b8);
    font-size: 9px;
    flex-shrink: 0;
  }

  .focus-map-btn {
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.14);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    font-size: 10px;
    font-weight: 700;
    white-space: nowrap;
    cursor: pointer;
    flex-shrink: 0;
  }

  .roster-body,
  .inspector-body {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }

  .roster-body[hidden],
  .inspector-body[hidden] {
    display: none;
  }

  .cop-canvas-wrap {
    display: flex;
    flex-direction: column;
    min-height: 240px;
    min-width: 0;
    max-width: 100%;
  }

  .rail-heading,
  .sub-heading {
    margin: 0 0 6px;
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.05em;
    color: var(--ui-text-secondary, #94a3b8);
  }

  .rail-empty,
  .muted {
    margin: 0;
    color: var(--ui-text-muted, #64748b);
    overflow-wrap: anywhere;
  }

  .roster-list,
  .exception-list,
  .event-list,
  .triple-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 0;
  }

  .roster-btn {
    width: 100%;
    max-width: 100%;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 5px 7px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-subtle, #1e293b);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    cursor: pointer;
    text-align: left;
  }

  .roster-btn.selected {
    border-color: var(--ui-border-interactive, #3b82f6);
  }

  .roster-title {
    flex: 1 1 0;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-weight: 600;
  }

  .roster-sub {
    font-size: 10px;
    color: var(--ui-text-muted, #64748b);
    max-width: 52%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    flex-shrink: 0;
  }

  .dot {
    width: 7px;
    height: 7px;
    border-radius: 999px;
    flex-shrink: 0;
  }

  .vehicle-dot.severity-error {
    background: var(--status-error, #ef4444);
  }

  .vehicle-dot.severity-warning {
    background: var(--status-warning, #f59e0b);
  }

  .vehicle-dot.severity-success {
    background: var(--status-success, #22c55e);
  }

  .vehicle-dot.severity-nominal {
    background: var(--ui-text-muted, #64748b);
  }

  .contact-dot {
    background: var(--skg-node-contact, #22d3ee);
  }

  .solo-tabs {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 4px;
    flex-shrink: 0;
    min-width: 0;
  }

  .solo-tab {
    padding: 5px 6px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-strong, #334155);
    background: var(--ui-surface-elevated, #172033);
    color: var(--ui-text-secondary, #94a3b8);
    font: inherit;
    font-size: 10px;
    font-weight: 700;
    cursor: pointer;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .solo-tab.active {
    border-color: var(--ui-border-interactive, #3b82f6);
    color: var(--ui-text-primary, #e2e8f0);
  }

  .solo-panel-host {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .solo-panel {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }

  .solo-panel[hidden] {
    display: none;
  }

  .solo-panel:focus-visible {
    border-radius: 4px;
  }

  .summary-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    min-width: 0;
  }

  .summary-entity-title {
    margin: 0;
    font-size: 11px;
    font-weight: 600;
    color: var(--ui-text-secondary, #94a3b8);
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .summary-contact-title {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .contact-badge {
    padding: 2px 6px;
    border-radius: 4px;
    background: rgba(34, 211, 238, 0.14);
    color: var(--skg-node-contact, #22d3ee);
    border: 1px solid rgba(34, 211, 238, 0.4);
    font-size: 10px;
    font-weight: 700;
    flex-shrink: 0;
    white-space: nowrap;
  }

  .contact-summary-card {
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-width: 0;
  }

  .kv-grid {
    margin: 0;
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 4px;
    min-width: 0;
  }

  .kv-grid div {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    padding: 4px 6px;
    border-radius: 4px;
    background: var(--ui-surface-elevated, #172033);
    min-width: 0;
  }

  .kv-grid dt {
    color: var(--ui-text-muted, #64748b);
    flex-shrink: 0;
  }

  .kv-grid dd {
    margin: 0;
    font-weight: 600;
    text-align: right;
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .jump-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    min-width: 0;
  }

  .jump-btn {
    padding: 4px 8px;
    border-radius: 4px;
    border: 1px solid var(--ui-border-interactive, #3b82f6);
    background: rgba(59, 130, 246, 0.14);
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    font-size: 10px;
    cursor: pointer;
    max-width: 100%;
    white-space: nowrap;
  }

  .exception-card,
  .event-item,
  .triple-row {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 6px;
    border-radius: 4px;
    background: var(--ui-surface-elevated, #172033);
    min-width: 0;
  }

  .exception-select {
    display: flex;
    justify-content: space-between;
    gap: 6px;
    border: none;
    padding: 0;
    background: transparent;
    color: var(--ui-text-primary, #e2e8f0);
    font: inherit;
    cursor: pointer;
    min-width: 0;
    text-align: left;
  }

  .exception-select span {
    min-width: 0;
    overflow-wrap: anywhere;
    text-align: right;
  }

  .chip-wrap {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    min-width: 0;
  }

  .chip-error {
    padding: 1px 5px;
    border-radius: 3px;
    font-size: 10px;
    color: var(--status-error, #ef4444);
    border: 1px solid var(--status-error, #ef4444);
    max-width: 100%;
    overflow-wrap: anywhere;
  }

  .chip-warning {
    padding: 1px 5px;
    border-radius: 3px;
    font-size: 10px;
    color: var(--status-warning, #f59e0b);
    border: 1px solid var(--status-warning, #f59e0b);
    max-width: 100%;
    overflow-wrap: anywhere;
  }

  .event-item span {
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .skg-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    min-width: 0;
  }

  .triple-pred {
    color: var(--ui-text-muted, #64748b);
    font-size: 10px;
    overflow-wrap: anywhere;
  }

  .triple-obj {
    word-break: break-all;
  }

  @media (min-width: 900px) and (min-height: 700px) {
    .cop-stage {
      flex: 1 1 0;
      height: 100%;
      max-height: 100%;
      overflow: hidden;
    }

    .cop-stage.lights-out {
      grid-template-columns: minmax(0, 1fr);
      grid-template-rows: minmax(0, 1fr);
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="true"] {
      grid-template-columns: 196px minmax(0, 1fr) 288px;
      grid-template-rows: minmax(0, 1fr);
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="true"]
      .cop-roster {
      grid-column: 1;
      grid-row: 1;
      min-height: 0;
      overflow: hidden;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="true"]
      .cop-canvas-wrap {
      grid-column: 2;
      grid-row: 1;
      min-height: 0;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="true"]
      .cop-solo-rail {
      grid-column: 3;
      grid-row: 1;
      min-height: 0;
      overflow: hidden;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="true"] {
      grid-template-columns: minmax(0, 1fr) 296px;
      grid-template-rows: auto minmax(0, 1fr);
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="true"]
      .cop-roster {
      grid-column: 1;
      grid-row: 1;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="true"]
      .cop-canvas-wrap {
      grid-column: 1;
      grid-row: 2;
      min-height: 0;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="true"]
      .cop-solo-rail {
      grid-column: 2;
      grid-row: 1 / 3;
      min-height: 0;
      overflow: hidden;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="false"] {
      grid-template-columns: 196px minmax(0, 1fr);
      grid-template-rows: minmax(0, 1fr) auto;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="false"]
      .cop-roster {
      grid-column: 1;
      grid-row: 1 / 3;
      min-height: 0;
      overflow: hidden;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="false"]
      .cop-canvas-wrap {
      grid-column: 2;
      grid-row: 1;
      min-height: 0;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="false"]
      .cop-solo-rail {
      grid-column: 2;
      grid-row: 2;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="false"] {
      grid-template-columns: minmax(0, 1fr);
      grid-template-rows: auto minmax(0, 1fr) auto;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="false"]
      .cop-roster {
      grid-column: 1;
      grid-row: 1;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="false"]
      .cop-canvas-wrap {
      grid-column: 1;
      grid-row: 2;
      min-height: 0;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="false"]
      .cop-solo-rail {
      grid-column: 1;
      grid-row: 3;
    }

    .roster-body:not([hidden]) {
      flex: 1 1 0;
      min-height: 0;
      overflow-y: auto;
      overflow-x: hidden;
      padding-right: 2px;
    }

    .inspector-body:not([hidden]) {
      flex: 1 1 0;
      min-height: 0;
      overflow: hidden;
    }

    .solo-panel-host {
      flex: 1 1 0;
      min-height: 0;
      overflow-y: auto;
      overflow-x: hidden;
      padding-right: 2px;
    }
  }

  @media (min-width: 1200px) and (min-height: 700px) {
    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="true"] {
      grid-template-columns: 220px minmax(0, 1fr) 310px;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="false"][data-inspector-open="true"] {
      grid-template-columns: minmax(0, 1fr) 310px;
    }

    .cop-stage:not(
        .lights-out
      )[data-roster-open="true"][data-inspector-open="false"] {
      grid-template-columns: 220px minmax(0, 1fr);
    }
  }
</style>
