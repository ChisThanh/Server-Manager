import { useEffect, useMemo, useState } from "react";
import { RotateCcw } from "lucide-react";
import { MonitorService } from "../../../bindings/server-manager/services/monitor";
import type { Check, Series } from "../../../bindings/server-manager/services/monitor";
import { formatBytes, formatDuration } from "../../lib/format";
import { t, useT, type Key } from "../../i18n";
import { useInterval } from "../../ui/hooks";
import { StateBadge, type Tone } from "../../ui/Status";

export const RANGES = [
  { id: "5m", sec: 300 },
  { id: "1h", sec: 3600 },
  { id: "6h", sec: 6 * 3600 },
  { id: "24h", sec: 86400 },
  { id: "7d", sec: 7 * 86400 },
  { id: "30d", sec: 30 * 86400 },
] as const;
export type RangeId = (typeof RANGES)[number]["id"];

export type Zoom = [number, number] | null;

/** Fetches history for the selected range (or zoom window), refreshing while visible. */
export function useHistory(connId: string, range: RangeId, zoom: Zoom, visible: boolean) {
  const [data, setData] = useState<Series | null>(null);
  const [error, setError] = useState("");
  const sec = RANGES.find((r) => r.id === range)!.sec;
  const load = async () => {
    const to = zoom ? zoom[1] : Math.floor(Date.now() / 1000);
    const from = zoom ? zoom[0] : to - sec;
    try {
      setData(await MonitorService.History(connId, from, to));
      setError("");
    } catch (e) {
      setError(String(e));
    }
  };
  useEffect(() => {
    if (visible) load();
  }, [connId, range, zoom?.[0], zoom?.[1], visible]);
  useInterval(() => {
    if (visible && !zoom && !document.hidden) load();
  }, visible && !zoom ? (sec <= 3600 ? 10000 : 60000) : null);
  const window: [number, number] = zoom ?? [Math.floor(Date.now() / 1000) - sec, Math.floor(Date.now() / 1000)];
  return { data, error, window, reload: load };
}

/** Columns of a series in uPlot's aligned format. */
export function columns(s: Series | null, keys: string[], scale = 1): [number[], ...(number | null)[][]] {
  if (!s) return [[], ...keys.map(() => [])];
  return [s.ts ?? [], ...keys.map((k) => (s.values?.[k] ?? []).map((v) => (v === null || v === undefined ? null : v * scale)))];
}

export function RangePicker({ value, onChange, zoom, onReset }: { value: RangeId; onChange: (r: RangeId) => void; zoom: Zoom; onReset: () => void }) {
  const tr = useT();
  return (
    <div className="mon-range">
      {zoom && (
        <button className="btn sm" onClick={onReset} title={tr("mon.resetZoom")}>
          <RotateCcw size={12} /> {tr("mon.resetZoom")}
        </button>
      )}
      <div className="segmented">
        {RANGES.map((r) => (
          <button key={r.id} className={value === r.id && !zoom ? "on" : ""} onClick={() => onChange(r.id)}>
            {r.id}
          </button>
        ))}
      </div>
    </div>
  );
}

export const fmtPct = (v: number) => `${v.toFixed(v >= 10 ? 0 : 1)}%`;
export const fmtRate = (v: number) => `${formatBytes(v)}/s`;
export const fmtMs = (v: number) => `${v.toFixed(v >= 10 ? 0 : 1)} ms`;
export const fmtNum = (v: number) => (v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2));
export const fmtBits = (bytesPerSec: number) => {
  const b = bytesPerSec * 8;
  if (b >= 1e9) return `${(b / 1e9).toFixed(1)} Gbps`;
  if (b >= 1e6) return `${(b / 1e6).toFixed(1)} Mbps`;
  if (b >= 1e3) return `${(b / 1e3).toFixed(0)} Kbps`;
  return `${b.toFixed(0)} bps`;
};

export const statusTone = (s: string): Tone => (s === "ok" ? "ok" : s === "warn" ? "warn" : s === "crit" ? "err" : "muted");

export function HealthBadge({ status, ok, total }: { status: string; ok?: number; total?: number }) {
  const tr = useT();
  const label = status === "ok" ? tr("mon.healthy") : status === "warn" ? tr("mon.warning") : status === "crit" ? tr("mon.critical") : tr("mon.unknown");
  return (
    <StateBadge tone={statusTone(status)}>
      {label}
      {total ? ` · ${ok}/${total}` : ""}
    </StateBadge>
  );
}

/** Human text for a health check. */
export function checkText(c: Check): { title: string; detail: string } {
  const k = `mon.check.${c.kind}` as Key;
  const title = c.name ? `${t(k)}: ${c.name}` : t(k);
  let detail = c.detail;
  switch (c.kind) {
    case "cpu":
    case "memory":
    case "swap":
    case "disk":
    case "inode":
      detail = fmtPct(c.value);
      break;
    case "load":
      detail = fmtNum(c.value);
      break;
    case "ssh":
      detail = c.status === "crit" ? c.detail : t("mon.sshLatency", { ms: Math.round(c.value) });
      break;
    case "network":
      detail = t("mon.netErrRate", { n: fmtNum(c.value) });
      break;
    case "http":
      detail = c.status === "ok" ? `${c.detail} · ${Math.round(c.value)} ms` : c.detail;
      break;
    case "ssl":
      detail = t("mon.sslDays", { n: Math.round(c.value) });
      break;
    case "backup":
      detail = c.value >= 1e5 ? t("mon.backupNever") : t("mon.backupAge", { h: Math.round(c.value) });
      break;
    case "failed-units":
      detail = c.detail;
      break;
  }
  return { title, detail };
}

export function CheckList({ checks }: { checks: Check[] }) {
  return (
    <div className="list-rows">
      {checks.map((c) => {
        const x = checkText(c);
        return (
          <div className="list-row" key={c.id}>
            <span className={`mon-check ${c.status}`}>{c.status === "ok" ? "✓" : c.status === "warn" ? "!" : c.status === "crit" ? "✕" : "?"}</span>
            <span className="grow" title={x.title}>
              {x.title}
            </span>
            <span className="muted mon-check-detail" title={x.detail}>
              {x.detail}
            </span>
          </div>
        );
      })}
    </div>
  );
}

export function uptimeText(sec: number) {
  return formatDuration(sec);
}

export function useNow(ms = 30000) {
  const [now, setNow] = useState(Date.now());
  useInterval(() => setNow(Date.now()), ms);
  return now;
}

/** "3 min ago" style relative time. */
export function ago(tsMs: number, now = Date.now()) {
  const s = Math.max(0, Math.round((now - tsMs) / 1000));
  if (s < 60) return t("mon.agoSec", { n: s });
  if (s < 3600) return t("mon.agoMin", { n: Math.floor(s / 60) });
  if (s < 86400) return t("mon.agoHour", { n: Math.floor(s / 3600) });
  return t("mon.agoDay", { n: Math.floor(s / 86400) });
}

export function useStable<T>(v: T, deps: unknown[]) {
  return useMemo(() => v, deps);
}
