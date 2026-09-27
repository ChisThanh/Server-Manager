import type { ReactNode } from "react";

export type Tone = "ok" | "warn" | "err" | "info" | "muted";

const TONES: Record<string, Tone> = {
  running: "ok",
  active: "ok",
  healthy: "ok",
  up: "ok",
  ok: "ok",
  valid: "ok",
  success: "ok",
  done: "ok",
  enabled: "ok",
  listening: "ok",
  resolved: "ok",
  warning: "warn",
  warn: "warn",
  degraded: "warn",
  restarting: "warn",
  paused: "warn",
  pending: "warn",
  starting: "warn",
  activating: "warn",
  reloading: "warn",
  expiring: "warn",
  created: "muted",
  failed: "err",
  error: "err",
  crit: "err",
  critical: "err",
  unhealthy: "err",
  dead: "err",
  down: "err",
  expired: "err",
  firing: "err",
  exited: "muted",
  stopped: "muted",
  inactive: "muted",
  disabled: "muted",
  unknown: "muted",
  cancelled: "muted",
  info: "info",
};

export function toneOf(state: string | undefined | null): Tone {
  return TONES[(state ?? "").toLowerCase()] ?? "muted";
}

/** Colored pill for a state string ("running", "failed"…) or an explicit tone. */
export function StateBadge({ state, tone, children }: { state?: string; tone?: Tone; children?: ReactNode }) {
  const tn = tone ?? toneOf(state);
  return (
    <span className={`ui-badge ${tn}`}>
      <span className="ui-dot" />
      {children ?? state}
    </span>
  );
}

export function Dot({ tone }: { tone: Tone }) {
  return <span className={`ui-dot standalone ${tone}`} />;
}

/** Horizontal usage bar; turns warn/crit at 75/90%. */
export function Bar({ pct, warn = 75, crit = 90 }: { pct: number; warn?: number; crit?: number }) {
  const cls = pct >= crit ? "crit" : pct >= warn ? "warn" : "";
  return (
    <div className={`meter ${cls}`}>
      <div style={{ width: `${Math.max(0, Math.min(100, pct))}%` }} />
    </div>
  );
}
