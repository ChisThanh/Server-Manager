import { memo, useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import { Ban, CheckSquare, Clock, Download, Lock, LockOpen, Plus, RefreshCw, ShieldBan, ShieldCheck, Square, Trash2, Unlock, UserCheck, Wand2 } from "lucide-react";
import { Empty, ErrorBox, Loading, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { Select } from "../../ui/Select";
import { useRemote, type Remote } from "../../ui/hooks";
import { runJob } from "../../ui/jobs";
import { Modal } from "../Overlays";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import {
  SecurityService,
  act,
  addrCovers,
  addrTokens,
  confirmOpts,
  errCode,
  errMsg,
  fmtSpan,
  fmtTime,
  isCancelled,
  net24,
  sudo,
  tk,
  validAddr,
  validPortSpec,
  type AccessEntry,
  type AccessResult,
  type AccessState,
  type EventsResult,
  type Fail2banConfig,
  type SecViewProps,
} from "./common";

type Hours = 24 | 168 | 720;
/** A row of the attackers table: one IP, or a /24 grouping several. */
interface Threat {
  addr: string;
  ips: number;
  count: number;
  users: string[];
  last: number;
}
type AddKind = "block" | "allow";

/** Where an address stands: blocked, banned, allowlisted, you. */
function standing(st: AccessState, addr: string): "self" | "allow" | "block" | "ban" | "" {
  if (st.clientIp && addrCovers(addr, st.clientIp)) return "self";
  const es = st.entries ?? [];
  if (es.some((e) => e.kind === "allow" && addrCovers(e.addr, addr))) return "allow";
  if (es.some((e) => e.kind === "block" && e.source !== "fail2ban" && addrCovers(e.addr, addr))) return "block";
  if (es.some((e) => e.kind === "block" && e.source === "fail2ban" && addrCovers(e.addr, addr))) return "ban";
  return "";
}

export function AccessView({ connId, active, canSec, go }: SecViewProps) {
  const t = useT();
  const acc = useRemote(() => sudo(connId, (pw) => SecurityService.AccessList(connId, pw)), [connId], { enabled: active });
  const [hours, setHours] = useState<Hours>(24);
  const ev = useRemote(() => sudo(connId, (pw) => SecurityService.Events(connId, hours, pw)), [connId, hours], { enabled: active });
  const [dialog, setDialog] = useState<{ kind: AddKind; addrs?: string[]; temporary?: boolean } | null>(null);
  const st = acc.data;

  const report = (r: AccessResult, okKey: "sec.acc.blocked" | "sec.acc.allowed" | "sec.acc.removed") => {
    const applied = r.applied ?? [];
    const skipped = r.skipped ?? [];
    if (applied.length) toast(t(okKey, { n: applied.length }), "success");
    if (skipped.length) {
      const list = skipped
        .slice(0, 6)
        .map((s) => `${s.addr} (${tk(`sec.acc.skip.${s.reason}`, undefined, s.reason)}${s.detail && s.reason === "failed" ? `: ${s.detail}` : ""})`)
        .join(", ");
      toast(t("sec.acc.skipped", { n: skipped.length, list: list + (skipped.length > 6 ? "…" : "") }), applied.length ? "info" : "error");
    }
    if (r.notSaved) toast(t("sec.acc.notSaved"), "info");
  };

  const reloadAll = () => {
    acc.reload();
    ev.reload();
  };

  const block = async (addrs: string[], temporary: boolean, comment = "") => {
    try {
      const r = await sudo(connId, (pw) => SecurityService.AccessBlock(connId, { addrs, comment, temporary, ports: "" }, pw));
      report(r, "sec.acc.blocked");
      acc.reload();
      return true;
    } catch (e) {
      if (!isCancelled(e)) toast(errMsg(e), "error");
      return false;
    }
  };
  const allow = async (addrs: string[], ports: string, comment = "") => {
    try {
      const r = await sudo(connId, (pw) => SecurityService.AccessAllow(connId, { addrs, comment, temporary: false, ports }, pw));
      report(r, "sec.acc.allowed");
      acc.reload();
      return true;
    } catch (e) {
      if (!isCancelled(e)) toast(errMsg(e), "error");
      return false;
    }
  };
  const remove = async (entries: AccessEntry[]): Promise<boolean> => {
    if (!entries.length || !st) return false;
    const allows = entries.filter((e) => e.kind === "allow");
    const ok = await confirmOpts({
      serverId: connId,
      title: allows.length === entries.length ? t("sec.acc.removeAllowQ", { n: entries.length }) : t("sec.acc.unblockQ", { n: entries.length }),
      message: <div className="mono sec-small sec-wrap">{entries.slice(0, 12).map((e) => e.addr).join(", ") + (entries.length > 12 ? "…" : "")}</div>,
      confirmText: allows.length ? t("common.delete") : t("sec.acc.unblock"),
      danger: allows.length > 0,
    });
    if (!ok) return false;
    const run = async (force: boolean) => {
      const r = await sudo(connId, (pw) => SecurityService.AccessRemove(connId, entries, force, pw));
      report(r, "sec.acc.removed");
      acc.reload();
    };
    try {
      await run(false);
      return true;
    } catch (e) {
      if (isCancelled(e)) return false;
      if (errCode(e) === "sec.acc.removeSelf") {
        const again = await confirmOpts({ serverId: connId, title: t("sec.acc.removeSelfQ"), message: errMsg(e), confirmText: t("common.delete"), typeWord: "remove" });
        return !!again && (await act(() => run(true)));
      }
      toast(errMsg(e), "error");
      return false;
    }
  };

  if (acc.error && !st) return <ErrorBox error={acc.error} onRetry={acc.reload} />;
  if (!st) return <Loading label={t("sec.acc.loading")} />;

  const f2b = st.fail2ban;
  const entries = st.entries ?? [];
  const blocks = entries.filter((e) => e.kind === "block");
  const allows = entries.filter((e) => e.kind === "allow");
  const fwBlocks = blocks.filter((e) => e.source !== "fail2ban");
  const bans = blocks.filter((e) => e.source === "fail2ban");
  const clientAllowed = !!st.clientIp && allows.some((e) => addrCovers(e.addr, st.clientIp) && (!e.ports || e.ports.split("/")[0].split(",").some((p) => (st.sshPorts ?? []).includes(+p))));
  const canTemp = f2b.running && !!f2b.sshJail;

  return (
    <div className="sec-acc">
      <div className="sec-acc-status">
        <div className="sec-acc-facts">
          <span className="sec-acc-fact">
            <span className="muted">{t("sec.acc.backend")}</span>
            {st.backend === "none" ? (
              <StateBadge tone="err">{t("sec.acc.noBackend")}</StateBadge>
            ) : (
              <StateBadge tone={st.active ? "ok" : "warn"}>
                {st.backend} · {st.active ? t("sec.acc.filtering") : t("sec.acc.notFiltering")}
              </StateBadge>
            )}
          </span>
          <span className="sec-acc-fact">
            <span className="muted">fail2ban</span>
            <StateBadge tone={f2b.running ? "ok" : f2b.installed ? "warn" : "muted"}>{!f2b.installed ? t("sec.f2b.notInstalled") : f2b.running ? t("sec.f2b.running") : t("sec.f2b.stopped")}</StateBadge>
          </span>
          {st.clientIp && (
            <span className="sec-acc-fact" title={t("sec.acc.youHint")}>
              <span className="muted">{t("sec.acc.you")}</span>
              <span className="mono">{st.clientIp}</span>
              <ShieldCheck size={13} className="sec-acc-ok" />
            </span>
          )}
        </div>
        <div className="grow" />
        {canSec && (
          <>
            <button className="btn sm" onClick={() => setDialog({ kind: "allow" })}>
              <UserCheck size={13} /> {t("sec.acc.allowBtn")}
            </button>
            <button className="btn sm danger" onClick={() => setDialog({ kind: "block" })} disabled={st.backend === "none" && !canTemp}>
              <Ban size={13} /> {t("sec.acc.blockBtn")}
            </button>
          </>
        )}
        <button className="icon-btn" onClick={reloadAll} title={t("common.refresh")} disabled={acc.loading}>
          <RefreshCw size={14} className={acc.loading ? "spin-icon" : ""} />
        </button>
      </div>

      {st.backend === "ufw" && !st.active && (
        <div className="hint-box warn sec-acc-hint">
          {t("sec.acc.ufwOff")}{" "}
          <button className="link-btn" onClick={() => go("firewall")}>
            {t("sec.acc.openFirewall")}
          </button>
        </div>
      )}
      {st.backend === "none" && <div className="hint-box warn sec-acc-hint">{t("sec.acc.noBackendHint")}</div>}
      {st.backend === "iptables" && !st.persist && <div className="hint-box sec-acc-hint">{t("sec.acc.iptNoPersist")}</div>}
      {st.docker && <div className="hint-box sec-acc-hint">{t("sec.acc.dockerHint")}</div>}

      <div className="cards">
        <div className="card">
          <div className="k">{t("sec.acc.c.blocked")}</div>
          <div className="v">{fwBlocks.length}</div>
          <div className="s">{st.backend !== "none" ? st.backend : "—"}</div>
        </div>
        <div className="card">
          <div className="k">{t("sec.acc.c.banned")}</div>
          <div className="v">{bans.length}</div>
          <div className="s">{f2b.running ? t("sec.acc.c.bannedSub", { time: fmtSpan(f2b.config.banTime) }) : t("sec.f2b.stopped")}</div>
        </div>
        <div className="card">
          <div className="k">{t("sec.acc.c.allowed")}</div>
          <div className="v">{allows.length}</div>
          <div className="s">{st.sshLocked ? t("sec.acc.c.locked") : t("sec.acc.c.open")}</div>
        </div>
        <div className={`card ${(ev.data?.failedTotal ?? 0) > 50 ? "sec-card-warn" : ""}`}>
          <div className="k">{t("sec.acc.c.attacks")}</div>
          <div className="v">{ev.data ? ev.data.failedTotal : "…"}</div>
          <div className="s">{ev.data ? t("sec.ev.c.ips", { n: ev.data.failedByIp?.length ?? 0 }) : " "}</div>
        </div>
      </div>

      <Threats
        connId={connId}
        st={st}
        ev={ev}
        hours={hours}
        setHours={setHours}
        canSec={canSec}
        canTemp={canTemp}
        onBlock={(addrs, temporary) => setDialog({ kind: "block", addrs, temporary })}
        onQuickBlock={block}
      />

      <EntryList
        title={t("sec.acc.blockList")}
        icon={<ShieldBan size={14} />}
        empty={t("sec.acc.noBlocks")}
        entries={blocks}
        canSec={canSec}
        onRemove={remove}
        removeLabel={t("sec.acc.unblock")}
        removeIcon={<Unlock size={13} />}
        sources
      />

      <EntryList
        title={t("sec.acc.allowList")}
        icon={<UserCheck size={14} />}
        empty={t("sec.acc.noAllows")}
        entries={allows}
        canSec={canSec}
        onRemove={remove}
        removeLabel={t("common.delete")}
        removeIcon={<Trash2 size={13} />}
        clientIp={st.clientIp}
        extra={
          canSec && st.clientIp && !clientAllowed ? (
            <button className="btn sm ghost" onClick={() => allow([st.clientIp], "", t("sec.acc.myIpComment"))}>
              <Plus size={13} /> {t("sec.acc.allowMe")}
            </button>
          ) : undefined
        }
      />

      {st.canLock && <SSHLock connId={connId} st={st} canSec={canSec} clientAllowed={clientAllowed} onChanged={acc.reload} onAllowMe={() => allow([st.clientIp], "", t("sec.acc.myIpComment"))} />}

      <Fail2banSection connId={connId} st={st} canSec={canSec} onChanged={acc.reload} />

      <Section title={t("sec.acc.tips")} icon={<Wand2 size={14} />}>
        <ol className="sec-acc-tips">
          <li>
            {t("sec.acc.tip.keys")}{" "}
            <button className="link-btn" onClick={() => go("ssh")}>
              {t("sec.acc.tip.keysLink")}
            </button>
          </li>
          <li>{t("sec.acc.tip.f2b")}</li>
          <li>{t("sec.acc.tip.allow")}</li>
          <li>{t("sec.acc.tip.subnet")}</li>
          <li>{t("sec.acc.tip.port")}</li>
        </ol>
      </Section>

      {dialog && (
        <AddDialog
          kind={dialog.kind}
          initial={dialog.addrs}
          temporary={dialog.temporary}
          st={st}
          onClose={() => setDialog(null)}
          onSubmit={async (addrs, opts) => {
            const ok = dialog.kind === "block" ? await block(addrs, opts.temporary, opts.comment) : await allow(addrs, opts.ports, opts.comment);
            if (ok) setDialog(null);
          }}
        />
      )}
    </div>
  );
}

// ---- attackers ----

function Threats(props: {
  connId: string;
  st: AccessState;
  ev: Remote<EventsResult>;
  hours: Hours;
  setHours: (h: Hours) => void;
  canSec: boolean;
  canTemp: boolean;
  onBlock: (addrs: string[], temporary: boolean) => void;
  onQuickBlock: (addrs: string[], temporary: boolean) => Promise<boolean>;
}) {
  const t = useT();
  const { st, ev } = props;
  const [grouped, setGrouped] = useState(true);
  const [q, setQ] = useState("");
  const [sel, setSel] = useState<Set<string>>(new Set());
  const [limit, setLimit] = useState(100);

  const rows = useMemo<Threat[]>(() => {
    const list = ev.data?.failedByIp ?? [];
    if (!grouped) return list.map((a) => ({ addr: a.ip, ips: 1, count: a.count, users: a.users ?? [], last: a.last }));
    const by = new Map<string, Threat & { members: string[] }>();
    for (const a of list) {
      const k = net24(a.ip) || a.ip;
      const g = by.get(k);
      if (g) {
        g.members.push(a.ip);
        g.count += a.count;
        g.last = Math.max(g.last, a.last);
        for (const u of a.users ?? []) if (!g.users.includes(u) && g.users.length < 20) g.users.push(u);
      } else by.set(k, { addr: a.ip, ips: 1, count: a.count, users: [...(a.users ?? [])], last: a.last, members: [a.ip] });
    }
    const out: Threat[] = [];
    for (const [k, g] of by) out.push(g.members.length > 1 ? { addr: k, ips: g.members.length, count: g.count, users: g.users, last: g.last } : { addr: g.members[0], ips: 1, count: g.count, users: g.users, last: g.last });
    return out.sort((a, b) => b.count - a.count);
  }, [ev.data, grouped]);

  const needle = q.trim().toLowerCase();
  const shown = rows.filter((r) => !needle || r.addr.includes(needle) || r.users.some((u) => u.toLowerCase().includes(needle)));
  const status = useMemo(() => new Map(rows.map((r) => [r.addr, standing(st, r.addr)])), [rows, st]);
  const selectable = (r: Threat) => !status.get(r.addr);
  const chosen = shown.filter((r) => sel.has(r.addr));
  const toggle = useCallback((addr: string) => setSel((s) => {
    const n = new Set(s);
    if (n.has(addr)) n.delete(addr);
    else n.add(addr);
    return n;
  }), []);
  const pickAtLeast = (n: number) => setSel(new Set(shown.filter((r) => r.count >= n && selectable(r)).map((r) => r.addr)));

  const blockChosen = async (temporary: boolean) => {
    const addrs = chosen.map((r) => r.addr);
    if (temporary && addrs.some((a) => a.includes("/"))) {
      props.onBlock(addrs, true);
      return;
    }
    const ok = await confirmOpts({
      serverId: props.connId,
      title: temporary ? t("sec.acc.tempQ", { n: addrs.length }) : t("sec.acc.blockQ", { n: addrs.length }),
      message: <div className="mono sec-small sec-wrap">{addrs.slice(0, 20).join(", ") + (addrs.length > 20 ? "…" : "")}</div>,
      confirmText: temporary ? t("sec.acc.tempBlock") : t("sec.acc.block"),
      danger: false,
    });
    if (ok && (await props.onQuickBlock(addrs, temporary))) setSel(new Set());
  };

  return (
    <Section
      title={t("sec.acc.threats")}
      icon={<Ban size={14} />}
      actions={
        <button className="icon-btn" onClick={ev.reload} title={t("common.refresh")} disabled={ev.loading}>
          <RefreshCw size={14} className={ev.loading ? "spin-icon" : ""} />
        </button>
      }
    >
      <div className="toolbar sec-acc-bar">
        <div className="segmented sec-seg-sm">
          {([24, 168, 720] as const).map((h) => (
            <button key={h} className={props.hours === h ? "on" : ""} onClick={() => props.setHours(h)}>
              {t(`sec.ev.range.${h}`)}
            </button>
          ))}
        </div>
        <input className="input input-sm" placeholder={t("sec.acc.filterThreats")} value={q} onChange={(e) => setQ(e.target.value)} />
        <label className="check">
          <input type="checkbox" checked={grouped} onChange={(e) => setGrouped(e.target.checked)} /> {t("sec.acc.group24")}
        </label>
        {props.canSec && (
          <Select<string>
            size="sm"
            value=""
            placeholder={t("sec.acc.pick")}
            onChange={(v) => (v === "none" ? setSel(new Set()) : pickAtLeast(+v))}
            options={[
              ...[1000, 100, 20, 5].map((n) => ({ value: String(n), label: t("sec.acc.pickAtLeast", { n }) })),
              { value: "none", label: t("sec.acc.pickNone") },
            ]}
          />
        )}
      </div>
      {props.canSec && chosen.length > 0 && (
        <div className="sec-acc-selbar">
          <b>{t("sec.acc.selected", { n: chosen.length })}</b>
          <button className="link-btn" onClick={() => setSel(new Set())}>
            {t("sec.acc.pickNone")}
          </button>
          <div className="grow" />
          {props.canTemp && (
            <button className="btn sm" onClick={() => blockChosen(true)} title={t("sec.acc.tempHint")}>
              <Clock size={13} /> {t("sec.acc.tempBlock")}
            </button>
          )}
          <button className="btn sm danger" onClick={() => blockChosen(false)} disabled={st.backend === "none"}>
            <Ban size={13} /> {t("sec.acc.block")}
          </button>
        </div>
      )}
      {ev.error && !ev.data ? (
        <ErrorBox error={ev.error} onRetry={ev.reload} />
      ) : !ev.data ? (
        <Loading label={t("sec.ev.loading")} />
      ) : shown.length === 0 ? (
        <Empty title={t("sec.ev.noFailed")} />
      ) : (
        <div className="table-wrap">
          <table className="grid sec-acc-table">
            <thead>
              <tr>
                {props.canSec && <th className="sec-acc-ck" />}
                <th>{t("sec.acc.col.addr")}</th>
                <th className="num">{t("sec.ev.count")}</th>
                <th>{t("sec.ev.users")}</th>
                <th>{t("sec.ev.lastSeen")}</th>
                <th>{t("sec.acc.col.status")}</th>
                {props.canSec && <th />}
              </tr>
            </thead>
            <tbody>
              {shown.slice(0, limit).map((r) => (
                <ThreatRow
                  key={r.addr}
                  r={r}
                  status={status.get(r.addr) ?? ""}
                  checked={sel.has(r.addr)}
                  canSec={props.canSec}
                  canTemp={props.canTemp}
                  noFirewall={st.backend === "none"}
                  onToggle={toggle}
                  onBlock={props.onBlock}
                />
              ))}
            </tbody>
          </table>
          {shown.length > limit && (
            <div className="sec-acc-more">
              <button className="btn sm ghost" onClick={() => setLimit((l) => l + 200)}>
                {t("sec.acc.showMore", { n: shown.length - limit })}
              </button>
            </div>
          )}
        </div>
      )}
    </Section>
  );
}

const ThreatRow = memo(function ThreatRow(p: {
  r: Threat;
  status: string;
  checked: boolean;
  canSec: boolean;
  canTemp: boolean;
  noFirewall: boolean;
  onToggle: (addr: string) => void;
  onBlock: (addrs: string[], temporary: boolean) => void;
}) {
  const t = useT();
  const { r, status } = p;
  const free = !status;
  return (
    <tr className={p.checked ? "picked" : ""}>
      {p.canSec && (
        <td className="sec-acc-ck">
          <input type="checkbox" checked={p.checked} disabled={!free} onChange={() => p.onToggle(r.addr)} />
        </td>
      )}
      <td className="mono">
        {r.addr}
        {r.ips > 1 && <span className="muted sec-small"> · {t("sec.acc.nIps", { n: r.ips })}</span>}
      </td>
      <td className="num">{r.count.toLocaleString()}</td>
      <td className="fill mono sec-small" title={r.users.join(", ")}>
        {r.users.join(", ")}
      </td>
      <td className="sec-small">{fmtTime(r.last)}</td>
      <td>
        {status === "self" && <StateBadge tone="info">{t("sec.acc.st.self")}</StateBadge>}
        {status === "allow" && <StateBadge tone="ok">{t("sec.acc.st.allow")}</StateBadge>}
        {status === "block" && <StateBadge tone="err">{t("sec.acc.st.block")}</StateBadge>}
        {status === "ban" && <StateBadge tone="warn">{t("sec.acc.st.ban")}</StateBadge>}
      </td>
      {p.canSec && (
        <td className="sec-acc-act">
          {free && (
            <div className="sec-acc-btns">
              {p.canTemp && r.ips === 1 && (
                <button className="icon-btn" title={t("sec.acc.tempBlock")} onClick={() => p.onBlock([r.addr], true)}>
                  <Clock size={13} />
                </button>
              )}
              <button className="icon-btn" title={t("sec.acc.block")} disabled={p.noFirewall} onClick={() => p.onBlock([r.addr], false)}>
                <Ban size={13} />
              </button>
            </div>
          )}
        </td>
      )}
    </tr>
  );
});

// ---- block / allow lists ----

function EntryList(props: {
  title: string;
  icon: ReactNode;
  empty: string;
  entries: AccessEntry[];
  canSec: boolean;
  onRemove: (e: AccessEntry[]) => Promise<boolean>;
  removeLabel: string;
  removeIcon: ReactNode;
  sources?: boolean;
  clientIp?: string;
  extra?: ReactNode;
}) {
  const t = useT();
  const [q, setQ] = useState("");
  const [src, setSrc] = useState("");
  const [sel, setSel] = useState<Set<string>>(new Set());
  const key = (e: AccessEntry) => `${e.source}|${e.ref}|${e.addr}`;
  const srcs = Array.from(new Set(props.entries.map((e) => e.source)));
  const needle = q.trim().toLowerCase();
  const shown = props.entries.filter((e) => (!src || e.source === src) && (!needle || e.addr.includes(needle) || e.comment.toLowerCase().includes(needle)));
  const chosen = shown.filter((e) => sel.has(key(e)));
  const all = shown.length > 0 && chosen.length === shown.length;
  const toggle = (e: AccessEntry) =>
    setSel((s) => {
      const n = new Set(s);
      const k = key(e);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });

  return (
    <Section
      title={
        <>
          {props.title} <span className="sec-count">{props.entries.length}</span>
        </>
      }
      icon={props.icon}
      actions={props.extra}
    >
      {props.entries.length === 0 ? (
        <div className="muted sec-acc-empty">{props.empty}</div>
      ) : (
        <>
          <div className="toolbar sec-acc-bar">
            <input className="input input-sm" placeholder={t("sec.acc.filterList")} value={q} onChange={(e) => setQ(e.target.value)} />
            {props.sources && srcs.length > 1 && (
              <Select size="sm" value={src} onChange={setSrc} options={[{ value: "", label: t("sec.acc.allSources") }, ...srcs.map((s) => ({ value: s, label: s }))]} />
            )}
            <div className="grow" />
            {props.canSec && chosen.length > 0 && (
              <button className="btn sm" onClick={async () => (await props.onRemove(chosen)) && setSel(new Set())}>
                {props.removeIcon} {props.removeLabel} ({chosen.length})
              </button>
            )}
          </div>
          <div className="table-wrap">
            <table className="grid sec-acc-table">
              <thead>
                <tr>
                  {props.canSec && (
                    <th className="sec-acc-ck">
                      <button className="icon-btn" onClick={() => setSel(all ? new Set() : new Set(shown.map(key)))} title={t("sec.acc.selectAll")}>
                        {all ? <CheckSquare size={13} /> : <Square size={13} />}
                      </button>
                    </th>
                  )}
                  <th>{t("sec.acc.col.addr")}</th>
                  <th>{t("sec.acc.col.source")}</th>
                  <th>{t("sec.acc.col.ports")}</th>
                  <th>{t("sec.acc.col.comment")}</th>
                  {props.canSec && <th />}
                </tr>
              </thead>
              <tbody>
                {shown.map((e) => (
                  <tr key={key(e)} className={sel.has(key(e)) ? "picked" : ""}>
                    {props.canSec && (
                      <td className="sec-acc-ck">
                        <input type="checkbox" checked={sel.has(key(e))} onChange={() => toggle(e)} />
                      </td>
                    )}
                    <td className="mono">
                      {e.addr}
                      {props.clientIp && addrCovers(e.addr, props.clientIp) && (
                        <>
                          {" "}
                          <StateBadge tone="info">{t("sec.acc.st.self")}</StateBadge>
                        </>
                      )}
                    </td>
                    <td>
                      <span className="badge">{e.source === "fail2ban" ? `fail2ban · ${e.jail}` : e.source}</span>
                    </td>
                    <td className="sec-small">{e.ports || t("sec.acc.allPorts")}</td>
                    <td className="fill sec-small" title={e.comment}>
                      {e.managed && <span className="badge sec-acc-app">app</span>} {e.comment.replace(/^sm(-block|-allow)?:\s?/, "")}
                    </td>
                    {props.canSec && (
                      <td className="sec-acc-act">
                        <button className="icon-btn" title={props.removeLabel} onClick={() => props.onRemove([e])}>
                          {props.removeIcon}
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </Section>
  );
}

// ---- SSH lock ----

function SSHLock(props: { connId: string; st: AccessState; canSec: boolean; clientAllowed: boolean; onChanged: () => void; onAllowMe: () => void }) {
  const t = useT();
  const { st } = props;
  const [busy, setBusy] = useState(false);
  const toggle = async () => {
    const lock = !st.sshLocked;
    const ok = await confirmOpts({
      serverId: props.connId,
      title: lock ? t("sec.acc.lockQ") : t("sec.acc.unlockQ"),
      message: lock ? t("sec.acc.lockMsg", { ports: (st.sshPorts ?? []).join(", ") }) : t("sec.acc.unlockMsg"),
      confirmText: lock ? t("sec.acc.lock") : t("sec.acc.unlock"),
      danger: lock,
      typeWord: lock ? "lock" : undefined,
    });
    if (!ok) return;
    setBusy(true);
    if (await act(() => sudo(props.connId, (pw) => SecurityService.AccessSSHLock(props.connId, lock, pw)), lock ? t("sec.acc.locked") : t("sec.acc.unlocked"))) props.onChanged();
    setBusy(false);
  };
  const blocker = !st.sshLocked && (!st.active ? t("sec.acc.lockNeedActive") : st.backend === "ufw" && !st.defaultDeny ? t("sec.acc.lockNeedDeny") : !props.clientAllowed ? t("sec.acc.lockNeedMe") : "");
  return (
    <Section title={t("sec.acc.lockTitle")} icon={<Lock size={14} />}>
      <div className="sec-acc-lock">
        <div className="sec-acc-lock-text">
          <div>
            <StateBadge tone={st.sshLocked ? "ok" : "muted"}>{st.sshLocked ? t("sec.acc.lockOn") : t("sec.acc.lockOff")}</StateBadge>
          </div>
          <div className="muted sec-small">{t("sec.acc.lockDesc")}</div>
          {blocker && <div className="sec-small sec-acc-warn">{blocker}</div>}
        </div>
        {props.canSec && (
          <div className="sec-acc-lock-btns">
            {!st.sshLocked && !props.clientAllowed && st.clientIp && (
              <button className="btn sm" onClick={props.onAllowMe}>
                <Plus size={13} /> {t("sec.acc.allowMe")}
              </button>
            )}
            <button className={`btn sm ${st.sshLocked ? "" : "primary"}`} onClick={toggle} disabled={busy || !!blocker}>
              {busy ? <span className="spinner" /> : st.sshLocked ? <LockOpen size={13} /> : <Lock size={13} />} {st.sshLocked ? t("sec.acc.unlock") : t("sec.acc.lock")}
            </button>
          </div>
        )}
      </div>
    </Section>
  );
}

// ---- fail2ban ----

const FIND = [300, 600, 1800, 3600, 86400];
const BAN = [600, 3600, 21600, 86400, 604800, -1];
const MAX = [86400, 604800, 2592000, 31536000];
const PRESETS: Record<"recommended" | "strict", Omit<Fail2banConfig, "ignoreIp">> = {
  recommended: { maxRetry: 5, findTime: 600, banTime: 3600, increment: true, maxTime: 604800, recidive: true, aggressive: false },
  strict: { maxRetry: 3, findTime: 3600, banTime: 86400, increment: true, maxTime: 2592000, recidive: true, aggressive: true },
};

function Fail2banSection(props: { connId: string; st: AccessState; canSec: boolean; onChanged: () => void }) {
  const t = useT();
  const f2b = props.st.fail2ban;
  const initial = useRef(f2b.config);
  const [cfg, setCfg] = useState<Fail2banConfig>(() => ({ ...f2b.config, ignoreIp: f2b.config.ignoreIp ?? [] }));
  const [ignoreText, setIgnoreText] = useState(() => (f2b.config.ignoreIp ?? []).join("\n"));
  const [busy, setBusy] = useState(false);
  // Follow the server when it changes and nothing was edited.
  if (initial.current !== f2b.config && JSON.stringify(cfg) === JSON.stringify({ ...initial.current, ignoreIp: initial.current.ignoreIp ?? [] })) {
    initial.current = f2b.config;
    setCfg({ ...f2b.config, ignoreIp: f2b.config.ignoreIp ?? [] });
    setIgnoreText((f2b.config.ignoreIp ?? []).join("\n"));
  }
  const set = <K extends keyof Fail2banConfig>(k: K, v: Fail2banConfig[K]) => setCfg((c) => ({ ...c, [k]: v }));
  const ignore = addrTokens(ignoreText);
  const badIgnore = ignore.filter((a) => !validAddr(a) || a === "any");

  const install = async () => {
    const ok = await confirmOpts({ serverId: props.connId, title: t("sec.acc.f2bInstallQ"), message: t("sec.acc.f2bInstallMsg"), confirmText: t("sec.acc.f2bInstall"), danger: false });
    if (!ok) return;
    const info = await runJob(t("sec.acc.f2bInstall"), () => sudo(props.connId, (pw) => SecurityService.Fail2banInstall(props.connId, pw)));
    if (info) props.onChanged();
  };
  const apply = async () => {
    setBusy(true);
    const body = { ...cfg, ignoreIp: ignore };
    if (await act(() => sudo(props.connId, (pw) => SecurityService.Fail2banApply(props.connId, body, pw)), t("sec.acc.f2bApplied"))) props.onChanged();
    setBusy(false);
  };
  const setRunning = async (on: boolean) => {
    if (!on) {
      const ok = await confirmOpts({ serverId: props.connId, title: t("sec.acc.f2bStopQ"), message: t("sec.acc.f2bStopMsg"), confirmText: t("sec.acc.f2bStop") });
      if (!ok) return;
    }
    setBusy(true);
    if (await act(() => sudo(props.connId, (pw) => SecurityService.Fail2banSetRunning(props.connId, on, pw)), on ? t("sec.acc.f2bStarted") : t("sec.acc.f2bStopped"))) props.onChanged();
    setBusy(false);
  };
  const spanOpts = (list: number[], cur: number) => (list.includes(cur) ? list : [...list, cur].sort((a, b) => (a < 0 ? 1 : b < 0 ? -1 : a - b))).map((n) => ({ value: n, label: fmtSpan(n) }));

  return (
    <Section
      title={t("sec.acc.f2bTitle")}
      icon={<Clock size={14} />}
      actions={
        f2b.installed && props.canSec ? (
          f2b.running ? (
            <button className="btn sm ghost" onClick={() => setRunning(false)} disabled={busy}>
              {t("sec.acc.f2bStop")}
            </button>
          ) : null
        ) : undefined
      }
    >
      {!f2b.installed ? (
        <div className="sec-acc-f2b-missing">
          <div className="muted">{t("sec.acc.f2bMissing")}</div>
          {props.canSec &&
            (f2b.pkgManager ? (
              <button className="btn sm primary" onClick={install}>
                <Download size={13} /> {t("sec.acc.f2bInstall")}
              </button>
            ) : (
              <span className="muted sec-small">{t("sec.acc.noPkg")}</span>
            ))}
        </div>
      ) : (
        <>
          <div className="muted sec-small sec-acc-f2b-state">
            {f2b.running ? t("sec.acc.f2bRunningInfo", { version: f2b.version || "?", jails: (f2b.jails ?? []).join(", ") || "—" }) : t("sec.acc.f2bStoppedInfo")}
            {!f2b.managed && ` ${t("sec.acc.f2bUnmanaged")}`}
          </div>
          <div className="sec-acc-presets">
            <span className="muted sec-small">{t("sec.acc.preset")}</span>
            <button className="btn sm ghost" disabled={!props.canSec} onClick={() => setCfg((c) => ({ ...c, ...PRESETS.recommended }))}>
              {t("sec.acc.preset.recommended")}
            </button>
            <button className="btn sm ghost" disabled={!props.canSec} onClick={() => setCfg((c) => ({ ...c, ...PRESETS.strict }))}>
              {t("sec.acc.preset.strict")}
            </button>
          </div>
          <div className="sec-acc-f2b-grid">
            <div className="field">
              <label>{t("sec.acc.f.maxRetry")}</label>
              <input className="input" type="number" min={1} max={100} value={cfg.maxRetry} disabled={!props.canSec} onChange={(e) => set("maxRetry", Math.max(1, Math.min(100, +e.target.value || 1)))} />
            </div>
            <div className="field">
              <label>{t("sec.acc.f.findTime")}</label>
              <Select value={cfg.findTime} disabled={!props.canSec} onChange={(v) => set("findTime", v)} options={spanOpts(FIND, cfg.findTime)} />
            </div>
            <div className="field">
              <label>{t("sec.acc.f.banTime")}</label>
              <Select value={cfg.banTime} disabled={!props.canSec} onChange={(v) => set("banTime", v)} options={spanOpts(BAN, cfg.banTime)} />
            </div>
            <div className="field">
              <label>{t("sec.acc.f.maxTime")}</label>
              <Select value={cfg.maxTime} disabled={!props.canSec || !cfg.increment || cfg.banTime < 0} onChange={(v) => set("maxTime", v)} options={spanOpts(MAX, cfg.maxTime)} />
            </div>
          </div>
          <div className="sec-acc-f2b-checks">
            <label className="check">
              <input type="checkbox" checked={cfg.increment} disabled={!props.canSec || cfg.banTime < 0} onChange={(e) => set("increment", e.target.checked)} /> {t("sec.acc.f.increment")}
            </label>
            <label className="check">
              <input type="checkbox" checked={cfg.recidive} disabled={!props.canSec} onChange={(e) => set("recidive", e.target.checked)} /> {t("sec.acc.f.recidive")}
            </label>
            <label className="check">
              <input type="checkbox" checked={cfg.aggressive} disabled={!props.canSec} onChange={(e) => set("aggressive", e.target.checked)} /> {t("sec.acc.f.aggressive")}
            </label>
          </div>
          <div className="muted sec-small sec-acc-f2b-sum">{t("sec.acc.f2bSummary", { n: cfg.maxRetry, find: fmtSpan(cfg.findTime), ban: fmtSpan(cfg.banTime) })}</div>
          <div className="field">
            <label>{t("sec.acc.f.ignore")}</label>
            <textarea className={`input mono ${badIgnore.length ? "invalid" : ""}`} rows={3} value={ignoreText} disabled={!props.canSec} placeholder="203.0.113.10&#10;10.0.0.0/8" onChange={(e) => setIgnoreText(e.target.value)} />
            <div className={badIgnore.length ? "error" : "hint"}>{badIgnore.length ? t("sec.acc.invalidAddrs", { list: badIgnore.join(", ") }) : t("sec.acc.f.ignoreHint", { ip: props.st.clientIp || "—" })}</div>
          </div>
          {props.canSec && (
            <div className="sec-acc-f2b-actions">
              <button className="btn primary" onClick={apply} disabled={busy || badIgnore.length > 0}>
                {busy ? <span className="spinner" /> : <ShieldCheck size={14} />} {f2b.running ? t("sec.acc.f2bApply") : t("sec.acc.f2bApplyStart")}
              </button>
              <span className="muted sec-small mono">/etc/fail2ban/jail.d/zz-server-manager.local</span>
            </div>
          )}
        </>
      )}
    </Section>
  );
}

// ---- add dialog ----

function AddDialog(props: {
  kind: AddKind;
  initial?: string[];
  temporary?: boolean;
  st: AccessState;
  onClose: () => void;
  onSubmit: (addrs: string[], opts: { temporary: boolean; ports: string; comment: string }) => Promise<void>;
}) {
  const t = useT();
  const { st } = props;
  const canTemp = st.fail2ban.running && !!st.fail2ban.sshJail;
  const [text, setText] = useState((props.initial ?? []).join("\n"));
  const [temporary, setTemporary] = useState(!!props.temporary && canTemp);
  const [portMode, setPortMode] = useState<"all" | "ssh" | "custom">("all");
  const [ports, setPorts] = useState("");
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const tokens = addrTokens(text);
  const bad = tokens.filter((a) => !validAddr(a) || a === "any");
  const good = tokens.filter((a) => !bad.includes(a));
  const self = good.filter((a) => st.clientIp && addrCovers(a, st.clientIp));
  const cidrTemp = temporary ? good.filter((a) => a.includes("/")) : [];
  const portsOk = portMode !== "custom" || validPortSpec(ports, false);
  const block = props.kind === "block";
  const noFw = st.backend === "none";

  const submit = async () => {
    setBusy(true);
    await props.onSubmit(good, { temporary: block && temporary, ports: portMode === "all" ? "" : portMode === "ssh" ? "ssh" : ports.trim(), comment: comment.trim() });
    setBusy(false);
  };

  return (
    <Modal
      title={block ? t("sec.acc.blockTitle") : t("sec.acc.allowTitle")}
      onClose={props.onClose}
      footer={
        <>
          <button className="btn" onClick={props.onClose}>
            {t("common.cancel")}
          </button>
          <button className={`btn ${block ? "danger" : "primary"}`} disabled={busy || good.length === 0 || !portsOk || (block && !temporary && noFw)} onClick={submit}>
            {busy ? <span className="spinner" /> : block ? <Ban size={13} /> : <UserCheck size={13} />} {block ? t("sec.acc.blockN", { n: good.length }) : t("sec.acc.allowN", { n: good.length })}
          </button>
        </>
      }
    >
      <div className="field">
        <label>{t("sec.acc.addrs")}</label>
        <textarea className={`input mono ${bad.length ? "invalid" : ""}`} rows={6} autoFocus value={text} placeholder={"192.0.2.86\n192.0.2.0/24\n2001:db8::1"} onChange={(e) => setText(e.target.value)} />
        <div className={bad.length ? "error" : "hint"}>{bad.length ? t("sec.acc.invalidAddrs", { list: bad.slice(0, 8).join(", ") }) : t("sec.acc.addrsHint")}</div>
      </div>
      {block ? (
        <div className="field">
          <label>{t("sec.acc.mode")}</label>
          <div className="segmented">
            <button type="button" className={!temporary ? "on" : ""} onClick={() => setTemporary(false)} disabled={noFw}>
              {t("sec.acc.modePerm", { fw: st.backend })}
            </button>
            <button type="button" className={temporary ? "on" : ""} onClick={() => setTemporary(true)} disabled={!canTemp}>
              {t("sec.acc.modeTemp", { time: fmtSpan(st.fail2ban.config.banTime) })}
            </button>
          </div>
          <div className="hint">{temporary ? t("sec.acc.modeTempHint") : t("sec.acc.modePermHint")}</div>
          {!canTemp && <div className="hint">{t("sec.acc.tempNeedsF2b")}</div>}
        </div>
      ) : (
        <div className="field">
          <label>{t("sec.acc.ports")}</label>
          <div className="segmented">
            <button type="button" className={portMode === "all" ? "on" : ""} onClick={() => setPortMode("all")}>
              {t("sec.acc.allPorts")}
            </button>
            <button type="button" className={portMode === "ssh" ? "on" : ""} onClick={() => setPortMode("ssh")}>
              {t("sec.acc.sshOnly", { ports: (st.sshPorts ?? []).join(",") })}
            </button>
            <button type="button" className={portMode === "custom" ? "on" : ""} onClick={() => setPortMode("custom")}>
              {t("sec.acc.customPorts")}
            </button>
          </div>
          {portMode === "custom" && <input className={`input mono ${portsOk ? "" : "invalid"}`} placeholder="22,443" value={ports} onChange={(e) => setPorts(e.target.value)} />}
          <div className="hint">{t("sec.acc.allowHint")}</div>
        </div>
      )}
      <div className="field">
        <label>{t("sec.acc.col.comment")}</label>
        <input className="input" maxLength={60} value={comment} placeholder={block ? t("sec.acc.commentBlockPh") : t("sec.acc.commentAllowPh")} onChange={(e) => setComment(e.target.value)} />
      </div>
      {block && self.length > 0 && <div className="hint-box warn">{t("sec.acc.selfWarn", { list: self.join(", ") })}</div>}
      {cidrTemp.length > 0 && <div className="hint-box warn">{t("sec.acc.cidrTempWarn", { list: cidrTemp.join(", ") })}</div>}
    </Modal>
  );
}
