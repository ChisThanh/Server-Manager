import { useEffect, useMemo, useState } from "react";
import { Bell, BellRing, Copy, History, Pencil, Plus, RotateCcw, Send, Trash2 } from "lucide-react";
import { MonitorService } from "../../../bindings/server-manager/services/monitor";
import type { Channel, Rule } from "../../../bindings/server-manager/services/monitor";
import { Page, PageHeader, Empty, Loading, ErrorBox } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { Modal } from "../Overlays";
import { useActiveAlerts } from "../../store/monitor";
import { useApp } from "../../store/app";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { useT, type Key } from "../../i18n";
import { AlertRow, BOOL_METRICS, METRIC_UNITS, alertSummary, metricLabel, ruleName } from "./alertsShared";
import { fmtTime } from "../timeline/Timeline";
import "../monitoring/monitoring.css";
import "./pages.css";
import { Select } from "../../ui/Select";

type Tab = "active" | "history" | "rules" | "channels";

export default function AlertsPage() {
  const t = useT();
  const [tab, setTab] = useState<Tab>("active");
  const active = useActiveAlerts((s) => s.alerts);
  const firing = active.filter((a) => a.state === "firing");
  return (
    <Page className="page">
      <PageHeader icon={<Bell size={18} />} title={t("app.page.alerts")} sub={t("mon.alertsSub")} />
      <SubTabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          { id: "active", label: t("mon.tabActive"), icon: <BellRing size={14} />, badge: firing.length },
          { id: "history", label: t("mon.tabHistory"), icon: <History size={14} /> },
          { id: "rules", label: t("mon.tabRules") },
          { id: "channels", label: t("mon.tabChannels"), icon: <Send size={14} /> },
        ]}
      />
      {tab === "active" && <ActiveTab />}
      {tab === "history" && <HistoryTab />}
      {tab === "rules" && <RulesTab />}
      {tab === "channels" && <ChannelsTab />}
    </Page>
  );
}

function ActiveTab() {
  const t = useT();
  const { alerts, loaded } = useActiveAlerts();
  useEffect(() => {
    useActiveAlerts.getState().reload();
  }, []);
  if (!loaded) return <Loading />;
  if (alerts.length === 0) return <Empty icon={<Bell size={28} />} title={t("mon.noAlerts")} text={t("mon.noAlertsHint")} />;
  const firing = alerts.filter((a) => a.state === "firing");
  const pending = alerts.filter((a) => a.state === "pending");
  return (
    <>
      {firing.length > 0 && (
        <div className="panel-box" style={{ marginBottom: 12 }}>
          <div className="box-title">{t("mon.firing", { n: firing.length })}</div>
          <div className="list-rows">
            {firing.map((a) => (
              <AlertRow key={a.key} alert={a} showServer />
            ))}
          </div>
        </div>
      )}
      {pending.length > 0 && (
        <div className="panel-box">
          <div className="box-title">{t("mon.pendingN", { n: pending.length })}</div>
          <div className="hint" style={{ marginBottom: 6 }}>
            {t("mon.pendingHint")}
          </div>
          <div className="list-rows">
            {pending.map((a) => (
              <AlertRow key={a.key} alert={a} showServer />
            ))}
          </div>
        </div>
      )}
    </>
  );
}

function HistoryTab() {
  const t = useT();
  const servers = useApp((s) => s.servers);
  const [server, setServer] = useState("");
  const r = useRemote(() => MonitorService.AlertHistory(server, 300), [server]);
  return (
    <>
      <div className="toolbar">
        <Select value={server} onChange={setServer} options={[{ value: "", label: t("mon.allServers") }, ...servers.map((s) => ({ value: s.id, label: s.name, hint: s.host }))]} />
      </div>
      {r.error && <ErrorBox error={r.error} onRetry={r.reload} />}
      {!r.data && !r.error && <Loading />}
      {r.data && r.data.length === 0 && <Empty title={t("mon.noHistory")} />}
      {r.data && r.data.length > 0 && (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("mon.severity")}</th>
                <th>{t("mon.server")}</th>
                <th>{t("mon.rule")}</th>
                <th>{t("mon.target")}</th>
                <th>{t("mon.firedAt")}</th>
                <th>{t("mon.resolvedAt")}</th>
                <th>{t("mon.duration")}</th>
                <th>{t("mon.ackCol")}</th>
              </tr>
            </thead>
            <tbody>
              {r.data.map((a) => (
                <tr key={a.key + a.resolved}>
                  <td>
                    <StateBadge tone={a.severity === "crit" ? "err" : "warn"}>{a.severity === "crit" ? t("mon.critical") : t("mon.warning")}</StateBadge>
                  </td>
                  <td>{servers.find((s) => s.id === a.server)?.name ?? a.serverName}</td>
                  <td>{ruleName(a.rule, a.ruleName)}</td>
                  <td className="mono muted">{a.target || "–"}</td>
                  <td>{a.fired ? fmtTime(a.fired) : "–"}</td>
                  <td>{fmtTime(a.resolved)}</td>
                  <td>{a.fired ? dur(a.resolved - a.fired) : "–"}</td>
                  <td className="muted">{a.ackedBy || "–"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function dur(ms: number) {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

// ---------------- rules ----------------

const METRICS = ["cpu", "memory", "swap", "disk", "inode", "load", "load_per_core", "iowait", "steal", "net_rx", "net_tx", "net_errors", "processes", "failed_units", "service_down", "unreachable", "http_down", "ssl_days", "container_unhealthy", "backup_age"];

function blankRule(): NRule {
  return { id: "", name: "", enabled: true, metric: "cpu", op: ">", threshold: 90, forSec: 300, severity: "warn", servers: [], environments: [], tags: [], groups: [], channels: [], repeatMin: 240, notifyResolved: true, builtin: false };
}

type NRule = Rule & { servers: string[]; environments: string[]; tags: string[]; groups: string[]; channels: string[] };

/** Go slices may arrive as null. */
function normRule(r: Rule): NRule {
  return { ...r, servers: r.servers ?? [], environments: r.environments ?? [], tags: r.tags ?? [], groups: r.groups ?? [], channels: r.channels ?? [] };
}

function RulesTab() {
  const t = useT();
  const r = useRemote(async () => ((await MonitorService.Rules()) ?? []).map(normRule), []);
  const ch = useRemote(() => MonitorService.Channels(), []);
  const [editing, setEditing] = useState<NRule | null>(null);
  const toggle = async (rule: NRule) => {
    try {
      await MonitorService.SaveRule({ ...rule, enabled: !rule.enabled });
      r.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  const remove = async (rule: NRule) => {
    if (!(await confirmDialog(t("mon.deleteRuleQ"), rule.name, t("common.delete"), true))) return;
    try {
      await MonitorService.DeleteRule(rule.id);
      r.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  const restore = async () => {
    await MonitorService.RestoreDefaultRules();
    r.reload();
    toast(t("mon.defaultsRestored"), "success");
  };
  const scope = (rule: NRule) => {
    const parts = [...rule.environments.map((e) => t(`app.env.${e}` as Key)), ...rule.groups, ...rule.tags.map((x) => "#" + x)];
    if (rule.servers.length) parts.push(t("mon.nServers", { n: rule.servers.length }));
    return parts.length ? parts.join(", ") : t("mon.allMonitored");
  };
  return (
    <>
      <div className="toolbar">
        <span className="hint">{t("mon.rulesHint")}</span>
        <div className="grow" />
        <button className="btn sm" onClick={restore}>
          <RotateCcw size={13} /> {t("mon.restoreDefaults")}
        </button>
        <button className="btn primary sm" onClick={() => setEditing(blankRule())}>
          <Plus size={13} /> {t("mon.newRule")}
        </button>
      </div>
      {r.error && <ErrorBox error={r.error} onRetry={r.reload} />}
      {r.data && (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th style={{ width: 40 }} />
                <th>{t("mon.rule")}</th>
                <th>{t("mon.condition")}</th>
                <th>{t("mon.severity")}</th>
                <th>{t("mon.scope")}</th>
                <th>{t("mon.tabChannels")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {r.data.map((rule) => (
                <tr key={rule.id} style={{ opacity: rule.enabled ? 1 : 0.55 }}>
                  <td>
                    <input type="checkbox" checked={rule.enabled} onChange={() => toggle(rule)} title={t("mon.enabled")} />
                  </td>
                  <td>{ruleName(rule.id, rule.name)}</td>
                  <td className="muted">
                    {conditionText(rule)}
                    {rule.forSec > 0 && ` · ${t("mon.forDur", { d: fmtFor(rule.forSec) })}`}
                  </td>
                  <td>
                    <StateBadge tone={rule.severity === "crit" ? "err" : "warn"}>{rule.severity === "crit" ? t("mon.critical") : t("mon.warning")}</StateBadge>
                  </td>
                  <td className="muted">{scope(rule)}</td>
                  <td className="muted">
                    {rule.channels.length ? rule.channels.map((id) => ch.data?.find((c) => c.id === id)?.name ?? "?").join(", ") : t("mon.defaultChannels")}
                  </td>
                  <td style={{ width: 80 }}>
                    <div style={{ display: "flex" }}>
                      <button className="icon-btn" title={t("common.edit")} onClick={() => setEditing(rule)}>
                        <Pencil size={13} />
                      </button>
                      <button className="icon-btn" title={t("mon.duplicate")} onClick={() => setEditing({ ...rule, id: "", builtin: false, name: rule.name + " (2)" })}>
                        <Copy size={13} />
                      </button>
                      <button className="icon-btn" title={t("common.delete")} onClick={() => remove(rule)}>
                        <Trash2 size={13} />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {editing && <RuleDialog rule={editing} channels={ch.data ?? []} onClose={() => setEditing(null)} onSaved={() => (setEditing(null), r.reload())} />}
    </>
  );
}

function conditionText(r: Rule) {
  if (BOOL_METRICS.has(r.metric)) return metricLabel(r.metric);
  const u = METRIC_UNITS[r.metric] ?? "";
  return `${metricLabel(r.metric)} ${r.op} ${r.threshold}${u.length <= 2 ? u : " " + u}`;
}

function fmtFor(s: number) {
  if (s % 3600 === 0) return `${s / 3600}h`;
  if (s % 60 === 0) return `${s / 60}m`;
  return `${s}s`;
}

function MultiPick({ options, value, onChange, placeholder }: { options: { id: string; label: string }[]; value: string[]; onChange: (v: string[]) => void; placeholder: string }) {
  return (
    <div className="multi-pick">
      <div className="chips">
        {value.map((v) => (
          <span className="chip" key={v}>
            {options.find((o) => o.id === v)?.label ?? v}
            <button onClick={() => onChange(value.filter((x) => x !== v))}>×</button>
          </span>
        ))}
      </div>
      <Select
        value=""
        placeholder={placeholder}
        onChange={(v) => v && !value.includes(v) && onChange([...value, v])}
        options={options.filter((o) => !value.includes(o.id)).map((o) => ({ value: o.id, label: o.label }))}
      />
    </div>
  );
}

function RuleDialog({ rule, channels, onClose, onSaved }: { rule: NRule; channels: Channel[]; onClose: () => void; onSaved: () => void }) {
  const t = useT();
  const [r, setR] = useState<NRule>(normRule(rule));
  const servers = useApp((s) => s.servers);
  const groups = useMemo(() => Array.from(new Set(servers.map((s) => s.group).filter(Boolean))), [servers]);
  const tags = useMemo(() => Array.from(new Set(servers.flatMap((s) => s.tags ?? []))), [servers]);
  const set = <K extends keyof NRule>(k: K, v: NRule[K]) => setR((x) => ({ ...x, [k]: v }));
  const isBool = BOOL_METRICS.has(r.metric);
  const save = async () => {
    try {
      await MonitorService.SaveRule(isBool ? { ...r, op: ">", threshold: 0 } : r);
      toast(t("mon.ruleSaved"), "success");
      onSaved();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  return (
    <Modal
      title={r.id ? t("mon.editRule") : t("mon.newRule")}
      size="wide"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save}>
            {t("common.save")}
          </button>
        </>
      }
    >
      <div className="field">
        <label>{t("mon.ruleName")}</label>
        <input className="input" value={r.name} onChange={(e) => set("name", e.target.value)} placeholder={metricLabel(r.metric)} autoFocus />
      </div>
      <div className="row">
        <div className="field" style={{ flex: 2 }}>
          <label>{t("mon.metric")}</label>
          <Select value={r.metric} onChange={(v) => set("metric", v)} options={METRICS.map((m) => ({ value: m, label: metricLabel(m), hint: METRIC_UNITS[m] || undefined }))} />
        </div>
        {!isBool && (
          <>
            <div className="field" style={{ flex: 0.6 }}>
              <label>{t("mon.op")}</label>
              <Select value={r.op} onChange={(v) => set("op", v)} options={[">", "<"]} />
            </div>
            <div className="field" style={{ flex: 1 }}>
              <label>{t("mon.threshold")}</label>
              <input className="input" type="number" value={r.threshold} onChange={(e) => set("threshold", Number(e.target.value))} />
            </div>
          </>
        )}
      </div>
      <div className="row">
        <div className="field">
          <label>{t("mon.forLabel")}</label>
          <Select value={r.forSec} onChange={(v) => set("forSec", v)} options={[0, 60, 120, 300, 600, 900, 1800, 3600].map((s) => ({ value: s, label: s === 0 ? t("mon.immediately") : fmtFor(s) }))} />
        </div>
        <div className="field">
          <label>{t("mon.severity")}</label>
          <div className="segmented">
            <button type="button" className={r.severity === "warn" ? "on" : ""} onClick={() => set("severity", "warn")}>
              {t("mon.warning")}
            </button>
            <button type="button" className={r.severity === "crit" ? "on" : ""} onClick={() => set("severity", "crit")}>
              {t("mon.critical")}
            </button>
          </div>
        </div>
        <div className="field">
          <label>{t("mon.repeat")}</label>
          <Select value={r.repeatMin} onChange={(v) => set("repeatMin", v)} options={[0, 30, 60, 240, 720, 1440].map((m) => ({ value: m, label: m === 0 ? t("mon.never") : fmtFor(m * 60) }))} />
        </div>
      </div>
      <div className="section-title">{t("mon.scope")}</div>
      <div className="hint" style={{ marginBottom: 6 }}>
        {t("mon.scopeHint")}
      </div>
      <div className="form-grid">
        <div className="field">
          <label>{t("mon.environments")}</label>
          <MultiPick
            options={["production", "staging", "development"].map((e) => ({ id: e, label: t(`app.env.${e}` as Key) }))}
            value={r.environments}
            onChange={(v) => set("environments", v)}
            placeholder={t("mon.add")}
          />
        </div>
        <div className="field">
          <label>{t("mon.groups")}</label>
          <MultiPick options={groups.map((g) => ({ id: g, label: g }))} value={r.groups} onChange={(v) => set("groups", v)} placeholder={t("mon.add")} />
        </div>
        <div className="field">
          <label>Tags</label>
          <MultiPick options={tags.map((g) => ({ id: g, label: "#" + g }))} value={r.tags} onChange={(v) => set("tags", v)} placeholder={t("mon.add")} />
        </div>
        <div className="field">
          <label>{t("mon.specificServers")}</label>
          <MultiPick options={servers.map((s) => ({ id: s.id, label: s.name }))} value={r.servers} onChange={(v) => set("servers", v)} placeholder={t("mon.add")} />
        </div>
      </div>
      <div className="section-title">{t("mon.notify")}</div>
      <div className="field">
        <label>{t("mon.tabChannels")}</label>
        <MultiPick options={channels.map((c) => ({ id: c.id, label: `${c.name} (${c.type})` }))} value={r.channels} onChange={(v) => set("channels", v)} placeholder={t("mon.add")} />
        <div className="hint">{t("mon.channelsHint")}</div>
      </div>
      <label className="check">
        <input type="checkbox" checked={r.notifyResolved} onChange={(e) => set("notifyResolved", e.target.checked)} /> {t("mon.notifyResolved")}
      </label>
      <label className="check" style={{ marginLeft: 16 }}>
        <input type="checkbox" checked={r.enabled} onChange={(e) => set("enabled", e.target.checked)} /> {t("mon.enabled")}
      </label>
    </Modal>
  );
}

// ---------------- channels ----------------

const CHANNEL_TYPES = ["telegram", "slack", "discord", "email", "webhook", "pagerduty", "desktop"] as const;

function ChannelsTab() {
  const t = useT();
  const r = useRemote(() => MonitorService.Channels(), []);
  const [editing, setEditing] = useState<Channel | null>(null);
  const remove = async (c: Channel) => {
    if (!(await confirmDialog(t("mon.deleteChannelQ"), c.name, t("common.delete"), true))) return;
    await MonitorService.DeleteChannel(c.id).catch((e) => toast(errMsg(e), "error"));
    r.reload();
  };
  const test = async (c: Channel) => {
    try {
      await MonitorService.TestChannel(c, "");
      toast(t("mon.testSent"), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  return (
    <>
      <div className="toolbar">
        <span className="hint">{t("mon.channelsIntro")}</span>
        <div className="grow" />
        <button className="btn primary sm" onClick={() => setEditing({ id: "", name: "", type: "telegram", enabled: true, config: {}, hasSecret: false, default: true })}>
          <Plus size={13} /> {t("mon.newChannel")}
        </button>
      </div>
      {r.data && r.data.length === 0 && <Empty icon={<Send size={28} />} title={t("mon.noChannels")} text={t("mon.noChannelsHint")} />}
      {r.data && r.data.length > 0 && (
        <div className="list-rows panel-box">
          {r.data.map((c) => (
            <div className="list-row" key={c.id}>
              <StateBadge tone={c.enabled ? "ok" : "muted"}>{c.type}</StateBadge>
              <span className="grow">
                {c.name}
                {c.default && <span className="muted"> · {t("mon.defaultChannel")}</span>}
              </span>
              <button className="btn sm" onClick={() => test(c)}>
                <Send size={12} /> {t("mon.test")}
              </button>
              <button className="icon-btn" title={t("common.edit")} onClick={() => setEditing(c)}>
                <Pencil size={13} />
              </button>
              <button className="icon-btn" title={t("common.delete")} onClick={() => remove(c)}>
                <Trash2 size={13} />
              </button>
            </div>
          ))}
        </div>
      )}
      {editing && <ChannelDialog ch={editing} onClose={() => setEditing(null)} onSaved={() => (setEditing(null), r.reload())} />}
    </>
  );
}

function ChannelDialog({ ch, onClose, onSaved }: { ch: Channel; onClose: () => void; onSaved: () => void }) {
  const t = useT();
  const [c, setC] = useState<Channel>({ ...ch, config: { ...(ch.config ?? {}) } });
  const [secret, setSecret] = useState("");
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const cfg = (k: string) => c.config?.[k] ?? "";
  const setCfg = (k: string, v: string) => setC((x) => ({ ...x, config: { ...x.config, [k]: v } }));
  const secretLabel: Record<string, string | null> = {
    telegram: "mon.ch.botToken",
    slack: "mon.ch.webhookUrl",
    discord: "mon.ch.webhookUrl",
    webhook: "mon.ch.hmacSecret",
    email: "mon.ch.smtpPassword",
    pagerduty: "mon.ch.routingKey",
    desktop: null,
  };
  const sl = secretLabel[c.type];
  const save = async () => {
    setBusy(true);
    try {
      await MonitorService.SaveChannel(c, secret, touched);
      toast(t("mon.channelSaved"), "success");
      onSaved();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };
  const test = async () => {
    setBusy(true);
    try {
      await MonitorService.TestChannel(c, touched ? secret : "");
      toast(t("mon.testSent"), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal
      title={c.id ? t("mon.editChannel") : t("mon.newChannel")}
      size="wide"
      onClose={onClose}
      footer={
        <>
          <button className="btn left" onClick={test} disabled={busy}>
            <Send size={13} /> {t("mon.sendTest")}
          </button>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save} disabled={busy}>
            {t("common.save")}
          </button>
        </>
      }
    >
      <div className="row">
        <div className="field">
          <label>{t("mon.ch.type")}</label>
          <Select value={c.type} disabled={!!c.id} onChange={(v) => setC((x) => ({ ...x, type: v }))} options={CHANNEL_TYPES.map((ty) => ({ value: ty, label: t(`mon.ch.t.${ty}` as Key) }))} />
        </div>
        <div className="field">
          <label>{t("mon.ch.name")}</label>
          <input className="input" value={c.name} onChange={(e) => setC((x) => ({ ...x, name: e.target.value }))} placeholder={t(`mon.ch.t.${c.type}` as Key)} />
        </div>
      </div>
      {c.type === "telegram" && (
        <div className="field">
          <label>Chat ID</label>
          <input className="input mono" value={cfg("chatId")} onChange={(e) => setCfg("chatId", e.target.value)} placeholder="-1001234567890" />
          <div className="hint">{t("mon.ch.telegramHint")}</div>
        </div>
      )}
      {c.type === "webhook" && (
        <div className="field">
          <label>URL</label>
          <input className="input mono" value={cfg("url")} onChange={(e) => setCfg("url", e.target.value)} placeholder="https://example.com/hooks/alerts" />
          <div className="hint">{t("mon.ch.webhookHint")}</div>
        </div>
      )}
      {c.type === "email" && (
        <>
          <div className="row">
            <div className="field" style={{ flex: 2 }}>
              <label>SMTP host</label>
              <input className="input mono" value={cfg("host")} onChange={(e) => setCfg("host", e.target.value)} placeholder="smtp.gmail.com" />
            </div>
            <div className="field" style={{ flex: 0.6 }}>
              <label>Port</label>
              <input className="input" value={cfg("port")} onChange={(e) => setCfg("port", e.target.value)} placeholder="587" />
            </div>
            <div className="field" style={{ flex: 1 }}>
              <label>TLS</label>
              <Select
                value={cfg("tls") || "starttls"}
                onChange={(v) => setCfg("tls", v)}
                options={[
                  { value: "starttls", label: "STARTTLS" },
                  { value: "tls", label: "TLS (465)" },
                  { value: "none", label: t("mon.ch.noTls") },
                ]}
              />
            </div>
          </div>
          <div className="row">
            <div className="field">
              <label>{t("mon.ch.username")}</label>
              <input className="input" value={cfg("username")} onChange={(e) => setCfg("username", e.target.value)} />
            </div>
            <div className="field">
              <label>{t("mon.ch.from")}</label>
              <input className="input" value={cfg("from")} onChange={(e) => setCfg("from", e.target.value)} placeholder="alerts@example.com" />
            </div>
          </div>
          <div className="field">
            <label>{t("mon.ch.to")}</label>
            <input className="input" value={cfg("to")} onChange={(e) => setCfg("to", e.target.value)} placeholder="ops@example.com, oncall@example.com" />
          </div>
        </>
      )}
      {sl && (
        <div className="field">
          <label>{t(sl as Key)}</label>
          <input
            className="input mono"
            type="password"
            value={secret}
            autoComplete="off"
            placeholder={c.hasSecret && !touched ? "••••••••" : ""}
            onChange={(e) => {
              setSecret(e.target.value);
              setTouched(true);
            }}
          />
          <div className="hint">{c.hasSecret && !touched ? t("mon.ch.secretKept") : t("mon.ch.secretStored")}</div>
        </div>
      )}
      {c.type === "desktop" && <div className="hint-box">{t("mon.ch.desktopHint")}</div>}
      <label className="check">
        <input type="checkbox" checked={c.default} onChange={(e) => setC((x) => ({ ...x, default: e.target.checked }))} /> {t("mon.ch.default")}
      </label>
      <label className="check" style={{ marginLeft: 16 }}>
        <input type="checkbox" checked={c.enabled} onChange={(e) => setC((x) => ({ ...x, enabled: e.target.checked }))} /> {t("mon.enabled")}
      </label>
    </Modal>
  );
}

export { alertSummary };
