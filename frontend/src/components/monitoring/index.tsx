import { useEffect, useMemo, useState } from "react";
import { Cpu, Gauge, HardDrive, ListTree, MemoryStick, Network, RefreshCw } from "lucide-react";
import { MonitorService } from "../../../bindings/server-manager/services/monitor";
import type { PanelProps } from "../../ui/types";
import { Page } from "../../ui/Page";
import { Bar } from "../../ui/Status";
import { TimeChart, type ChartSeries } from "../../ui/TimeChart";
import { useInterval } from "../../ui/hooks";
import { primeSample, useSample } from "../../store/monitor";
import { formatBytes } from "../../lib/format";
import { useT } from "../../i18n";
import { RangePicker, columns, fmtMs, fmtNum, fmtPct, fmtRate, useHistory, type RangeId, type Zoom } from "./shared";
import "./monitoring.css";

type ChartDef = { key: string; title: string; icon: React.ReactNode; fields: string[]; series: ChartSeries[]; fmt: (v: number) => string; yMax?: number; scale?: number };

export default function MonitoringPanel({ connId, visible, connected, navigate }: PanelProps) {
  const t = useT();
  const sample = useSample(connId, visible);
  const [range, setRange] = useState<RangeId>("1h");
  const [zoom, setZoom] = useState<Zoom>(null);
  const hist = useHistory(connId, range, zoom, visible);

  useEffect(() => {
    if (visible) primeSample(connId);
    if (visible && connected) MonitorService.Live(connId);
  }, [visible, connected, connId]);
  useInterval(() => visible && connected && MonitorService.Live(connId), visible ? 10000 : null);

  const charts: ChartDef[] = useMemo(
    () => [
      {
        key: "cpu",
        title: "CPU",
        icon: <Cpu size={12} />,
        fields: ["cpu", "cpuMax", "iowait", "steal"],
        series: [
          { label: t("mon.avg"), color: "#4f8cff", fill: true },
          { label: t("mon.max"), color: "#4f8cff", dash: true, width: 1 },
          { label: "iowait", color: "#f5a524" },
          { label: "steal", color: "#f0525d" },
        ],
        fmt: fmtPct,
        yMax: 100,
      },
      {
        key: "load",
        title: t("mon.load"),
        icon: <Gauge size={12} />,
        fields: ["load1", "load5", "load15"],
        series: [
          { label: "1m", color: "#4f8cff" },
          { label: "5m", color: "#34c77b" },
          { label: "15m", color: "#a970ff" },
        ],
        fmt: fmtNum,
      },
      {
        key: "mem",
        title: t("mon.memory"),
        icon: <MemoryStick size={12} />,
        fields: ["memPct", "swapPct"],
        series: [
          { label: "RAM", color: "#34c77b", fill: true },
          { label: "Swap", color: "#f5a524" },
        ],
        fmt: fmtPct,
        yMax: 100,
      },
      {
        key: "memabs",
        title: t("mon.memUsedCache"),
        icon: <MemoryStick size={12} />,
        fields: ["memUsed", "memCache"],
        series: [
          { label: t("mon.used"), color: "#34c77b", fill: true },
          { label: "cache", color: "#8b8b94" },
        ],
        fmt: (v) => formatBytes(v),
      },
      {
        key: "disk",
        title: t("mon.diskUsage"),
        icon: <HardDrive size={12} />,
        fields: ["diskPct", "inodePct"],
        series: [
          { label: t("mon.space"), color: "#17c3b2", fill: true },
          { label: "inode", color: "#ff7ab6" },
        ],
        fmt: fmtPct,
        yMax: 100,
      },
      {
        key: "io",
        title: t("mon.diskIO"),
        icon: <HardDrive size={12} />,
        fields: ["diskRead", "diskWrite"],
        series: [
          { label: t("mon.read"), color: "#17c3b2", fill: true },
          { label: t("mon.write"), color: "#ff7ab6" },
        ],
        fmt: fmtRate,
      },
      {
        key: "iops",
        title: "IOPS",
        icon: <HardDrive size={12} />,
        fields: ["iops"],
        series: [{ label: "IOPS", color: "#a970ff", fill: true }],
        fmt: fmtNum,
      },
      {
        key: "lat",
        title: t("mon.diskLatency"),
        icon: <HardDrive size={12} />,
        fields: ["diskLat"],
        series: [{ label: t("mon.latency"), color: "#f5a524", fill: true }],
        fmt: fmtMs,
      },
      {
        key: "net",
        title: t("mon.bandwidth"),
        icon: <Network size={12} />,
        fields: ["netRx", "netTx"],
        series: [
          { label: "↓ RX", color: "#f5a524", fill: true },
          { label: "↑ TX", color: "#a970ff" },
        ],
        fmt: fmtRate,
      },
      {
        key: "neterr",
        title: t("mon.netErrors"),
        icon: <Network size={12} />,
        fields: ["netErr", "netDrop"],
        series: [
          { label: t("mon.errors"), color: "#f0525d" },
          { label: t("mon.drops"), color: "#f5a524" },
        ],
        fmt: fmtNum,
      },
      {
        key: "procs",
        title: t("mon.processes"),
        icon: <ListTree size={12} />,
        fields: ["procs"],
        series: [{ label: t("mon.processes"), color: "#8b8b94", fill: true }],
        fmt: (v) => v.toFixed(0),
      },
    ],
    [t],
  );

  const snap = sample?.snapshot;
  const chartData = useMemo(() => charts.map((c) => columns(hist.data, c.fields)), [hist.data, charts]);

  return (
    <Page className="mon-page">
      <div className="mon-toolbar">
        <h2 style={{ margin: 0, fontSize: 16 }}>{t("mon.title")}</h2>
        {hist.data && <span className="muted mon-res">{hist.data.res === 0 ? t("mon.resRaw") : t("mon.resAvg", { n: hist.data.res / 60 })}</span>}
        <div className="grow" />
        <RangePicker value={range} onChange={(r) => (setRange(r), setZoom(null))} zoom={zoom} onReset={() => setZoom(null)} />
        <button className="icon-btn" title={t("common.refresh")} onClick={hist.reload}>
          <RefreshCw size={14} />
        </button>
      </div>
      <div className="hint" style={{ marginBottom: 8 }}>
        {t("mon.zoomHint")}
      </div>

      <div className="chart-grid">
        {charts.map((c, ci) => (
          <div className="chart-card" key={c.key}>
            <div className="chart-title">
              {c.icon} {c.title}
            </div>
            <TimeChart
              data={chartData[ci]}
              series={c.series}
              fmt={c.fmt}
              yMax={c.yMax}
              height={150}
              syncKey={`mon-${connId}`}
              xRange={hist.window}
              onZoom={(a, b) => setZoom([a, b])}
            />
          </div>
        ))}
      </div>

      {snap && (
        <>
          <div className="section-title">{t("mon.realtime")}</div>
          <div className="dash-cols">
            <div className="panel-box">
              <div className="box-title">
                <Cpu size={14} /> {t("mon.perCore")}
                <div className="grow" />
                <span className="muted">{fmtPct(snap.cpu)}</span>
              </div>
              <div className="core-grid">
                {(snap.cores ?? []).map((v, i) => (
                  <div className="core" key={i} title={`CPU ${i}: ${fmtPct(v)}`}>
                    <div className="core-bar">
                      <div style={{ height: `${Math.max(2, v)}%` }} className={v >= 90 ? "crit" : v >= 75 ? "warn" : ""} />
                    </div>
                    <span>{i}</span>
                  </div>
                ))}
              </div>
              <div className="muted" style={{ fontSize: 11.5, marginTop: 6 }}>
                user {fmtPct(snap.user)} · system {fmtPct(snap.system)} · iowait {fmtPct(snap.iowait)} · steal {fmtPct(snap.steal)}
              </div>
            </div>
            <div className="panel-box">
              <div className="box-title">
                <MemoryStick size={14} /> {t("mon.memory")}
              </div>
              <div className="mem-rows">
                <MemRow label={t("mon.used")} v={snap.memUsed} total={snap.memTotal} />
                <MemRow label="cache" v={snap.memCache} total={snap.memTotal} />
                <MemRow label="buffers" v={snap.memBuffer} total={snap.memTotal} />
                <MemRow label={t("mon.available")} v={snap.memAvail} total={snap.memTotal} />
                {snap.swapTotal > 0 && <MemRow label="swap" v={snap.swapUsed} total={snap.swapTotal} />}
              </div>
            </div>
          </div>

          <div className="section-title">{t("mon.filesystems")}</div>
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>{t("mon.mount")}</th>
                  <th>{t("mon.fs")}</th>
                  <th className="num">{t("mon.used")}</th>
                  <th className="num">{t("mon.avail")}</th>
                  <th className="num">{t("mon.totalCol")}</th>
                  <th style={{ width: "22%" }}>{t("mon.space")}</th>
                  <th style={{ width: "16%" }}>inode</th>
                </tr>
              </thead>
              <tbody>
                {(snap.mounts ?? []).map((m) => (
                  <tr key={m.mount}>
                    <td className="mono">{m.mount}</td>
                    <td className="mono muted">{m.fs}</td>
                    <td className="num">{formatBytes(m.used)}</td>
                    <td className="num">{formatBytes(m.avail)}</td>
                    <td className="num">{formatBytes(m.total)}</td>
                    <td>
                      <div className="bar-cell">
                        <Bar pct={m.pct} warn={80} crit={90} />
                        <span>{fmtPct(m.pct)}</span>
                      </div>
                    </td>
                    <td>
                      {m.inodesTotal > 0 ? (
                        <div className="bar-cell">
                          <Bar pct={m.inodePct} warn={80} crit={90} />
                          <span>{fmtPct(m.inodePct)}</span>
                        </div>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <div className="dash-cols" style={{ marginTop: 16 }}>
            <div className="panel-box">
              <div className="box-title">
                <HardDrive size={14} /> {t("mon.devices")}
              </div>
              <table className="grid compact">
                <thead>
                  <tr>
                    <th>{t("mon.device")}</th>
                    <th className="num">{t("mon.read")}</th>
                    <th className="num">{t("mon.write")}</th>
                    <th className="num">IOPS</th>
                    <th className="num">{t("mon.latency")}</th>
                    <th className="num">util</th>
                  </tr>
                </thead>
                <tbody>
                  {(snap.disks ?? []).map((d) => (
                    <tr key={d.device}>
                      <td className="mono">{d.device}</td>
                      <td className="num">{fmtRate(d.readBps)}</td>
                      <td className="num">{fmtRate(d.writeBps)}</td>
                      <td className="num">{fmtNum(d.readIops + d.writeIops)}</td>
                      <td className="num">{fmtMs(d.latencyMs)}</td>
                      <td className="num">{fmtPct(d.util)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="panel-box">
              <div className="box-title">
                <Network size={14} /> {t("mon.interfaces")}
              </div>
              <table className="grid compact">
                <thead>
                  <tr>
                    <th>{t("mon.iface")}</th>
                    <th className="num">↓</th>
                    <th className="num">↑</th>
                    <th className="num">{t("mon.packets")}</th>
                    <th className="num">{t("mon.errors")}</th>
                    <th className="num">{t("mon.drops")}</th>
                  </tr>
                </thead>
                <tbody>
                  {(snap.nets ?? []).map((n) => (
                    <tr key={n.iface}>
                      <td className="mono">{n.iface}</td>
                      <td className="num">{fmtRate(n.rxBps)}</td>
                      <td className="num">{fmtRate(n.txBps)}</td>
                      <td className="num">{fmtNum(n.rxPps + n.txPps)}/s</td>
                      <td className={`num ${n.errors > 0 ? "err" : ""}`}>{fmtNum(n.errors)}/s</td>
                      <td className={`num ${n.drops > 0 ? "warn-text" : ""}`}>{fmtNum(n.drops)}/s</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>

          <div className="dash-cols">
            <ProcTable title={t("mon.topCpu")} procs={snap.topCpu ?? []} onMore={() => navigate("processes")} />
            <ProcTable title={t("mon.topMem")} procs={snap.topMem ?? []} onMore={() => navigate("processes")} />
          </div>
        </>
      )}
    </Page>
  );
}

function MemRow({ label, v, total }: { label: string; v: number; total: number }) {
  const pct = total ? (v / total) * 100 : 0;
  return (
    <div className="mem-row">
      <span className="muted">{label}</span>
      <Bar pct={pct} warn={101} crit={101} />
      <span className="num">{formatBytes(v)}</span>
    </div>
  );
}

function ProcTable({ title, procs, onMore }: { title: string; procs: { pid: number; user: string; cpu: number; mem: number; rss: number; command: string }[]; onMore: () => void }) {
  const t = useT();
  return (
    <div className="panel-box">
      <div className="box-title">
        <ListTree size={14} /> {title}
        <div className="grow" />
        <button className="link-btn" onClick={onMore}>
          {t("mon.viewAll")}
        </button>
      </div>
      <table className="grid compact">
        <thead>
          <tr>
            <th className="num">PID</th>
            <th>{t("proc.user")}</th>
            <th className="num">CPU</th>
            <th className="num">RAM</th>
            <th>{t("proc.cmd")}</th>
          </tr>
        </thead>
        <tbody>
          {procs.map((p) => (
            <tr key={p.pid}>
              <td className="num">{p.pid}</td>
              <td>{p.user || "–"}</td>
              <td className="num">{fmtPct(p.cpu)}</td>
              <td className="num">{formatBytes(p.rss)}</td>
              <td className="cmd" title={p.command}>
                {p.command}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
