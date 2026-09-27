import { useMemo, useState } from "react";
import {
  Info,
  MoreHorizontal,
  Pause,
  Play,
  RefreshCw,
  RotateCcw,
  ScrollText,
  Skull,
  Square,
  TerminalSquare,
  Trash2,
  Repeat,
  ExternalLink,
  Box,
} from "lucide-react";
import type { TermRequest } from "../terminal/TerminalPanel";
import { useRemote } from "../../ui/hooks";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { Bar } from "../../ui/Status";
import { runJob } from "../../ui/jobs";
import { confirmDanger } from "../../ui/confirm";
import { Modal } from "../Overlays";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { toast, useUI, type MenuItem } from "../../store/ui";
import { useT } from "../../i18n";
import {
  DockerService,
  POLL_MS,
  StateCell,
  ago,
  cmp,
  confirmOpts,
  ds,
  fullDate,
  useFilter,
  useSort,
  type Container,
  type ContainerStats,
  type ViewProps,
} from "./common";
import { InspectModal } from "./Inspect";
import { Select } from "../../ui/Select";

type Filter = "all" | "running" | "stopped" | "unhealthy";
type SortKey = "name" | "image" | "state" | "cpu" | "mem" | "created";

interface Row extends Container {
  st?: ContainerStats;
}

export function ContainersView(p: ViewProps) {
  const t = useT();
  const showMenu = useUI((s) => s.showMenu);
  const list = useRemote(() => ds(p.connId, (pw) => DockerService.Containers(p.connId, pw)), [p.connId], {
    enabled: p.active,
    poll: p.auto ? POLL_MS : undefined,
  });
  const stats = useRemote(() => ds(p.connId, (pw) => DockerService.Stats(p.connId, pw)), [p.connId], {
    enabled: p.active,
    poll: p.auto ? POLL_MS : undefined,
  });
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [busy, setBusy] = useState<string | null>(null);
  const [logs, setLogs] = useState<string | null>(null);
  const [inspect, setInspect] = useState<string | null>(null);
  const { sort, th } = useSort<SortKey>("name");

  const rows: Row[] = useMemo(() => {
    const byId = new Map((stats.data ?? []).map((s) => [s.id, s]));
    return (list.data ?? []).map((c) => ({ ...c, st: c.state === "running" ? byId.get(c.id) : undefined }));
  }, [list.data, stats.data]);

  const counts = useMemo(() => {
    const c = { all: rows.length, running: 0, stopped: 0, unhealthy: 0, cpu: 0, mem: 0, memLimit: 0 };
    for (const r of rows) {
      if (r.state === "running") c.running++;
      else c.stopped++;
      if (r.health === "unhealthy") c.unhealthy++;
      if (r.st) {
        c.cpu += r.st.cpu;
        c.mem += r.st.memUsage;
        c.memLimit = Math.max(c.memLimit, r.st.memLimit);
      }
    }
    return c;
  }, [rows]);

  const textFiltered = useFilter(rows, q, (r) => [r.name, r.image, r.project, r.service, r.ports, r.status, r.id]);
  const visible = useMemo(() => {
    const f = textFiltered.filter((r) => {
      if (filter === "running") return r.state === "running";
      if (filter === "stopped") return r.state !== "running";
      if (filter === "unhealthy") return r.health === "unhealthy";
      return true;
    });
    const key = (r: Row): string | number => {
      switch (sort.key) {
        case "cpu":
          return r.st?.cpu ?? -1;
        case "mem":
          return r.st?.memUsage ?? -1;
        case "created":
          return r.created;
        default:
          return r[sort.key];
      }
    };
    return [...f].sort((a, b) => cmp(key(a), key(b)) * (sort.desc ? -1 : 1));
  }, [textFiltered, filter, sort]);

  const reload = () => {
    list.reload();
    stats.reload();
  };

  const act = async (c: Row, action: "start" | "stop" | "restart" | "pause" | "unpause" | "kill") => {
    if (action === "stop" || action === "kill") {
      const ok = await confirmDanger({
        serverId: p.connId,
        title: t(action === "stop" ? "docker.c.stopQ" : "docker.c.killQ", { name: c.name }),
        message: c.project ? t("docker.c.composeHint", { project: c.project }) : undefined,
        confirmText: t(action === "stop" ? "docker.c.stop" : "docker.c.kill"),
      });
      if (!ok) return;
    }
    setBusy(c.name);
    try {
      await ds(p.connId, (pw) => DockerService.ContainerAction(p.connId, c.name, action, pw));
      toast(t("docker.c.done", { name: c.name, action: t(`docker.c.${action}`) }), "success");
    } catch (e) {
      toast(`${c.name}: ${errMsg(e)}`, "error");
    } finally {
      setBusy(null);
      reload();
    }
  };

  const remove = async (c: Row) => {
    const opts = await confirmOpts({
      serverId: p.connId,
      title: t("docker.c.removeQ", { name: c.name }),
      message: c.state === "running" ? t("docker.c.removeRunning") : t("docker.c.removeMsg"),
      confirmText: t("docker.c.remove"),
      options: [
        { name: "force", label: t("docker.c.optForce"), value: c.state === "running" },
        { name: "volumes", label: t("docker.c.optVolumes") },
      ],
    });
    if (!opts) return;
    setBusy(c.name);
    try {
      await ds(p.connId, (pw) => DockerService.RemoveContainer(p.connId, c.name, opts.force, opts.volumes, pw));
      toast(t("docker.c.removed", { name: c.name }), "success");
    } catch (e) {
      toast(`${c.name}: ${errMsg(e)}`, "error");
    } finally {
      setBusy(null);
      reload();
    }
  };

  const recreate = async (c: Row) => {
    const opts = await confirmOpts({
      serverId: p.connId,
      title: t("docker.c.recreateQ", { name: c.name }),
      message: t("docker.c.recreateMsg", { project: c.project, service: c.service }),
      confirmText: t("docker.c.recreate"),
      options: [{ name: "pull", label: t("docker.c.optPull") }],
    });
    if (!opts) return;
    await runJob(t("docker.c.recreateTitle", { name: c.name }), () =>
      ds(p.connId, (pw) => DockerService.RecreateContainer(p.connId, c.name, opts.pull, pw)),
    );
    reload();
  };

  const shell = (c: Row) =>
    p.navigate("terminal", { cwd: "", nonce: Date.now(), exec: { kind: "docker", target: c.name, title: c.name } } satisfies TermRequest);
  const openLogsPanel = (c: Row) => p.navigate("logs", { source: "docker", container: c.name });

  const menu = (c: Row, x: number, y: number) => {
    const running = c.state === "running";
    const paused = c.state === "paused";
    const items: MenuItem[] = [];
    if (p.canEdit) {
      if (!running && !paused) items.push({ label: t("docker.c.start"), icon: <Play size={13} />, onClick: () => act(c, "start") });
      if (running) items.push({ label: t("docker.c.stop"), icon: <Square size={12} />, onClick: () => act(c, "stop") });
      items.push({ label: t("docker.c.restart"), icon: <RotateCcw size={13} />, onClick: () => act(c, "restart") });
      if (running) items.push({ label: t("docker.c.pause"), icon: <Pause size={13} />, onClick: () => act(c, "pause") });
      if (paused) items.push({ label: t("docker.c.unpause"), icon: <Play size={13} />, onClick: () => act(c, "unpause") });
      if (running || paused) items.push({ label: t("docker.c.kill"), icon: <Skull size={13} />, danger: true, onClick: () => act(c, "kill") });
      if (c.project && (c.configFiles ?? []).length > 0)
        items.push({ label: t("docker.c.recreate"), icon: <Repeat size={13} />, onClick: () => recreate(c) });
      items.push({ separator: true });
    }
    items.push({ label: t("docker.c.logs"), icon: <ScrollText size={13} />, onClick: () => setLogs(c.name) });
    items.push({ label: t("docker.c.openLogs"), icon: <ExternalLink size={13} />, onClick: () => openLogsPanel(c) });
    if (p.canShell) items.push({ label: t("docker.c.shell"), icon: <TerminalSquare size={13} />, disabled: !running, onClick: () => shell(c) });
    items.push({ label: t("docker.c.inspect"), icon: <Info size={13} />, onClick: () => setInspect(c.name) });
    items.push({ label: t("docker.c.copyName"), onClick: () => navigator.clipboard?.writeText(c.name) });
    if (p.canEdit) {
      items.push({ separator: true });
      items.push({ label: t("docker.c.remove"), icon: <Trash2 size={13} />, danger: true, onClick: () => remove(c) });
    }
    showMenu(x, y, items);
  };

  if (list.error && !list.data) return <ErrorBox error={list.error} onRetry={reload} />;
  if (!list.data) return <Loading label={t("common.loading")} />;

  return (
    <>
      <div className="cards docker-cards">
        <div className="card">
          <div className="k">{t("docker.c.running")}</div>
          <div className="v">
            {counts.running}
            <span className="docker-of"> / {counts.all}</span>
          </div>
          <div className="s">{t("docker.c.stoppedN", { n: counts.stopped })}</div>
        </div>
        <div className="card">
          <div className="k">{t("docker.c.unhealthy")}</div>
          <div className="v" style={{ color: counts.unhealthy ? "var(--err)" : undefined }}>
            {counts.unhealthy}
          </div>
          <div className="s">{t("docker.c.healthHint")}</div>
        </div>
        <div className="card">
          <div className="k">CPU</div>
          <div className="v">{stats.data ? `${counts.cpu.toFixed(1)}%` : "…"}</div>
          <div className="s">{t("docker.c.cpuHint")}</div>
        </div>
        <div className="card">
          <div className="k">RAM</div>
          <div className="v">{stats.data ? formatBytes(counts.mem) : "…"}</div>
          <div className="s">{counts.memLimit ? t("docker.c.memOf", { total: formatBytes(counts.memLimit) }) : " "}</div>
          {counts.memLimit > 0 && <Bar pct={(counts.mem / counts.memLimit) * 100} />}
        </div>
      </div>

      <div className="toolbar">
        <input className="input" placeholder={t("docker.c.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="segmented docker-seg">
          {(["all", "running", "stopped", "unhealthy"] as const).map((k) => (
            <button key={k} className={filter === k ? "on" : ""} onClick={() => setFilter(k)}>
              {t(`docker.c.f.${k}`)} <span className="docker-count">{counts[k]}</span>
            </button>
          ))}
        </div>
        <div className="grow" />
        {(list.error || stats.error) && <span className="badge err" title={list.error || stats.error}>{t("docker.refreshFailed")}</span>}
        <span className="muted">{t("docker.c.count", { n: visible.length })}</span>
        <button className="icon-btn" onClick={reload} title={t("common.refresh")} disabled={list.loading}>
          <RefreshCw size={14} className={list.loading ? "spin-icon" : ""} />
        </button>
      </div>

      {rows.length === 0 ? (
        <Empty icon={<Box size={28} />} title={t("docker.c.none")} text={t("docker.c.noneHint")} />
      ) : (
        <div className="table-wrap docker-table">
          <table className="grid">
            <thead>
              <tr>
                {th("name", t("docker.col.name"))}
                {th("state", t("docker.col.state"))}
                {th("image", t("docker.col.image"))}
                {th("cpu", "CPU", { num: true, defDesc: true })}
                {th("mem", "RAM", { num: true, defDesc: true })}
                <th className="num">{t("docker.col.net")}</th>
                <th>{t("docker.col.ports")}</th>
                {th("created", t("docker.col.created"), { defDesc: true })}
                <th />
              </tr>
            </thead>
            <tbody>
              {visible.map((c) => {
                const running = c.state === "running";
                return (
                  <tr
                    key={c.id}
                    onContextMenu={(e) => {
                      e.preventDefault();
                      menu(c, e.clientX, e.clientY);
                    }}
                    onDoubleClick={() => setInspect(c.name)}
                  >
                    <td className="docker-name-cell">
                      <div className="docker-name mono" title={c.id}>
                        {c.name}
                      </div>
                      {c.project && (
                        <div className="docker-sub">
                          <span className="chip">{c.project}</span>
                          {c.service && <span className="muted">{c.service}</span>}
                        </div>
                      )}
                    </td>
                    <td title={c.status}>
                      <StateCell state={c.state} health={c.health} />
                      <div className="docker-sub muted">{c.status}</div>
                    </td>
                    <td className="docker-image mono" title={c.image}>
                      {c.image}
                    </td>
                    <td className="num docker-usage">
                      {c.st ? (
                        <>
                          <span>{c.st.cpu.toFixed(1)}%</span>
                          <Bar pct={c.st.cpu} />
                        </>
                      ) : running ? (
                        <span className="muted">…</span>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td className="num docker-usage" title={c.st ? `${formatBytes(c.st.memUsage)} / ${formatBytes(c.st.memLimit)} (${c.st.memPct.toFixed(1)}%)` : ""}>
                      {c.st ? (
                        <>
                          <span>{formatBytes(c.st.memUsage)}</span>
                          <Bar pct={c.st.memPct} />
                        </>
                      ) : (
                        <span className="muted">{running ? "…" : "—"}</span>
                      )}
                    </td>
                    <td className="num muted" title={c.st ? `Block I/O: ${formatBytes(c.st.blockRead)} / ${formatBytes(c.st.blockWrite)} · PIDs ${c.st.pids}` : ""}>
                      {c.st ? `↓${formatBytes(c.st.netRx)} ↑${formatBytes(c.st.netTx)}` : "—"}
                    </td>
                    <td className="docker-ports mono" title={c.ports}>
                      {compactPorts(c.ports) || <span className="muted">—</span>}
                    </td>
                    <td className="muted" title={fullDate(c.created)}>
                      {ago(c.created)}
                    </td>
                    <td className="docker-actions">
                      {busy === c.name ? (
                        <span className="spinner" />
                      ) : (
                        <div className="docker-btns">
                          {p.canEdit &&
                            (running ? (
                              <button className="icon-btn" title={t("docker.c.stop")} onClick={() => act(c, "stop")}>
                                <Square size={12} />
                              </button>
                            ) : c.state === "paused" ? (
                              <button className="icon-btn" title={t("docker.c.unpause")} onClick={() => act(c, "unpause")}>
                                <Play size={13} />
                              </button>
                            ) : (
                              <button className="icon-btn" title={t("docker.c.start")} onClick={() => act(c, "start")}>
                                <Play size={13} />
                              </button>
                            ))}
                          {p.canEdit && (
                            <button className="icon-btn" title={t("docker.c.restart")} onClick={() => act(c, "restart")}>
                              <RotateCcw size={13} />
                            </button>
                          )}
                          <button className="icon-btn" title={t("docker.c.logs")} onClick={() => setLogs(c.name)}>
                            <ScrollText size={13} />
                          </button>
                          {p.canShell && (
                            <button className="icon-btn" title={t("docker.c.shell")} disabled={!running} onClick={() => shell(c)}>
                              <TerminalSquare size={13} />
                            </button>
                          )}
                          <button className="icon-btn" title={t("docker.c.inspect")} onClick={() => setInspect(c.name)}>
                            <Info size={13} />
                          </button>
                          <button
                            className="icon-btn"
                            title={t("docker.more")}
                            onClick={(e) => {
                              const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
                              menu(c, r.left, r.bottom);
                            }}
                          >
                            <MoreHorizontal size={14} />
                          </button>
                        </div>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {visible.length === 0 && <div className="docker-nomatch muted">{t("docker.noMatch")}</div>}
        </div>
      )}

      {logs && <LogsModal connId={p.connId} name={logs} onClose={() => setLogs(null)} onOpenPanel={() => p.navigate("logs", { source: "docker", container: logs })} />}
      {inspect && <InspectModal connId={p.connId} name={inspect} onClose={() => setInspect(null)} />}
    </>
  );
}

/** "0.0.0.0:8080->80/tcp, [::]:8080->80/tcp" → "8080→80/tcp". */
export function compactPorts(ports: string): string {
  if (!ports) return "";
  const out = new Set<string>();
  for (const part of ports.split(",")) {
    const s = part.trim();
    const m = s.match(/:(\d+(?:-\d+)?)->(\d+(?:-\d+)?\/\w+)$/);
    out.add(m ? `${m[1]}→${m[2]}` : s);
  }
  return [...out].join(", ");
}

export function LogsModal({ connId, name, onClose, onOpenPanel }: { connId: string; name: string; onClose: () => void; onOpenPanel: () => void }) {
  const t = useT();
  const [tail, setTail] = useState(500);
  const [since, setSince] = useState("");
  const [ts, setTs] = useState(false);
  const logs = useRemote(() => ds(connId, (pw) => DockerService.ContainerLogs(connId, name, tail, since, ts, pw)), [connId, name, tail, since, ts]);
  return (
    <Modal
      title={
        <>
          <ScrollText size={16} /> {t("docker.logs.title", { name })}
        </>
      }
      size="xwide"
      onClose={onClose}
      footer={
        <>
          <button className="btn left" onClick={onOpenPanel}>
            <ExternalLink size={13} /> {t("docker.c.openLogs")}
          </button>
          <button className="btn primary" onClick={onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      <div className="toolbar">
        <label className="muted">{t("docker.logs.tail")}</label>
        <Select value={tail} onChange={setTail} options={[100, 500, 1000, 5000]} />
        <label className="muted">{t("docker.logs.since")}</label>
        <Select
          value={since}
          onChange={setSince}
          options={[{ value: "", label: t("docker.logs.all") }, ...["5m", "15m", "1h", "6h", "24h"].map((s) => ({ value: s, label: t("docker.logs.last", { d: s }) }))]}
        />
        <label className="check">
          <input type="checkbox" checked={ts} onChange={(e) => setTs(e.target.checked)} /> {t("docker.logs.timestamps")}
        </label>
        <div className="grow" />
        <button className="icon-btn" onClick={logs.reload} title={t("common.refresh")} disabled={logs.loading}>
          <RefreshCw size={14} className={logs.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {logs.error ? (
        <div className="hint-box err">{logs.error}</div>
      ) : logs.data === undefined ? (
        <Loading />
      ) : (
        <div
          className="log-view docker-log"
          ref={(el) => {
            if (el) el.scrollTop = el.scrollHeight;
          }}
        >
          {logs.data || t("docker.logs.empty")}
        </div>
      )}
    </Modal>
  );
}

