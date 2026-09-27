import { useMemo, useState } from "react";
import { Ban, Lock, Pause, Play, RefreshCw, XCircle } from "lucide-react";
import { useRemote } from "../../ui/hooks";
import { confirmDanger } from "../../ui/confirm";
import { Empty, ErrorBox, Loading, Section } from "../../ui/Page";
import { StateBadge, type Tone } from "../../ui/Status";
import { Modal } from "../Overlays";
import { errMsg } from "../../lib/api";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import {
  DatabaseService,
  POLL_MS,
  dbs,
  shortDur,
  type Session,
  type TargetViewProps,
} from "./common";

const LONG_WARN = 60;
const LONG_CRIT = 300;

function stateTone(s: Session): Tone {
  if (s.state === "active")
    return s.duration >= LONG_CRIT
      ? "err"
      : s.duration >= LONG_WARN
        ? "warn"
        : "ok";
  if (s.state.startsWith("idle in transaction")) return "warn";
  if (s.state === "idle" || s.state === "background") return "muted";
  return "info";
}

export function ActivityView({
  connId,
  target,
  active,
  canEdit,
}: TargetViewProps) {
  const t = useT();
  const pg = target.engine === "postgres";
  const [paused, setPaused] = useState(false);
  const [hideIdle, setHideIdle] = useState(true);
  const [clientsOnly, setClientsOnly] = useState(true);
  const [q, setQ] = useState("");
  const [busy, setBusy] = useState<number | null>(null);
  const [detail, setDetail] = useState<Session | null>(null);
  const act = useRemote(
    () => dbs(connId, (pw) => DatabaseService.Activity(connId, target.id, pw)),
    [connId, target.id],
    {
      enabled: active,
      poll: paused ? undefined : POLL_MS,
    },
  );
  const a = act.data;

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return (a?.sessions ?? []).filter((s) => {
      if (hideIdle && (s.state === "idle" || s.state === "")) return false;
      if (
        clientsOnly &&
        s.backendType !== "client backend" &&
        s.backendType !== "client"
      )
        return false;
      if (!needle) return true;
      return [s.user, s.database, s.app, s.client, s.query, String(s.id)].some(
        (v) => v.toLowerCase().includes(needle),
      );
    });
  }, [a, hideIdle, clientsOnly, q]);

  const total = (a?.sessions ?? []).filter(
    (s) => s.backendType === "client backend" || s.backendType === "client",
  ).length;

  const signal = async (s: Session, terminate: boolean) => {
    const title = terminate
      ? t(pg ? "db.act.terminateQ" : "db.act.killConnQ", { id: s.id })
      : t(pg ? "db.act.cancelQ" : "db.act.killQueryQ", { id: s.id });
    const ok = await confirmDanger({
      serverId: connId,
      title,
      message: (
        <>
          <div className="muted">
            {s.user}@{s.database || "–"} · {s.client || "local"}
          </div>
          <pre className="db-confirm-sql">{s.query.slice(0, 500) || "–"}</pre>
        </>
      ),
      confirmText: terminate ? t("db.act.terminate") : t("db.act.cancel"),
    });
    if (!ok) return;
    setBusy(s.id);
    try {
      await dbs(connId, (pw) =>
        DatabaseService.CancelSession(connId, target.id, s.id, terminate, pw),
      );
      toast(
        terminate
          ? t("db.act.terminated", { id: s.id })
          : t("db.act.cancelled", { id: s.id }),
        "success",
      );
      await act.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
    }
  };

  if (act.error && !a)
    return <ErrorBox error={act.error} onRetry={act.reload} />;
  if (!a) return <Loading />;

  return (
    <>
      <div className="toolbar">
        <input
          className="input"
          placeholder={t("db.filter")}
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <label className="check">
          <input
            type="checkbox"
            checked={hideIdle}
            onChange={(e) => setHideIdle(e.target.checked)}
          />{" "}
          {t("db.act.hideIdle")}
        </label>
        <label className="check">
          <input
            type="checkbox"
            checked={clientsOnly}
            onChange={(e) => setClientsOnly(e.target.checked)}
          />{" "}
          {t("db.act.clientsOnly")}
        </label>
        <div className="grow" />
        <span className="muted">
          {t("db.act.count", { n: total, max: a.maxConnections })}
        </span>
        <button
          className="icon-btn"
          onClick={() => setPaused(!paused)}
          title={paused ? t("db.resume") : t("db.pause")}
        >
          {paused ? <Play size={14} /> : <Pause size={14} />}
        </button>
        <button
          className="icon-btn"
          onClick={act.reload}
          title={t("common.refresh")}
        >
          <RefreshCw size={14} className={act.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {rows.length === 0 ? (
        <Empty text={t("db.act.none")} />
      ) : (
        <div className="table-wrap db-scroll-x">
          <table className="grid">
            <thead>
              <tr>
                <th className="num">{pg ? "PID" : "ID"}</th>
                <th>{t("db.act.user")}</th>
                <th>{t("db.act.db")}</th>
                <th>{t("db.act.client")}</th>
                <th>{t("db.act.state")}</th>
                {pg && <th>{t("db.act.wait")}</th>}
                <th className="num">{t("db.act.duration")}</th>
                <th>{t("db.act.query")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((s) => {
                const long = s.state === "active" && s.duration >= LONG_WARN;
                return (
                  <tr
                    key={s.id}
                    className={
                      long
                        ? s.duration >= LONG_CRIT
                          ? "db-row-crit"
                          : "db-row-warn"
                        : ""
                    }
                  >
                    <td className="num mono">{s.id}</td>
                    <td>
                      {s.user || <span className="muted">{s.backendType}</span>}
                    </td>
                    <td className="mono">{s.database}</td>
                    <td className="muted" title={s.app}>
                      {s.client}
                      {s.app && <div className="db-sub">{s.app}</div>}
                    </td>
                    <td>
                      <StateBadge tone={stateTone(s)}>
                        {s.state || "–"}
                      </StateBadge>
                      {!pg && s.command && s.command !== "Query" && (
                        <div className="db-sub">{s.command}</div>
                      )}
                      {(s.blockedBy ?? []).length > 0 && (
                        <div className="db-sub db-warn">
                          <Lock size={10} />{" "}
                          {t("db.act.blockedBy", {
                            ids: (s.blockedBy ?? []).join(", "),
                          })}
                        </div>
                      )}
                    </td>
                    {pg && <td className="muted">{s.wait}</td>}
                    <td className={`num ${long ? "db-warn" : ""}`}>
                      {s.duration >= 0 ? shortDur(s.duration) : "–"}
                    </td>
                    <td
                      className="cmd db-query-cell mono"
                      onClick={() => setDetail(s)}
                      title={t("db.act.showQuery")}
                    >
                      {s.query.replace(/\s+/g, " ").slice(0, 160)}
                    </td>
                    <td className="db-actions">
                      {canEdit &&
                        (busy === s.id ? (
                          <span className="spinner" />
                        ) : (
                          <>
                            <button
                              className="icon-btn"
                              title={
                                pg
                                  ? t("db.act.cancelPg")
                                  : t("db.act.killQuery")
                              }
                              onClick={() => signal(s, false)}
                              disabled={s.state === "idle"}
                            >
                              <Ban size={13} />
                            </button>
                            <button
                              className="icon-btn danger"
                              title={
                                pg
                                  ? t("db.act.terminatePg")
                                  : t("db.act.killConn")
                              }
                              onClick={() => signal(s, true)}
                            >
                              <XCircle size={13} />
                            </button>
                          </>
                        ))}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {pg && (a.locks ?? []).length > 0 && (
        <Section
          title={t("db.act.locks", { n: (a.locks ?? []).length })}
          icon={<Lock size={14} />}
        >
          <div className="table-wrap db-scroll-x">
            <table className="grid">
              <thead>
                <tr>
                  <th className="num">PID</th>
                  <th>{t("db.act.user")}</th>
                  <th>{t("db.act.lock")}</th>
                  <th>{t("db.act.relation")}</th>
                  <th>{t("db.act.blocker")}</th>
                  <th className="num">{t("db.act.waiting")}</th>
                  <th>{t("db.act.query")}</th>
                </tr>
              </thead>
              <tbody>
                {(a.locks ?? []).map((l, i) => (
                  <tr key={i}>
                    <td className="num mono">{l.pid}</td>
                    <td>{l.user}</td>
                    <td className="mono">
                      {l.lockType} · {l.mode}
                    </td>
                    <td className="mono">{l.relation || "–"}</td>
                    <td className="mono db-warn">
                      {(l.blockedBy ?? []).join(", ") || "–"}
                    </td>
                    <td className="num">{shortDur(l.waiting)}</td>
                    <td className="cmd mono">
                      {l.query.replace(/\s+/g, " ").slice(0, 120)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Section>
      )}
      {detail && (
        <Modal
          title={t("db.act.queryOf", { id: detail.id })}
          size="wide"
          onClose={() => setDetail(null)}
          footer={
            <button className="btn" onClick={() => setDetail(null)}>
              {t("common.close")}
            </button>
          }
        >
          <div className="muted db-pad">
            {detail.user}@{detail.database} · {detail.client} · {detail.app} ·{" "}
            {detail.state} · {shortDur(detail.duration)}
          </div>
          <pre className="log-view db-cell-full">{detail.query || "–"}</pre>
        </Modal>
      )}
    </>
  );
}
