export const UXS_DOMAINS = ["Quadcopter", "Rover", "SurfaceBoat"] as const;

export type UxsDomain = (typeof UXS_DOMAINS)[number];

export function formatShellBanner(version: string): string {
  const normalized = version.trim() || "dev";
  return `bbops COP/GCS (${normalized}) — ${UXS_DOMAINS.join(" · ")}`;
}
