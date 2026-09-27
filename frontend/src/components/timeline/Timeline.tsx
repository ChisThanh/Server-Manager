import { useEffect, useState } from "react";
import { Events } from "@wailsio/runtime";
import { AlertTriangle, Bell, CheckCircle2, Info, Plug, Rocket, Archive, User, XCircle, Shield, Boxes, Server as ServerIcon, HeartPulse } from "lucide-react";
import { AppService } from "../../../bindings/server-manager/services";
import type { Event as TEvent } from "../../../bindings/server-manager/internal/core";
import { hasKey, t, useT, type Key } from "../../i18n";
import { useApp } from "../../store/app";

/** Readable label for an audit action code ("service.restart"). */
export function actionLabel(action: string): string {
  const k = `audit.${action}`;
  return hasKey(k) ? t(k as Key) : action;
}

export function eventText(e: TEvent): string {
  if (e.code === "action") {
    const target = e.params?.target ?? "";
    return `${actionLabel(e.params?.action ?? "")}${target ? `: ${target}` : ""}`;
  }
  const k = `ev.${e.code}`;
  const params = Object.fromEntries(Object.entries(e.params ?? {}).map(([a, b]) => [a, b ?? ""]));
  return hasKey(k) ? t(k as Key, params) : e.code;
}

const kindIcon: Record<string, React.ReactNode> = {
  alert: <Bell size={13} />,
  conn: <Plug size={13} />,
  deploy: <Rocket size={13} />,
  backup: <Archive size={13} />,
  action: <User size={13} />,
  security: <Shield size={13} />,
  container: <Boxes size={13} />,
  service: <ServerIcon size={13} />,
  health: <HeartPulse size={13} />,
};

function sevIcon(sev: string) {
  if (sev === "crit") return <XCircle size={13} color="var(--err)" />;
  if (sev === "warn") return <AlertTriangle size={13} color="var(--warn)" />;
  if (sev === "ok") return <CheckCircle2 size={13} color="var(--ok)" />;
  return <Info size={13} color="var(--fg-2)" />;
}

export function fmtTime(ms: number) {
  const d = new Date(ms);
  const today = new Date();
  const pad = (x: number) => String(x).padStart(2, "0");
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.toDateString() === today.toDateString()) return hm;
  return `${pad(d.getDate())}/${pad(d.getMonth() + 1)} ${hm}`;
}

/**
 * Event timeline (alerts, connection drops, deployments, backups, user
 * actions…) for one server or all servers, updated live.
 */
export function Timeline({ serverId, limit = 50, kinds, showServer, compact }: { serverId?: string; limit?: number; kinds?: string[]; showServer?: boolean; compact?: boolean }) {
  const tr = useT();
  const [items, setItems] = useState<TEvent[] | null>(null);
  const [more, setMore] = useState(true);
  const servers = useApp((s) => s.servers);
  const kindKey = (kinds ?? []).join(",");

  useEffect(() => {
    let alive = true;
    AppService.Events({ server: serverId ?? "", kinds: kinds ?? [], severity: [], since: 0, until: 0, limit, before: 0 })
      .then((l) => {
        if (!alive) return;
        setItems(l ?? []);
        setMore((l ?? []).length >= limit);
      })
      .catch(() => alive && setItems([]));
    const off = Events.On("timeline:new", (ev) => {
      const e = ev.data;
      if (serverId && e.server !== serverId) return;
      if (kinds && kinds.length && !kinds.includes(e.kind)) return;
      setItems((l) => [e, ...(l ?? [])].slice(0, 500));
    });
    return () => {
      alive = false;
      off();
    };
  }, [serverId, kindKey, limit]);

  const loadMore = async () => {
    const last = items?.[items.length - 1];
    if (!last) return;
    const l = (await AppService.Events({ server: serverId ?? "", kinds: kinds ?? [], severity: [], since: 0, until: 0, limit, before: last.id })) ?? [];
    setItems((x) => [...(x ?? []), ...l]);
    setMore(l.length >= limit);
  };

  if (items === null) return <div className="muted" style={{ padding: 10 }}>{tr("common.loading")}</div>;
  if (items.length === 0) return <div className="muted" style={{ padding: 10 }}>{tr("mon.noEvents")}</div>;
  return (
    <div className={`timeline ${compact ? "compact" : ""}`}>
      {items.map((e) => (
        <div className={`tl-row ${e.severity}`} key={e.id}>
          <span className="tl-time">{fmtTime(e.ts)}</span>
          <span className="tl-icon">{e.kind === "action" ? kindIcon.action : sevIcon(e.severity)}</span>
          <span className="tl-text">
            {showServer && e.server && <b className="tl-server">{servers.find((s) => s.id === e.server)?.name ?? "?"} · </b>}
            {eventText(e)}
            {e.actor && <span className="tl-actor"> — {e.actor}</span>}
            {e.detail && !compact && <span className="tl-detail">{e.detail}</span>}
          </span>
          <span className="tl-kind muted">{kindIcon[e.kind]}</span>
        </div>
      ))}
      {more && !compact && (
        <button className="btn sm" style={{ alignSelf: "center", marginTop: 8 }} onClick={loadMore}>
          {tr("mon.loadMore")}
        </button>
      )}
    </div>
  );
}
