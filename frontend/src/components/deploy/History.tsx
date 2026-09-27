import { useMemo, useState, type ReactNode } from "react";
import { ArrowDown, ArrowUp, FileText, RefreshCw, RotateCcw, Rocket, Search } from "lucide-react";
import { DeployService } from "../../../bindings/server-manager/services/deploy";
import type { App, Run, RunDetail } from "../../../bindings/server-manager/services/deploy/models";
import { Modal } from "../Overlays";
import { Empty, ErrorBox, KV, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { formatAppError } from "../../lib/api";
import { useT } from "../../i18n";
import { StepList } from "./Pipeline";
import { fmtMs, fmtTime, runDuration, statusLabel, statusTone, triggerLabel } from "./util";

const stripAnsi = (s: string) => s.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");

type SortKey = "started" | "duration";

/** Deployment history of one app. */
export function History(props: {
  connId: string;
  app: App;
  currentTarget: string;
  canDeploy: boolean;
  reloadKey: number;
  onRedeploy: (r: Run) => void;
  onRollback: (r: Run) => void;
}) {
  const { connId, app } = props;
  const t = useT();
  const [q, setQ] = useState("");
  const [status, setStatus] = useState<"all" | "ok" | "failed">("all");
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({ key: "started", desc: true });
  const [open, setOpen] = useState<string | null>(null);
  const [hasRunning, setHasRunning] = useState(false);
  const runs = useRemote(
    async () => {
      const r = (await DeployService.Runs(connId, app.id, 200)) ?? [];
      setHasRunning(r.some((x) => x.status === "running"));
      return r;
    },
    [connId, app.id, props.reloadKey],
    { poll: hasRunning ? 2500 : undefined },
  );

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = (runs.data ?? []).filter((r) => {
      if (status === "ok" && r.status !== "ok") return false;
      if (status === "failed" && r.status === "ok") return false;
      if (!needle) return true;
      return [r.version, r.data.commit?.subject, r.data.commit?.sha, r.data.branch, r.data.ref, r.actor, r.data.trigger].some((v) => (v ?? "").toLowerCase().includes(needle));
    });
    const val = (r: Run) => (sort.key === "started" ? r.started : runDuration(r));
    return [...list].sort((a, b) => (sort.desc ? val(b) - val(a) : val(a) - val(b)));
  }, [runs.data, q, status, sort]);

  const sortBy = (key: SortKey) => setSort((s) => ({ key, desc: s.key === key ? !s.desc : true }));
  const arrow = (key: SortKey) => (sort.key === key ? sort.desc ? <ArrowDown size={11} /> : <ArrowUp size={11} /> : null);

  if (runs.error && !runs.data) return <ErrorBox error={runs.error} onRetry={runs.reload} />;
  if (!runs.data) return <Loading />;

  return (
    <div className="deploy-history">
      <div className="toolbar">
        <div className="deploy-search">
          <Search size={13} />
          <input className="input input-sm" placeholder={t("deploy.h.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <div className="segmented deploy-seg">
          {(["all", "ok", "failed"] as const).map((s) => (
            <button key={s} className={status === s ? "on" : ""} onClick={() => setStatus(s)}>
              {t(`deploy.h.f.${s}`)}
            </button>
          ))}
        </div>
        <div className="grow" />
        <button className="icon-btn" title={t("common.refresh")} onClick={runs.reload}>
          <RefreshCw size={14} className={runs.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {rows.length === 0 ? (
        <Empty title={runs.data.length ? t("deploy.h.noMatch") : t("deploy.h.empty")} text={runs.data.length ? undefined : t("deploy.h.emptyHint")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("deploy.h.status")}</th>
                <th>{t("deploy.h.version")}</th>
                <th>{t("deploy.h.ref")}</th>
                <th>{t("deploy.h.trigger")}</th>
                <th className="sortable" onClick={() => sortBy("started")}>
                  {t("deploy.h.started")} {arrow("started")}
                </th>
                <th className="num sortable" onClick={() => sortBy("duration")}>
                  {t("deploy.h.duration")} {arrow("duration")}
                </th>
                <th>{t("deploy.h.actor")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const isCurrent = r.status === "ok" && r.data.target === props.currentTarget;
                return (
                  <tr key={r.id} className="deploy-row" onDoubleClick={() => setOpen(r.id)}>
                    <td>
                      <StateBadge tone={statusTone(r.status)}>{statusLabel(r.status)}</StateBadge>
                      {isCurrent && <span className="chip deploy-current-chip">{t("deploy.h.current")}</span>}
                    </td>
                    <td className="deploy-ver-cell">
                      <span className="mono">{r.version || "—"}</span>
                      {r.data.commit?.subject && <div className="muted deploy-subject">{r.data.commit.subject}</div>}
                      {r.data.error && <div className="err deploy-subject">{formatAppError(r.data.error)}</div>}
                    </td>
                    <td className="mono">{r.data.branch || r.data.ref || ""}</td>
                    <td>
                      {triggerLabel(r.data.trigger)}
                      {r.data.rolledBackBy && <div className="muted deploy-subject">{t("deploy.h.rolledBack")}</div>}
                    </td>
                    <td title={fmtTime(r.started)}>{fmtTime(r.started)}</td>
                    <td className="num">{r.status === "running" ? <span className="spinner" /> : fmtMs(runDuration(r))}</td>
                    <td>{r.actor}</td>
                    <td className="deploy-actions-cell">
                      <button className="icon-btn" title={t("deploy.h.view")} onClick={() => setOpen(r.id)}>
                        <FileText size={14} />
                      </button>
                      {props.canDeploy && r.data.target && r.data.appType === app.type && r.status !== "running" && (
                        <button className="icon-btn" title={t("deploy.h.redeploy")} onClick={() => props.onRedeploy(r)}>
                          <Rocket size={14} />
                        </button>
                      )}
                      {props.canDeploy && r.status === "ok" && !isCurrent && r.data.appType === app.type && (
                        <button className="icon-btn" title={t("deploy.h.rollbackTo")} onClick={() => props.onRollback(r)}>
                          <RotateCcw size={14} />
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {open && (
        <RunView
          connId={connId}
          app={app}
          runId={open}
          canDeploy={props.canDeploy}
          isCurrent={(runs.data ?? []).some((r) => r.id === open && r.status === "ok" && r.data.target === props.currentTarget)}
          onClose={() => setOpen(null)}
          onRedeploy={(r) => (setOpen(null), props.onRedeploy(r))}
          onRollback={(r) => (setOpen(null), props.onRollback(r))}
        />
      )}
    </div>
  );
}

function RunView(props: {
  connId: string;
  app: App;
  runId: string;
  canDeploy: boolean;
  isCurrent: boolean;
  onClose: () => void;
  onRedeploy: (r: Run) => void;
  onRollback: (r: Run) => void;
}) {
  const t = useT();
  const d = useRemote<RunDetail>(() => DeployService.RunDetail(props.connId, props.runId), [props.runId]);
  const r = d.data?.run;
  return (
    <Modal
      title={
        <>
          <FileText size={16} />
          {t("deploy.runTitle", { name: props.app.name, version: r?.version ?? "" })}
        </>
      }
      size="xwide"
      onClose={props.onClose}
      footer={
        <>
          {r && props.canDeploy && r.data.target && r.data.appType === props.app.type && r.status !== "running" && (
            <button className="btn left" onClick={() => props.onRedeploy(r)}>
              <Rocket size={14} /> {t("deploy.h.redeploy")}
            </button>
          )}
          {r && props.canDeploy && r.status === "ok" && !props.isCurrent && r.data.appType === props.app.type && (
            <button className="btn" onClick={() => props.onRollback(r)}>
              <RotateCcw size={14} /> {t("deploy.h.rollbackTo")}
            </button>
          )}
          <button className="btn primary" onClick={props.onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      {d.error ? (
        <ErrorBox error={d.error} onRetry={d.reload} />
      ) : !r ? (
        <Loading />
      ) : (
        <div className="deploy-run">
          <div className="deploy-run-side">
            <StateBadge tone={statusTone(r.status)}>{statusLabel(r.status)}</StateBadge>
            <StepList steps={r.data.steps ?? []} type={r.data.appType} />
          </div>
          <div className="deploy-run-main">
            <KV
              items={[
                [t("deploy.h.version"), <span className="mono">{r.version}</span>],
                ...(r.data.commit
                  ? ([
                      [t("deploy.r.commit"), <span className="mono">{r.data.commit.sha}</span>],
                      [t("deploy.r.subject"), r.data.commit.subject],
                      [t("deploy.r.author"), r.data.commit.author + (r.data.commit.time ? ` · ${fmtTime(r.data.commit.time * 1000)}` : "")],
                    ] as [string, ReactNode][])
                  : []),
                [t("deploy.h.ref"), <span className="mono">{[r.data.branch || r.data.ref, r.data.refKind && t(`deploy.kind.${r.data.refKind === "branch" || r.data.refKind === "tag" || r.data.refKind === "commit" ? r.data.refKind : "image"}`)].filter(Boolean).join(" · ")}</span>],
                [t("deploy.r.previous"), <span className="mono">{r.data.previousVersion || "—"}</span>],
                [t("deploy.h.trigger"), triggerLabel(r.data.trigger)],
                [t("deploy.h.actor"), r.actor],
                [t("deploy.h.started"), fmtTime(r.started)],
                [t("deploy.h.duration"), fmtMs(runDuration(r)) || "—"],
                ...(r.data.error ? ([[t("deploy.r.error"), <span className="err">{formatAppError(r.data.error)}</span>]] as [string, ReactNode][]) : []),
                ...(r.data.rolledBackBy ? ([[t("deploy.r.rolledBackBy"), t("deploy.r.autoRolledBack")]] as [string, ReactNode][]) : []),
              ]}
            />
            <div className="deploy-plan-title">{t("deploy.r.log")}</div>
            <pre className="log-view deploy-log">{stripAnsi(d.data?.log || "") || t("deploy.r.noLog")}</pre>
          </div>
        </div>
      )}
    </Modal>
  );
}
