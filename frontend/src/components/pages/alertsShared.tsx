import { BellOff, Check } from "lucide-react";
import { MonitorService } from "../../../bindings/server-manager/services/monitor";
import type { Alert } from "../../../bindings/server-manager/services/monitor";
import { errMsg } from "../../lib/api";
import { toast } from "../../store/ui";
import { useActiveAlerts } from "../../store/monitor";
import { useApp } from "../../store/app";
import { t, useT, type Key } from "../../i18n";
import { StateBadge } from "../../ui/Status";
import { ago, useNow } from "../monitoring/shared";

const BUILTIN_EN: Record<string, string> = {
  "builtin-cpu-85": "CPU > 85% (5m)",
  "builtin-memory-90": "RAM > 90% (5m)",
  "builtin-disk-80": "Disk > 80%",
  "builtin-disk-90": "Disk > 90%",
  "builtin-load_per_core-2": "Load / core > 2 (5m)",
  "builtin-service_down-0": "Service down",
  "builtin-unreachable-0": "Server unreachable",
  "builtin-ssl_days-14": "SSL expires < 14 days",
  "builtin-http_down-0": "HTTP health check failed",
  "builtin-container_unhealthy-0": "Container unhealthy",
  "builtin-backup_age-26": "Backup older than 26h",
};

/** Localized name of a built-in rule (unless the user renamed it). */
export function ruleName(id: string, name: string) {
  return BUILTIN_EN[id] === name ? t(`mon.rule.${id}` as Key) : name;
}

export function metricLabel(m: string) {
  return t(`mon.metric.${m}` as Key);
}

export const BOOL_METRICS = new Set(["service_down", "unreachable", "http_down", "container_unhealthy"]);
export const METRIC_UNITS: Record<string, string> = {
  cpu: "%", memory: "%", swap: "%", disk: "%", inode: "%", iowait: "%", steal: "%", load: "", load_per_core: "",
  net_rx: "Mbps", net_tx: "Mbps", net_errors: "/s", processes: "", failed_units: "", ssl_days: t("mon.unitDays"), backup_age: "h",
};

export function fmtMetric(metric: string, v: number) {
  if (BOOL_METRICS.has(metric)) return "";
  const u = METRIC_UNITS[metric] ?? "";
  const n = Math.abs(v) >= 100 ? v.toFixed(0) : Math.abs(v) >= 10 ? v.toFixed(1) : v.toFixed(2);
  return `${n}${u && !u.startsWith(" ") && u.length <= 2 ? u : " " + u}`.trim();
}

export function alertSummary(a: Alert) {
  const v = fmtMetric(a.metric, a.value);
  const th = fmtMetric(a.metric, a.threshold);
  return v ? `${v} ${a.op} ${th}` : metricLabel(a.metric);
}

export function AlertRow({ alert: a, compact, showServer }: { alert: Alert; compact?: boolean; showServer?: boolean }) {
  const tr = useT();
  const now = useNow();
  const serverName = useApp((s) => s.servers.find((x) => x.id === a.server)?.name) ?? a.serverName;
  const ack = async () => {
    try {
      await MonitorService.Ack(a.key);
      toast(tr("mon.acked"), "success");
      useActiveAlerts.getState().reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  return (
    <div className={`list-row alert-row ${a.state}`}>
      <StateBadge tone={a.state === "pending" ? "muted" : a.severity === "crit" ? "err" : "warn"}>
        {a.state === "pending" ? tr("mon.pending") : a.severity === "crit" ? tr("mon.critical") : tr("mon.warning")}
      </StateBadge>
      <div className="grow" style={{ whiteSpace: "normal" }}>
        <div>
          {showServer && <b>{serverName} · </b>}
          {ruleName(a.rule, a.ruleName)}
          {a.target && <span className="mono muted"> [{a.target}]</span>}
        </div>
        {!compact && (
          <div className="muted" style={{ fontSize: 11.5 }}>
            {alertSummary(a)} · {tr("mon.since", { t: ago(a.since, now) })}
            {a.acked > 0 && ` · ${tr("mon.ackedBy", { who: a.ackedBy })}`}
          </div>
        )}
        {compact && <div className="muted" style={{ fontSize: 11.5 }}>{alertSummary(a)}</div>}
      </div>
      {a.state === "firing" && a.acked === 0 && (
        <button className="btn sm" onClick={ack} title={tr("mon.ackHint")}>
          <Check size={12} /> {tr("mon.ack")}
        </button>
      )}
      {a.acked > 0 && <BellOff size={13} className="muted" />}
    </div>
  );
}
