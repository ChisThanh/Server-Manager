import { useState } from "react";
import { FileText, Info, RefreshCw, RotateCcw, Wrench } from "lucide-react";
import { useRemote } from "../../ui/hooks";
import { confirmDanger } from "../../ui/confirm";
import { Empty, ErrorBox, Loading, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { Modal } from "../Overlays";
import { errMsg } from "../../lib/api";
import { formatDate } from "../../lib/format";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import {
  DatabaseService,
  dbs,
  ms,
  num,
  pct,
  type RedisSlow,
  type SlowLogSettings,
  type TargetViewProps,
} from "./common";

type Order = "total" | "mean" | "calls" | "max";

export function SlowView({ connId, target, active, canEdit }: TargetViewProps) {
  const t = useT();
  const pg = target.engine === "postgres";
  const [order, setOrder] = useState<Order>("total");
  const [busy, setBusy] = useState(false);
  const [expanded, setExpanded] = useState<number | null>(null);
  const rep = useRemote(
    () =>
      dbs(connId, (pw) =>
        DatabaseService.SlowQueries(connId, target.id, "", order, pw),
      ),
    [connId, target.id, order],
    { enabled: active },
  );
  const r = rep.data;

  const action = async (fn: (pw: string) => Promise<void>, okMsg: string) => {
    setBusy(true);
    try {
      await dbs(connId, fn);
      toast(okMsg, "success");
      await rep.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    const ok = await confirmDanger({
      serverId: connId,
      title: t("db.slow.resetQ"),
      message: t("db.slow.resetMsg"),
      confirmText: t("db.slow.reset"),
    });
    if (ok)
      action(
        (pw) => DatabaseService.ResetSlowStats(connId, target.id, "", pw),
        t("db.slow.resetDone"),
      );
  };

  if (rep.error && !r)
    return <ErrorBox error={rep.error} onRetry={rep.reload} />;
  if (!r) return <Loading />;

  const queries = r.queries ?? [];
  return (
    <>
      {!pg && (
        <SlowLogBox
          connId={connId}
          targetId={target.id}
          s={r.slowLog}
          canEdit={canEdit}
          onChanged={rep.reload}
        />
      )}
      {r.source === "none" ? (
        <div className="hint-box db-howto">
          <Info size={14} />
          <div>
            {r.reason === "notPreloaded" && (
              <>
                <b>{t("db.slow.pgNotPreloaded")}</b>
                <ol>
                  <li>
                    {t("db.slow.stepConf")}{" "}
                    <code>shared_preload_libraries = 'pg_stat_statements'</code>{" "}
                    ({t("db.slow.orRun")}{" "}
                    <code>
                      ALTER SYSTEM SET shared_preload_libraries =
                      'pg_stat_statements';
                    </code>
                    )
                  </li>
                  <li>
                    {t("db.slow.stepRestart")}{" "}
                    <code>sudo systemctl restart postgresql</code>
                  </li>
                  <li>
                    {t("db.slow.stepCreate")}{" "}
                    <code>CREATE EXTENSION pg_stat_statements;</code>
                  </li>
                </ol>
              </>
            )}
            {r.reason === "notInstalled" && (
              <>
                <b>{t("db.slow.pgNotInstalled", { db: r.database })}</b>
                {canEdit && (
                  <div className="db-mt">
                    <button
                      className="btn primary sm"
                      disabled={busy}
                      onClick={() =>
                        action(
                          (pw) =>
                            DatabaseService.EnableStatStatements(
                              connId,
                              target.id,
                              r.database,
                              pw,
                            ),
                          t("db.slow.enabled"),
                        )
                      }
                    >
                      <Wrench size={13} /> {t("db.slow.enable")}
                    </button>
                  </div>
                )}
              </>
            )}
            {r.reason === "psDisabled" && (
              <>
                <b>{t("db.slow.psDisabled")}</b>
                <div>
                  {t("db.slow.psHow")} <code>performance_schema = ON</code>
                </div>
              </>
            )}
            {r.reason === "error" && (
              <>
                <b>{t("db.slow.error")}</b>
                <pre>{r.detail}</pre>
              </>
            )}
          </div>
        </div>
      ) : (
        <Section
          title={t("db.slow.top", { src: r.source })}
          actions={
            <div className="row">
              <div className="segmented">
                {(["total", "mean", "calls", "max"] as Order[]).map((o) => (
                  <button
                    key={o}
                    className={order === o ? "on" : ""}
                    onClick={() => setOrder(o)}
                  >
                    {t(`db.slow.by.${o}`)}
                  </button>
                ))}
              </div>
              {canEdit && (
                <button
                  className="btn ghost sm"
                  onClick={reset}
                  disabled={busy}
                >
                  <RotateCcw size={13} /> {t("db.slow.reset")}
                </button>
              )}
              <button
                className="icon-btn"
                onClick={rep.reload}
                title={t("common.refresh")}
              >
                <RefreshCw
                  size={14}
                  className={rep.loading ? "spin-icon" : ""}
                />
              </button>
            </div>
          }
        >
          {queries.length === 0 ? (
            <Empty text={t("db.slow.none")} />
          ) : (
            <div className="table-wrap db-scroll-x">
              <table className="grid">
                <thead>
                  <tr>
                    <th>{t("db.slow.query")}</th>
                    <th className="num">{t("db.slow.calls")}</th>
                    <th className="num">{t("db.slow.total")}</th>
                    <th className="num">{t("db.slow.mean")}</th>
                    <th className="num">{t("db.slow.max")}</th>
                    <th className="num">{t("db.slow.rows")}</th>
                    <th>{t("db.act.db")}</th>
                    {pg && <th className="num">{t("db.slow.hit")}</th>}
                  </tr>
                </thead>
                <tbody>
                  {queries.map((q, i) => (
                    <tr
                      key={i}
                      onClick={() => setExpanded(expanded === i ? null : i)}
                      className="db-clickable"
                    >
                      <td className="mono db-slow-q">
                        {expanded === i ? (
                          <pre>{q.query}</pre>
                        ) : (
                          q.query.replace(/\s+/g, " ").slice(0, 180)
                        )}
                      </td>
                      <td className="num">{num(q.calls)}</td>
                      <td className="num">{ms(q.totalMs)}</td>
                      <td className={`num ${q.meanMs > 1000 ? "db-warn" : ""}`}>
                        {ms(q.meanMs)}
                      </td>
                      <td className="num">{ms(q.maxMs)}</td>
                      <td className="num">{num(q.rows)}</td>
                      <td className="mono">
                        {q.database}
                        {q.user && <div className="db-sub">{q.user}</div>}
                      </td>
                      {pg && <td className="num">{pct(q.hitRatio)}</td>}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Section>
      )}
    </>
  );
}

function SlowLogBox(props: {
  connId: string;
  targetId: string;
  s: SlowLogSettings;
  canEdit: boolean;
  onChanged: () => void;
}) {
  const t = useT();
  const [lqt, setLqt] = useState(String(props.s.longQueryTime));
  const [busy, setBusy] = useState(false);
  const [tail, setTail] = useState<string | null>(null);

  const apply = async (enabled: boolean) => {
    const v = Number(lqt);
    if (!Number.isFinite(v) || v < 0 || v > 3600) {
      toast(t("db.slow.lqtInvalid"), "error");
      return;
    }
    const ok = await confirmDanger({
      serverId: props.connId,
      title: enabled ? t("db.slow.logOnQ") : t("db.slow.logOffQ"),
      message: t("db.slow.logRuntime"),
      confirmText: t("db.slow.apply"),
    });
    if (!ok) return;
    setBusy(true);
    try {
      await dbs(props.connId, (pw) =>
        DatabaseService.SetSlowLog(
          props.connId,
          props.targetId,
          enabled,
          v,
          pw,
        ),
      );
      toast(t("db.slow.applied"), "success");
      props.onChanged();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const showTail = async () => {
    setTail("");
    try {
      setTail(
        await dbs(props.connId, (pw) =>
          DatabaseService.SlowLogTail(props.connId, props.targetId, 400, pw),
        ),
      );
    } catch (e) {
      setTail(null);
      toast(errMsg(e), "error");
    }
  };

  return (
    <div className="panel-box db-mb">
      <div className="box-title">
        <FileText size={13} /> {t("db.ov.slowLog")}
        {props.s.enabled ? (
          <StateBadge tone="ok">{t("db.on")}</StateBadge>
        ) : (
          <StateBadge tone="muted">{t("db.off")}</StateBadge>
        )}
        <div className="grow" />
        {props.s.file && (
          <button className="btn ghost sm" onClick={showTail}>
            <FileText size={13} /> {t("db.slow.viewFile")}
          </button>
        )}
      </div>
      <div className="row db-slowlog-row">
        <span className="muted">
          {t("db.slow.file")}:{" "}
          <span className="mono">{props.s.file || "–"}</span> · log_output:{" "}
          <span className="mono">{props.s.output || "–"}</span>
        </span>
        <div className="grow" />
        <label className="muted">long_query_time (s)</label>
        <input
          className="input input-sm db-w80"
          value={lqt}
          onChange={(e) => setLqt(e.target.value)}
          disabled={!props.canEdit}
        />
        {props.canEdit && (
          <>
            <button
              className="btn sm"
              disabled={busy}
              onClick={() => apply(true)}
            >
              {props.s.enabled ? t("db.slow.apply") : t("db.slow.turnOn")}
            </button>
            {props.s.enabled && (
              <button
                className="btn ghost sm"
                disabled={busy}
                onClick={() => apply(false)}
              >
                {t("db.slow.turnOff")}
              </button>
            )}
          </>
        )}
      </div>
      {tail !== null && (
        <Modal
          title={props.s.file}
          size="xwide"
          onClose={() => setTail(null)}
          footer={
            <button className="btn" onClick={() => setTail(null)}>
              {t("common.close")}
            </button>
          }
        >
          {tail === "" ? (
            <span className="spinner" />
          ) : (
            <pre className="log-view db-log">
              {tail || t("db.slow.fileEmpty")}
            </pre>
          )}
        </Modal>
      )}
    </div>
  );
}

export function RedisSlowlog({
  entries,
  error,
}: {
  entries: RedisSlow[];
  error: string;
}) {
  const t = useT();
  if (error)
    return (
      <div className="hint-box">{t("db.rd.slowlogError", { err: error })}</div>
    );
  if (entries.length === 0) return <Empty text={t("db.rd.slowlogEmpty")} />;
  return (
    <div className="table-wrap db-scroll-x">
      <table className="grid">
        <thead>
          <tr>
            <th className="num">ID</th>
            <th>{t("db.rd.time")}</th>
            <th className="num">{t("db.slow.duration")}</th>
            <th>{t("db.slow.command")}</th>
            <th>{t("db.act.client")}</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id}>
              <td className="num mono">{e.id}</td>
              <td className="db-nowrap">{formatDate(e.ts)}</td>
              <td className={`num ${e.duration > 100000 ? "db-warn" : ""}`}>
                {ms(e.duration / 1000)}
              </td>
              <td className="mono cmd">{e.command}</td>
              <td className="muted">
                {e.client}
                {e.name && <div className="db-sub">{e.name}</div>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
