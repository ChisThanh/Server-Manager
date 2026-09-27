import { useMemo, useState } from "react";
import { Activity, Bell, LayoutGrid, List, Plus } from "lucide-react";
import { MonitorService } from "../../../bindings/server-manager/services/monitor";
import type { FleetItem } from "../../../bindings/server-manager/services/monitor";
import { Page, PageHeader, Empty } from "../../ui/Page";
import { Bar, StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { useApp } from "../../store/app";
import { useSamples } from "../../store/monitor";
import { toast } from "../../store/ui";
import { errMsg, type Server } from "../../lib/api";
import { useT, type Key } from "../../i18n";
import { Avatar } from "../Sidebar";
import { EnvBadge } from "../EnvBadge";
import { ServerDialog, blankServer } from "../ServerDialog";
import { HealthBadge, fmtPct, uptimeText } from "../monitoring/shared";
import "./pages.css";
import { Select } from "../../ui/Select";

type GroupBy = "none" | "group" | "environment" | "region" | "provider" | "status";

export default function FleetPage() {
  const t = useT();
  const servers = useApp((s) => s.servers);
  const conns = useApp((s) => s.conns);
  const samples = useSamples((s) => s.samples);
  const fleet = useRemote(() => MonitorService.Fleet(), [samples], { poll: 15000 });
  const [q, setQ] = useState("");
  const [env, setEnv] = useState("");
  const [tag, setTag] = useState("");
  const [status, setStatus] = useState("");
  const [groupBy, setGroupBy] = useState<GroupBy>("group");
  const [view, setView] = useState<"cards" | "table">("cards");
  const [adding, setAdding] = useState(false);

  const byId = useMemo(() => Object.fromEntries((fleet.data ?? []).map((f) => [f.server, f])), [fleet.data]);
  const tags = useMemo(() => Array.from(new Set(servers.flatMap((s) => s.tags ?? []))).sort(), [servers]);
  const statusOf = (s: Server, f?: FleetItem) => {
    if (!f?.collecting) return "offline";
    if (!f.reachable) return "down";
    return f.status || "unknown";
  };

  const list = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return servers.filter((s) => {
      if (env && s.environment !== env) return false;
      if (tag && !(s.tags ?? []).includes(tag)) return false;
      if (status && statusOf(s, byId[s.id]) !== status) return false;
      if (!needle) return true;
      return [s.name, s.host, s.user, s.group, s.region, s.provider, s.osInfo, ...(s.tags ?? [])].some((v) => (v ?? "").toLowerCase().includes(needle));
    });
  }, [servers, q, env, tag, status, byId]);

  const groups = useMemo(() => {
    const m = new Map<string, Server[]>();
    for (const s of list) {
      let k = "";
      if (groupBy === "group") k = s.group;
      else if (groupBy === "environment") k = s.environment ? t(`app.env.${s.environment}` as Key) : "";
      else if (groupBy === "region") k = s.region;
      else if (groupBy === "provider") k = s.provider;
      else if (groupBy === "status") k = t(`fleet.st.${statusOf(s, byId[s.id])}` as Key);
      m.set(k, [...(m.get(k) ?? []), s]);
    }
    return Array.from(m.entries()).sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : a.localeCompare(b)));
  }, [list, groupBy, byId, t]);

  const counts = useMemo(() => {
    const c = { total: servers.length, ok: 0, warn: 0, crit: 0, down: 0, offline: 0, alerts: 0 };
    for (const s of servers) {
      const st = statusOf(s, byId[s.id]);
      if (st === "ok") c.ok++;
      else if (st === "warn") c.warn++;
      else if (st === "crit") c.crit++;
      else if (st === "down") c.down++;
      else c.offline++;
      c.alerts += byId[s.id]?.alerts ?? 0;
    }
    return c;
  }, [servers, byId]);

  const open = (s: Server) => {
    const st = useApp.getState();
    if (st.open.includes(s.id) && conns[s.id]?.status !== "disconnected") st.setActive(s.id);
    else st.connect(s.id);
  };

  const toggleMonitor = async (s: Server, on: boolean) => {
    try {
      await MonitorService.SetMonitor(s.id, on);
      await useApp.getState().loadServers();
      fleet.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <Page className="page">
      <PageHeader
        icon={<LayoutGrid size={18} />}
        title={t("app.page.fleet")}
        sub={t("fleet.sub")}
        actions={
          <button className="btn primary sm" onClick={() => setAdding(true)}>
            <Plus size={13} /> {t("app.addServer")}
          </button>
        }
      />
      <div className="fleet-summary">
        <div className="card">
          <div className="k">{t("fleet.total")}</div>
          <div className="v">{counts.total}</div>
        </div>
        <div className="card">
          <div className="k" style={{ color: "var(--ok)" }}>{t("mon.healthy")}</div>
          <div className="v">{counts.ok}</div>
        </div>
        <div className="card">
          <div className="k" style={{ color: "var(--warn)" }}>{t("mon.warning")}</div>
          <div className="v">{counts.warn}</div>
        </div>
        <div className="card">
          <div className="k" style={{ color: "var(--err)" }}>{t("fleet.critDown")}</div>
          <div className="v">{counts.crit + counts.down}</div>
        </div>
        <div className="card">
          <div className="k">{t("fleet.notMonitored")}</div>
          <div className="v">{counts.offline}</div>
        </div>
        <div className="card">
          <div className="k">
            <Bell size={12} /> {t("fleet.alerts")}
          </div>
          <div className="v">{counts.alerts}</div>
        </div>
      </div>
      <div className="toolbar">
        <input className="input" placeholder={t("fleet.search")} value={q} onChange={(e) => setQ(e.target.value)} />
        <Select value={env} onChange={setEnv} options={[{ value: "", label: t("fleet.allEnv") }, ...["production", "staging", "development"].map((e) => ({ value: e, label: t(`app.env.${e}` as Key) }))]} />
        {tags.length > 0 && <Select value={tag} onChange={setTag} options={[{ value: "", label: t("fleet.allTags") }, ...tags.map((x) => ({ value: x, label: `#${x}` }))]} />}
        <Select value={status} onChange={setStatus} options={[{ value: "", label: t("fleet.allStatus") }, ...["ok", "warn", "crit", "down", "offline"].map((x) => ({ value: x, label: t(`fleet.st.${x}` as Key) }))]} />
        <Select<GroupBy>
          value={groupBy}
          onChange={setGroupBy}
          title={t("fleet.groupBy")}
          options={(["none", "group", "environment", "region", "provider", "status"] as GroupBy[]).map((g) => ({ value: g, label: `${t("fleet.groupBy")}: ${t(`fleet.gb.${g}` as Key)}` }))}
        />
        <div className="grow" />
        <div className="segmented" style={{ width: 80 }}>
          <button className={view === "cards" ? "on" : ""} onClick={() => setView("cards")} title={t("fleet.cards")}>
            <LayoutGrid size={13} />
          </button>
          <button className={view === "table" ? "on" : ""} onClick={() => setView("table")} title={t("fleet.table")}>
            <List size={13} />
          </button>
        </div>
      </div>

      {servers.length === 0 && <Empty title={t("sidebar.empty")} action={<button className="btn primary" onClick={() => setAdding(true)}>{t("app.addServer")}</button>} />}

      {groups.map(([g, items]) => (
        <div key={g || "_"}>
          {groupBy !== "none" && (
            <div className="fleet-group-title">
              {g || t("sidebar.other")} <span className="muted">({items.length})</span>
            </div>
          )}
          {view === "cards" ? (
            <div className="fleet-grid">
              {items.map((s) => (
                <FleetCard key={s.id} s={s} f={byId[s.id]} st={statusOf(s, byId[s.id])} onOpen={() => open(s)} onMonitor={(on) => toggleMonitor(s, on)} />
              ))}
            </div>
          ) : (
            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th>{t("mon.server")}</th>
                    <th>{t("fleet.status")}</th>
                    <th className="num">CPU</th>
                    <th className="num">RAM</th>
                    <th className="num">{t("mon.disk")}</th>
                    <th className="num">{t("mon.load")}</th>
                    <th>{t("fleet.alerts")}</th>
                    <th>OS</th>
                    <th>{t("fleet.region")}</th>
                    <th>Tags</th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((s) => {
                    const f = byId[s.id];
                    const st = statusOf(s, f);
                    const has = f?.collecting && f.reachable && f.lastSeen > 0;
                    return (
                      <tr key={s.id} onDoubleClick={() => open(s)} style={{ cursor: "pointer" }}>
                        <td>
                          <b>{s.name}</b> <EnvBadge env={s.environment} short />
                          <div className="muted mono" style={{ fontSize: 11 }}>
                            {s.user}@{s.host}
                          </div>
                        </td>
                        <td>
                          <FleetStatus st={st} f={f} />
                        </td>
                        <td className="num">{has ? fmtPct(f.cpu) : "–"}</td>
                        <td className="num">{has ? fmtPct(f.mem) : "–"}</td>
                        <td className="num">{has ? fmtPct(f.disk) : "–"}</td>
                        <td className="num">{has ? f.load.toFixed(2) : "–"}</td>
                        <td>{f?.alerts ? <StateBadge tone="err">{f.alerts}</StateBadge> : <span className="muted">0</span>}</td>
                        <td className="muted">{s.osInfo || "–"}</td>
                        <td className="muted">{[s.region, s.provider].filter(Boolean).join(" · ") || "–"}</td>
                        <td>
                          <div className="chips">
                            {(s.tags ?? []).map((x) => (
                              <span className="chip" key={x}>
                                #{x}
                              </span>
                            ))}
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
      ))}
      {adding && <ServerDialog server={blankServer()} onClose={() => setAdding(false)} />}
    </Page>
  );
}

function FleetStatus({ st, f }: { st: string; f?: FleetItem }) {
  const t = useT();
  if (st === "offline") return <StateBadge tone="muted">{t("fleet.st.offline")}</StateBadge>;
  if (st === "down") return <StateBadge tone="err" >{t("fleet.st.down")}</StateBadge>;
  return <HealthBadge status={st} ok={f?.ok} total={f?.total} />;
}

function FleetCard({ s, f, st, onOpen, onMonitor }: { s: Server; f?: FleetItem; st: string; onOpen: () => void; onMonitor: (on: boolean) => void }) {
  const t = useT();
  const has = f?.collecting && f.reachable && f.lastSeen > 0;
  return (
    <div className={`fleet-card ${st === "down" ? "crit" : st}`} onClick={onOpen}>
      <div className="fc-head">
        <Avatar server={s} />
        <div style={{ minWidth: 0, flex: 1 }}>
          <div className="fc-name">
            {s.name} <EnvBadge env={s.environment} short />
          </div>
          <div className="fc-sub mono">
            {s.user}@{s.host}
            {s.port !== 22 ? `:${s.port}` : ""}
          </div>
        </div>
        <FleetStatus st={st} f={f} />
      </div>
      {has ? (
        <div className="fc-metrics">
          <div className="m">
            <span>
              CPU <b>{fmtPct(f.cpu)}</b>
            </span>
            <Bar pct={f.cpu} />
          </div>
          <div className="m">
            <span>
              RAM <b>{fmtPct(f.mem)}</b>
            </span>
            <Bar pct={f.mem} />
          </div>
          <div className="m">
            <span>
              {t("mon.disk")} <b>{fmtPct(f.disk)}</b>
            </span>
            <Bar pct={f.disk} warn={80} crit={90} />
          </div>
        </div>
      ) : (
        <div className="muted" style={{ fontSize: 12 }}>
          {st === "down" ? f?.error : t("fleet.noData")}
          {!s.monitor && (
            <button
              className="link-btn"
              style={{ marginLeft: 6 }}
              onClick={(e) => {
                e.stopPropagation();
                onMonitor(true);
              }}
            >
              <Activity size={11} /> {t("mon.enableBg")}
            </button>
          )}
        </div>
      )}
      <div className="fc-foot">
        {f?.alerts ? (
          <StateBadge tone="err">
            <Bell size={10} /> {f.alerts}
          </StateBadge>
        ) : null}
        {has && <span>⏱ {uptimeText(f.uptime)}</span>}
        {s.osInfo && <span>· {s.osInfo}</span>}
        {s.region && <span>· {s.region}</span>}
        {s.provider && <span>· {s.provider}</span>}
        {(s.tags ?? []).map((x) => (
          <span className="chip" key={x}>
            #{x}
          </span>
        ))}
      </div>
    </div>
  );
}
