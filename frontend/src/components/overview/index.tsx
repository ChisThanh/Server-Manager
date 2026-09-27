import { useEffect, useMemo, useRef, useState } from "react";
import { Activity, Bell, Boxes, Clock, Cpu, Gauge, HardDrive, HeartPulse, MemoryStick, Network, RefreshCw, Server as ServerIcon, Settings2 } from "lucide-react";
import { MonitorService } from "../../../bindings/server-manager/services/monitor";
import type { PanelProps } from "../../ui/types";
import { Page } from "../../ui/Page";
import { Bar, StateBadge } from "../../ui/Status";
import { TimeChart } from "../../ui/TimeChart";
import { useInterval } from "../../ui/hooks";
import { primeSample, useActiveAlerts, useSample } from "../../store/monitor";
import { useServer } from "../../ui/perm";
import { formatBytes } from "../../lib/format";
import { useT } from "../../i18n";
import { EnvBadge } from "../EnvBadge";
import { ServerDialog } from "../ServerDialog";
import { Timeline } from "../timeline/Timeline";
import { CheckList, HealthBadge, RangePicker, columns, fmtPct, fmtRate, useHistory, uptimeText, type RangeId, type Zoom } from "../monitoring/shared";
import { AlertRow } from "../pages/alertsShared";
import "../monitoring/monitoring.css";

export default function OverviewPanel({ connId, visible, connected, navigate }: PanelProps) {
  const t = useT();
  // Hidden: keep the last values so background refreshes don't re-render.
  const server = useServer(connId, visible);
  const sample = useSample(connId, visible);
  const lastAlerts = useRef<ReturnType<typeof useActiveAlerts.getState>["alerts"]>([]);
  const alerts = useActiveAlerts((s) => (visible ? (lastAlerts.current = s.alerts) : lastAlerts.current)).filter((a) => a.server === connId);
  const [range, setRange] = useState<RangeId>("1h");
  const [zoom, setZoom] = useState<Zoom>(null);
  const [editing, setEditing] = useState(false);
  const hist = useHistory(connId, range, zoom, visible);
  const cols = useMemo(
    () => ({ cpu: columns(hist.data, ["cpu", "memPct"]), net: columns(hist.data, ["netRx", "netTx"]), io: columns(hist.data, ["diskRead", "diskWrite"]) }),
    [hist.data],
  );

  useEffect(() => {
    if (visible) primeSample(connId);
  }, [visible, connId]);
  // Faster sampling while the overview is on screen.
  useEffect(() => {
    if (visible && connected) MonitorService.Live(connId);
  }, [visible, connected]);
  useInterval(() => visible && connected && MonitorService.Live(connId), visible ? 10000 : null);

  const snap = sample?.snapshot;
  const health = sample?.health;
  const disk = useMemo(() => {
    const ms = snap?.mounts ?? [];
    const root = ms.find((m) => m.mount === "/") ?? ms[0];
    const fullest = ms.reduce<(typeof ms)[number] | undefined>((a, m) => (!a || m.pct > a.pct ? m : a), undefined);
    return { root, fullest };
  }, [snap]);

  if (!server) return null;
  if (!snap) {
    return (
      <Page>
        <div className="ui-center">
          {sample?.error ? <div className="err">{sample.error}</div> : <span className="spinner lg" />}
          <div className="muted">{t("mon.collecting")}</div>
        </div>
      </Page>
    );
  }

  const watchList = server.watchServices ?? [];
  const watched = (snap.units ?? []).filter((u) => watchList.includes(u.name));
  const unhealthy = (snap.containers ?? []).filter((c) => c.health === "unhealthy" || c.state === "restarting");

  return (
    <Page className="mon-page">
      <div className="ov-head">
        <div className="ov-title">
          <h2>{server.name}</h2>
          <EnvBadge env={server.environment} />
          {health && <HealthBadge status={health.status} ok={health.ok} total={health.total} />}
          {!sample.reachable && <StateBadge tone="err">{t("mon.unreachable")}</StateBadge>}
        </div>
        <div className="sub muted">
          {server.osInfo || "—"} · {t("mon.uptimeFor", { d: uptimeText(snap.uptime) })} · {t("mon.cores", { n: snap.cpus })}
          {server.region ? ` · ${server.region}` : ""}
          {server.provider ? ` · ${server.provider}` : ""}
        </div>
        <div className="ov-actions">
          {!server.monitor && (
            <button className="btn sm" onClick={() => MonitorService.SetMonitor(connId, true)} title={t("mon.enableBgHint")}>
              <Activity size={13} /> {t("mon.enableBg")}
            </button>
          )}
          <button className="btn sm" onClick={() => setEditing(true)}>
            <Settings2 size={13} /> {t("mon.configure")}
          </button>
          <button className="icon-btn" title={t("common.refresh")} onClick={() => MonitorService.CheckNow(connId)}>
            <RefreshCw size={14} />
          </button>
        </div>
      </div>

      <div className="cards ov-cards">
        <div className="card">
          <div className="k">
            <Cpu size={13} /> CPU
          </div>
          <div className="v">{fmtPct(snap.cpu)}</div>
          <div className="s">
            iowait {fmtPct(snap.iowait)} · steal {fmtPct(snap.steal)}
          </div>
          <Bar pct={snap.cpu} />
        </div>
        <div className="card">
          <div className="k">
            <MemoryStick size={13} /> RAM
          </div>
          <div className="v">{fmtPct(snap.memPct)}</div>
          <div className="s">
            {formatBytes(snap.memUsed)} / {formatBytes(snap.memTotal)}
          </div>
          <Bar pct={snap.memPct} />
        </div>
        <div className="card">
          <div className="k">
            <HardDrive size={13} /> {t("mon.disk")}
          </div>
          <div className="v">{disk.fullest ? fmtPct(disk.fullest.pct) : "–"}</div>
          <div className="s" title={disk.fullest?.mount}>
            {disk.fullest ? `${formatBytes(disk.fullest.used)} / ${formatBytes(disk.fullest.total)} · ${disk.fullest.mount}` : ""}
          </div>
          {disk.fullest && <Bar pct={disk.fullest.pct} warn={80} crit={90} />}
        </div>
        <div className="card">
          <div className="k">
            <Network size={13} /> {t("mon.network")}
          </div>
          <div className="v" style={{ fontSize: 17 }}>
            ↓ {fmtRate(snap.netRx)}
          </div>
          <div className="s">↑ {fmtRate(snap.netTx)}</div>
        </div>
        <div className="card">
          <div className="k">
            <Gauge size={13} /> {t("mon.load")}
          </div>
          <div className="v">{snap.load[0].toFixed(2)}</div>
          <div className="s">
            5m {snap.load[1].toFixed(2)} · 15m {snap.load[2].toFixed(2)}
          </div>
          <Bar pct={snap.cpus ? (snap.load[0] / snap.cpus) * 100 : 0} />
        </div>
        <div className="card">
          <div className="k">
            <Clock size={13} /> {t("mon.processes")}
          </div>
          <div className="v">{snap.procs}</div>
          <div className="s">{t("mon.runningN", { n: snap.running })}</div>
        </div>
      </div>

      <div className="mon-toolbar">
        <span className="section-title" style={{ margin: 0 }}>
          {t("mon.charts")}
        </span>
        <div className="grow" />
        <RangePicker value={range} onChange={(r) => (setRange(r), setZoom(null))} zoom={zoom} onReset={() => setZoom(null)} />
      </div>
      <div className="chart-grid">
        <div className="chart-card">
          <div className="chart-title">
            <Cpu size={12} /> CPU / RAM
          </div>
          <TimeChart
            data={cols.cpu}
            series={[
              { label: "CPU", color: "#4f8cff", fill: true },
              { label: "RAM", color: "#34c77b" },
            ]}
            yMax={100}
            fmt={fmtPct}
            height={150}
            syncKey={`ov-${connId}`}
            xRange={hist.window}
            onZoom={(a, b) => setZoom([a, b])}
          />
        </div>
        <div className="chart-card">
          <div className="chart-title">
            <Network size={12} /> {t("mon.network")}
          </div>
          <TimeChart
            data={cols.net}
            series={[
              { label: "↓ RX", color: "#f5a524", fill: true },
              { label: "↑ TX", color: "#a970ff" },
            ]}
            fmt={fmtRate}
            height={150}
            syncKey={`ov-${connId}`}
            xRange={hist.window}
            onZoom={(a, b) => setZoom([a, b])}
          />
        </div>
        <div className="chart-card">
          <div className="chart-title">
            <HardDrive size={12} /> {t("mon.diskIO")}
          </div>
          <TimeChart
            data={cols.io}
            series={[
              { label: t("mon.read"), color: "#17c3b2", fill: true },
              { label: t("mon.write"), color: "#ff7ab6" },
            ]}
            fmt={fmtRate}
            height={150}
            syncKey={`ov-${connId}`}
            xRange={hist.window}
            onZoom={(a, b) => setZoom([a, b])}
          />
        </div>
      </div>

      <div className="dash-cols">
        <div className="panel-box">
          <div className="box-title">
            <HeartPulse size={14} /> {t("mon.health")}
            <div className="grow" />
            {health && <HealthBadge status={health.status} ok={health.ok} total={health.total} />}
          </div>
          {health ? <CheckList checks={health.checks ?? []} /> : null}
        </div>
        <div className="panel-box">
          <div className="box-title">
            <ServerIcon size={14} /> {t("mon.services")}
            <div className="grow" />
            <button className="link-btn" onClick={() => navigate("services")}>
              {t("mon.viewAll")}
            </button>
          </div>
          <div className="list-rows">
            {watched.map((u) => (
              <div className="list-row" key={u.name}>
                <StateBadge state={u.state === "active" ? "running" : u.state}>{u.state}</StateBadge>
                <span className="grow mono">{u.name}</span>
              </div>
            ))}
            {(snap.failed ?? []).map((u) => (
              <div className="list-row" key={u}>
                <StateBadge tone="err">failed</StateBadge>
                <span className="grow mono">{u}</span>
              </div>
            ))}
            {unhealthy.map((c) => (
              <div className="list-row clickable" key={c.name} onClick={() => navigate("docker")}>
                <StateBadge tone="err">
                  <Boxes size={11} /> {c.health || c.state}
                </StateBadge>
                <span className="grow mono">{c.name}</span>
              </div>
            ))}
            {watched.length === 0 && (snap.failed ?? []).length === 0 && unhealthy.length === 0 && (
              <div className="muted" style={{ padding: "6px 4px" }}>
                {watchList.length === 0 ? (
                  <>
                    {t("mon.noWatched")}{" "}
                    <button className="link-btn" onClick={() => setEditing(true)}>
                      {t("mon.chooseServices")}
                    </button>
                  </>
                ) : (
                  t("mon.allServicesOk")
                )}
              </div>
            )}
          </div>
        </div>
        <div className="panel-box">
          <div className="box-title">
            <Bell size={14} /> {t("mon.activeAlerts")}
            <div className="grow" />
            <span className="muted">{alerts.length}</span>
          </div>
          <div className="list-rows">
            {alerts.map((a) => (
              <AlertRow key={a.key} alert={a} compact />
            ))}
            {alerts.length === 0 && <div className="muted" style={{ padding: "6px 4px" }}>{t("mon.noAlerts")}</div>}
          </div>
        </div>
      </div>

      <div className="panel-box">
        <div className="box-title">
          <Activity size={14} /> {t("mon.recentEvents")}
        </div>
        <Timeline serverId={connId} limit={30} />
      </div>
      {editing && <ServerDialog server={server} initialTab="monitoring" onClose={() => setEditing(false)} />}
    </Page>
  );
}
