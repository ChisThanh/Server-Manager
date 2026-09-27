import { useMemo, useState } from "react";
import { Globe, Lock, Network, RefreshCw, ShieldPlus } from "lucide-react";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { useT } from "../../i18n";
import { SecurityService, isCancelled, sudo, type Listener, type PortsResult, type SecViewProps } from "./common";

type Filter = "all" | "public" | "local";

interface Group {
  process: string;
  pids: number[];
  items: Listener[];
  public: boolean;
}

export function PortsView({ connId, active, canSec, go }: SecViewProps) {
  const t = useT();
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [grouped, setGrouped] = useState(true);
  const ports = useRemote<PortsResult>(
    async () => {
      try {
        return await sudo(connId, (pw) => SecurityService.OpenPorts(connId, true, pw));
      } catch (e) {
        // Without sudo, still show the sockets (process names of other users hidden).
        if (isCancelled(e)) return SecurityService.OpenPorts(connId, false, "");
        throw e;
      }
    },
    [connId],
    { enabled: active },
  );
  const d = ports.data;

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return (d?.listeners ?? []).filter((l) => {
      if (filter === "public" && !l.public) return false;
      if (filter === "local" && l.public) return false;
      return !needle || String(l.port).includes(needle) || l.process.toLowerCase().includes(needle) || l.address.includes(needle);
    });
  }, [d, q, filter]);

  const groups = useMemo(() => {
    const m = new Map<string, Group>();
    for (const l of rows) {
      const k = l.process || "?";
      const g = m.get(k) ?? { process: k, pids: [], items: [], public: false };
      g.items.push(l);
      g.public = g.public || l.public;
      for (const p of l.pids ?? []) if (!g.pids.includes(p)) g.pids.push(p);
      m.set(k, g);
    }
    return [...m.values()].sort((a, b) => Number(b.public) - Number(a.public) || a.process.localeCompare(b.process));
  }, [rows]);

  const nPublic = (d?.listeners ?? []).filter((l) => l.public).length;

  const ruleFor = (l: Listener, action: "allow" | "deny") => go("firewall", { rule: { action, port: String(l.port), proto: l.proto, comment: l.process } });

  const row = (l: Listener, i: number) => (
    <tr key={`${l.proto}-${l.address}-${l.port}-${i}`}>
      <td className="num mono">{l.port}</td>
      <td className="mono">{l.proto}</td>
      <td className="mono">{l.address}</td>
      <td>
        {l.public ? (
          <StateBadge tone="warn">
            <Globe size={11} /> {t(l.scope === "any" ? "sec.ports.allIfaces" : "sec.ports.public")}
          </StateBadge>
        ) : (
          <StateBadge tone="ok">
            <Lock size={11} /> {t(l.scope === "loopback" ? "sec.ports.loopback" : "sec.ports.local")}
          </StateBadge>
        )}
      </td>
      {!grouped && <td className="mono">{l.process || <span className="muted">?</span>}</td>}
      {!grouped && <td className="mono muted">{(l.pids ?? []).join(", ")}</td>}
      <td style={{ width: 170, textAlign: "right" }}>
        {canSec && l.public && (
          <span className="sec-btn-row">
            <button className="btn ghost sm" title={t("sec.ports.allowHint")} onClick={() => ruleFor(l, "allow")}>
              <ShieldPlus size={12} /> {t("sec.ports.allow")}
            </button>
            <button className="btn ghost sm" title={t("sec.ports.denyHint")} onClick={() => ruleFor(l, "deny")}>
              {t("sec.ports.deny")}
            </button>
          </span>
        )}
      </td>
    </tr>
  );

  if (ports.error && !d) return <ErrorBox error={ports.error} onRetry={ports.reload} />;
  if (!d) return <Loading />;

  return (
    <div className="sec-ports">
      <div className="toolbar">
        <input className="input input-sm" placeholder={t("sec.ports.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="segmented sec-seg">
          {(["all", "public", "local"] as const).map((k) => (
            <button key={k} className={filter === k ? "on" : ""} onClick={() => setFilter(k)}>
              {t(`sec.ports.f.${k}`)}
            </button>
          ))}
        </div>
        <label className="check">
          <input type="checkbox" checked={grouped} onChange={(e) => setGrouped(e.target.checked)} /> {t("sec.ports.group")}
        </label>
        <div className="grow" />
        <span className="muted sec-small">
          {t("sec.ports.summary", { n: d.listeners?.length ?? 0, pub: nPublic, tool: d.tool })}
        </span>
        <button className="icon-btn" onClick={ports.reload} title={t("common.refresh")} disabled={ports.loading}>
          <RefreshCw size={14} className={ports.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {d.limited && <div className="hint-box">{t("sec.ports.limited")}</div>}
      {rows.length === 0 ? (
        <Empty icon={<Network size={28} />} title={t("sec.ports.none")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th className="num">{t("sec.ports.port")}</th>
                <th>{t("sec.fw.proto")}</th>
                <th>{t("sec.ports.address")}</th>
                <th>{t("sec.ports.exposure")}</th>
                {!grouped && <th>{t("sec.ports.process")}</th>}
                {!grouped && <th>PID</th>}
                <th />
              </tr>
            </thead>
            <tbody>
              {grouped
                ? groups.map((g) => [
                    <tr key={`g-${g.process}`} className="sec-group-row">
                      <td colSpan={5}>
                        <b className="mono">{g.process === "?" ? t("sec.ports.unknownProc") : g.process}</b>
                        {g.pids.length > 0 && <span className="muted mono sec-small"> · PID {g.pids.slice(0, 6).join(", ")}{g.pids.length > 6 ? "…" : ""}</span>}
                        <span className="muted sec-small"> · {t("sec.ports.nSockets", { n: g.items.length })}</span>
                      </td>
                    </tr>,
                    ...g.items.map(row),
                  ])
                : rows.map(row)}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
