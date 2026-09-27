import { memo, useCallback, useMemo, useRef, useState, type MouseEvent } from "react";
import { Info, Play, RefreshCw, RotateCcw, ScrollText, Square } from "lucide-react";
import { LogsService, type Unit, type UnitStats } from "../../../bindings/server-manager/services/logs";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { withSudo } from "../../store/sudo";
import { toast, useUI, type MenuItem } from "../../store/ui";
import { useRemote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { ErrorBox, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useT, type Key } from "../../i18n";
import { ServiceDetailModal } from "../logs/ServiceDetail";
import { confirmServiceAction, serviceActionLabel } from "../logs/serviceActions";
import "../logs/logs.css";
import { Select } from "../../ui/Select";

type Filter = "all" | "running" | "failed" | "enabled" | "disabled";
type Sort = "name" | "failed" | "cpu" | "mem";

interface Usage {
  cpu: number; // percent of one core, -1 = unknown
  mem: number; // bytes, -1 = unknown
}

const isEnabled = (s: string) => s === "enabled" || s === "enabled-runtime";
const isRunning = (u: Unit) => u.sub === "running" || (u.active === "active" && u.sub !== "exited");

export function Services({
  connId,
  visible,
  connected,
  navigate,
}: {
  connId: string;
  visible: boolean;
  connected: boolean;
  navigate?: (panel: string, arg?: unknown) => void;
}) {
  const t = useT();
  const canChange = useCan(connId, "services");
  const showMenu = useUI((s) => s.showMenu);
  const list = useRemote(() => LogsService.ServiceList(connId), [connId], { enabled: visible && connected });
  const init = list.data?.init;
  const units = list.data?.units ?? [];

  // Resource usage: CPU% from the CPU-time delta between two polls.
  const prev = useRef<UnitStats | null>(null);
  const [usage, setUsage] = useState<Record<string, Usage>>({});
  useRemote(
    async () => {
      const cur = await LogsService.ServiceStats(connId);
      const p = prev.current;
      const next: Record<string, Usage> = {};
      const before = new Map((p?.units ?? []).map((u) => [u.name, u]));
      const dt = p ? cur.sampleNs - p.sampleNs : 0;
      for (const u of cur.units ?? []) {
        const b = before.get(u.name);
        const cpu = b && dt > 0 && u.cpuNSec >= 0 && b.cpuNSec >= 0 && u.cpuNSec >= b.cpuNSec ? ((u.cpuNSec - b.cpuNSec) / dt) * 100 : -1;
        next[u.name] = { cpu, mem: u.memory };
      }
      prev.current = cur;
      setUsage(next);
      return cur;
    },
    [connId],
    { enabled: visible && connected && init === "systemd", poll: 5000 },
  );

  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [sort, setSort] = useState<Sort>("failed");
  const [busy, setBusy] = useState<string | null>(null);
  const [detail, setDetail] = useState<string | null>(null);

  const counts = useMemo(
    () => ({
      all: units.length,
      running: units.filter(isRunning).length,
      failed: units.filter((u) => u.active === "failed").length,
      enabled: units.filter((u) => isEnabled(u.unitFileState)).length,
    }),
    [units],
  );

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const out = units.filter((u) => {
      if (filter === "running" && !isRunning(u)) return false;
      if (filter === "failed" && u.active !== "failed") return false;
      if (filter === "enabled" && !isEnabled(u.unitFileState)) return false;
      if (filter === "disabled" && u.unitFileState !== "disabled") return false;
      return !needle || u.name.toLowerCase().includes(needle) || u.description.toLowerCase().includes(needle);
    });
    const rank = (u: Unit) => (u.active === "failed" ? 0 : isRunning(u) ? 1 : 2);
    out.sort((a, b) => {
      switch (sort) {
        case "failed":
          return rank(a) - rank(b) || a.name.localeCompare(b.name);
        case "cpu":
          return (usage[b.name]?.cpu ?? -1) - (usage[a.name]?.cpu ?? -1) || a.name.localeCompare(b.name);
        case "mem":
          return (usage[b.name]?.mem ?? -1) - (usage[a.name]?.mem ?? -1) || a.name.localeCompare(b.name);
        default:
          return a.name.localeCompare(b.name);
      }
    });
    return out;
  }, [units, q, filter, sort, usage]);

  const act = async (u: Unit, action: string) => {
    if (!(await confirmServiceAction(connId, u.name, action, u.description))) return;
    setBusy(u.name);
    try {
      await withSudo(connId, (pw) => LogsService.ServiceControl(connId, u.name, action, pw));
      toast(t("svc.ok", { name: u.name, action: serviceActionLabel(action) }), "success");
      await list.reload();
    } catch (e) {
      toast(`${u.name}: ${errMsg(e)}`, "error");
    } finally {
      setBusy(null);
    }
  };

  const openLogs = (u: Unit) => navigate?.("logs", { source: "unit", unit: u.name });

  const rowMenu = (e: MouseEvent, u: Unit) => {
    const systemd = init === "systemd";
    const items: MenuItem[] = [];
    if (systemd) items.push({ label: t("logs.svc.detail"), onClick: () => setDetail(u.name) }, { separator: true });
    if (canChange) {
      items.push(
        { label: t("svc.start"), onClick: () => act(u, "start") },
        { label: t("svc.stop"), onClick: () => act(u, "stop") },
        { label: t("svc.restart"), onClick: () => act(u, "restart") },
        { label: t("svc.reload"), onClick: () => act(u, "reload") },
      );
      if (init !== "sysv") {
        items.push({ separator: true }, { label: t("svc.enable"), onClick: () => act(u, "enable") }, { label: t("svc.disable"), onClick: () => act(u, "disable") });
      }
    }
    if (systemd && navigate) items.push({ separator: true }, { label: t("logs.svc.d.openLogs"), onClick: () => openLogs(u) });
    if (items.length) showMenu(e.clientX, e.clientY, items);
  };
  // One stable callback for every row, so a row re-renders only when its own
  // data changes (the CPU/RAM poll touches just the rows whose numbers moved).
  const latest = useRef({ act, openLogs, rowMenu, init });
  latest.current = { act, openLogs, rowMenu, init };
  const onRowEvent = useCallback((ev: RowEvent, u: Unit, e?: MouseEvent) => {
    const h = latest.current;
    if (ev === "detail") h.init === "systemd" && setDetail(u.name);
    else if (ev === "logs") h.openLogs(u);
    else if (ev === "menu") h.rowMenu(e!, u);
    else h.act(u, ev);
  }, []);

  if (list.error && !list.data) {
    return (
      <div className="sys-page">
        <ErrorBox error={list.error} onRetry={list.reload} />
      </div>
    );
  }
  if (!list.data) {
    return (
      <div className="sys-page">
        <Loading />
      </div>
    );
  }
  if (init === "none") {
    return (
      <div className="sys-page">
        <div className="hint-box">{t("logs.svc.init.none")}</div>
      </div>
    );
  }

  const systemd = init === "systemd";
  const fsLabel = (s: string) => {
    const k = `logs.svc.fs.${s}`;
    return s ? t(k as Key) === k ? s : t(k as Key) : "—";
  };
  const fmtCpu = (u: Unit) => {
    const v = usage[u.name]?.cpu;
    return v === undefined || v < 0 ? "" : `${v.toFixed(v < 10 ? 1 : 0)}%`;
  };
  const fmtMem = (u: Unit) => {
    const v = usage[u.name]?.mem;
    return v === undefined || v <= 0 ? "" : formatBytes(v);
  };
  const countBox = (f: Filter, label: Key, n: number, tone = "") => (
    <button className={`logs-svc-count ${tone} ${filter === f ? "on" : ""}`} onClick={() => setFilter(filter === f ? "all" : f)}>
      {t(label)} <b>{n}</b>
    </button>
  );
  const sortHead = (s: Sort, label: string, cls = "") => (
    <th className={`sortable ${cls}`} onClick={() => setSort(s)}>
      {label}
      {sort === s ? " ▾" : ""}
    </th>
  );

  return (
    <div className="sys-page">
      {init === "openrc" && <div className="hint-box" style={{ marginBottom: 12 }}>{t("logs.svc.init.openrc")}</div>}
      {init === "sysv" && <div className="hint-box" style={{ marginBottom: 12 }}>{t("logs.svc.init.sysv")}</div>}
      {!canChange && <div className="logs-hint" style={{ marginBottom: 8 }}>{t("logs.svc.readOnly")}</div>}

      <div className="logs-svc-counts">
        {countBox("all", "logs.svc.count.total", counts.all)}
        {countBox("running", "logs.svc.count.running", counts.running, "ok")}
        {countBox("failed", "logs.svc.count.failed", counts.failed, counts.failed ? "err" : "")}
        {init !== "sysv" && countBox("enabled", "logs.svc.count.enabled", counts.enabled)}
      </div>

      <div className="toolbar">
        <input className="input" placeholder={t("svc.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <Select<Filter>
          value={filter}
          onChange={setFilter}
          options={[
            { value: "all", label: t("svc.all") },
            { value: "running", label: t("svc.running") },
            { value: "failed", label: t("svc.failed") },
            ...(init !== "sysv"
              ? [
                  { value: "enabled" as const, label: t("logs.svc.filter.enabled") },
                  { value: "disabled" as const, label: t("logs.svc.filter.disabled") },
                ]
              : []),
          ]}
        />
        <Select<Sort>
          value={sort}
          onChange={setSort}
          title={t("logs.svc.sort")}
          options={[
            { value: "failed", label: t("logs.svc.sort.failed") },
            { value: "name", label: t("logs.svc.sort.name") },
            ...(systemd
              ? [
                  { value: "cpu" as const, label: t("logs.svc.sort.cpu") },
                  { value: "mem" as const, label: t("logs.svc.sort.mem") },
                ]
              : []),
          ]}
        />
        <div className="grow" />
        <span className="muted">{t("svc.count", { n: rows.length })}</span>
        <button className="icon-btn" onClick={list.reload} title={t("common.refresh")} disabled={list.loading}>
          <RefreshCw size={14} className={list.loading ? "spin-icon" : ""} />
        </button>
      </div>

      <div className="table-wrap">
        <table className="grid logs-svc-table">
          <thead>
            <tr>
              {sortHead("name", t("svc.name"))}
              {sortHead("failed", t("svc.state"))}
              {init !== "sysv" && <th>{t("logs.svc.startup")}</th>}
              {systemd && sortHead("cpu", t("logs.svc.cpu"), "num")}
              {systemd && sortHead("mem", t("logs.svc.mem"), "num")}
              <th>{t("svc.desc")}</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((u) => (
              <ServiceRow
                key={u.name}
                u={u}
                cpu={fmtCpu(u)}
                mem={fmtMem(u)}
                busy={busy === u.name}
                init={init}
                canChange={canChange}
                hasLogs={systemd && !!navigate}
                fsLabel={fsLabel(u.unitFileState)}
                onEvent={onRowEvent}
              />
            ))}
          </tbody>
        </table>
        {rows.length === 0 && <div className="ui-center muted" style={{ padding: 24 }}>{t("logs.svc.none")}</div>}
      </div>

      {detail && <ServiceDetailModal connId={connId} name={detail} onClose={() => setDetail(null)} onChanged={list.reload} navigate={navigate} />}
    </div>
  );
}

type RowEvent = "detail" | "logs" | "menu" | "start" | "stop" | "restart";

const ServiceRow = memo(function ServiceRow({
  u,
  cpu,
  mem,
  busy,
  init,
  canChange,
  hasLogs,
  fsLabel,
  onEvent,
}: {
  u: Unit;
  cpu: string;
  mem: string;
  busy: boolean;
  init?: string;
  canChange: boolean;
  hasLogs: boolean;
  fsLabel: string;
  onEvent: (ev: RowEvent, u: Unit, e?: MouseEvent) => void;
}) {
  const t = useT();
  const systemd = init === "systemd";
  return (
    <tr
      onClick={() => onEvent("detail", u)}
      onContextMenu={(e) => {
        e.preventDefault();
        onEvent("menu", u, e);
      }}
    >
      <td className="mono clip" title={u.name}>
        {u.name}
      </td>
      <td>
        <StateBadge state={u.active === "failed" ? "failed" : isRunning(u) ? "running" : u.sub || u.active} />
      </td>
      {init !== "sysv" && (
        <td>
          <span className={`logs-svc-fs ${u.unitFileState}`}>{fsLabel}</span>
        </td>
      )}
      {systemd && <td className="num">{cpu}</td>}
      {systemd && <td className="num">{mem}</td>}
      <td className="desc fill" title={u.description}>
        {u.description}
      </td>
      <td style={{ width: 110 }} onClick={(e) => e.stopPropagation()}>
        {busy ? (
          <span className="spinner" />
        ) : (
          <div className="logs-svc-actions">
            {canChange &&
              (isRunning(u) ? (
                <button className="icon-btn" title={t("svc.stop")} onClick={() => onEvent("stop", u)}>
                  <Square size={12} />
                </button>
              ) : (
                <button className="icon-btn" title={t("svc.start")} onClick={() => onEvent("start", u)}>
                  <Play size={13} />
                </button>
              ))}
            {canChange && (
              <button className="icon-btn" title={t("svc.restart")} onClick={() => onEvent("restart", u)}>
                <RotateCcw size={13} />
              </button>
            )}
            {hasLogs && (
              <button className="icon-btn" title={t("logs.svc.d.openLogs")} onClick={() => onEvent("logs", u)}>
                <ScrollText size={13} />
              </button>
            )}
            {systemd && (
              <button className="icon-btn" title={t("logs.svc.detail")} onClick={() => onEvent("detail", u)}>
                <Info size={13} />
              </button>
            )}
          </div>
        )}
      </td>
    </tr>
  );
});
