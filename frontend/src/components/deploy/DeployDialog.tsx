import { useEffect, useMemo, useState, type ReactNode } from "react";
import { AlertTriangle, CheckCircle2, Loader2, RotateCcw, Rocket, Unlock, XCircle } from "lucide-react";
import { DeployService } from "../../../bindings/server-manager/services/deploy";
import type { App, DeployStart, GitRef, Run } from "../../../bindings/server-manager/services/deploy/models";
import type { JobInfo } from "../../../bindings/server-manager/internal/core";
import { AppService } from "../../../bindings/server-manager/services";
import { Modal } from "../Overlays";
import { JobLog } from "../../ui/JobLog";
import { confirmDanger } from "../../ui/confirm";
import { useServer } from "../../ui/perm";
import { errMsg, formatAppError } from "../../lib/api";
import { withSudo } from "../../store/sudo";
import { confirmDialog, toast } from "../../store/ui";
import { useT } from "../../i18n";
import { LivePipeline } from "./Pipeline";
import { envPath, stepLabel, validRef, validTag } from "./util";

export type DeployMode = { kind: "deploy"; redeployOf?: Run } | { kind: "rollback"; target?: Run | null };

/** Confirms and starts a deployment or rollback, then shows it live. */
export function DeployDialog(props: { connId: string; app: App; mode: DeployMode; onClose: () => void; onChanged: () => void }) {
  const [started, setStarted] = useState<DeployStart | null>(null);
  if (started) return <LiveDeploy connId={props.connId} app={props.app} start={started} onClose={props.onClose} onChanged={props.onChanged} />;
  return <PlanDialog {...props} onStarted={(s) => (setStarted(s), props.onChanged())} />;
}

function PlanDialog({ connId, app, mode, onClose, onStarted }: { connId: string; app: App; mode: DeployMode; onClose: () => void; onStarted: (s: DeployStart) => void }) {
  const t = useT();
  const server = useServer(connId);
  const git = app.type === "git";
  const fixed = mode.kind === "rollback" ? mode.target : mode.redeployOf;
  const [ref, setRef] = useState(fixed ? fixed.data.ref || fixed.data.target : git ? app.branch : app.defaultTag);
  const [refs, setRefs] = useState<GitRef[] | null>(null);
  const [refsErr, setRefsErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!git || fixed) return;
    // Suggestions only: never prompt for sudo here.
    DeployService.RemoteRefs(connId, app.id, "")
      .then((r) => setRefs(r ?? []))
      .catch((e) => setRefsErr(errMsg(e)));
  }, [connId, app.id]);

  const refValid = fixed ? true : git ? validRef(ref.trim()) : validTag(ref.trim());
  const prod = server?.environment === "production" || app.stage === "production";
  const isRollback = mode.kind === "rollback";
  const title = isRollback ? t("deploy.rollbackTitle", { name: app.name }) : t("deploy.deployTitle", { name: app.name });

  const targetText = (r: Run) => `${r.version}${r.data.commit?.subject ? ` — ${r.data.commit.subject}` : ""}`;

  const plan = useMemo(() => {
    const items: [string, ReactNode, boolean][] = []; // step, description, skipped
    const cmdPreview = (s: string) => {
      const lines = s.split("\n").filter((l) => l.trim());
      return (
        <code className="deploy-cmd">
          {lines.slice(0, 3).join("\n")}
          {lines.length > 3 ? `\n… (+${lines.length - 3})` : ""}
        </code>
      );
    };
    const tools = git ? "git" : "docker compose";
    items.push(["preflight", t("deploy.plan.preflight", { tools, mb: app.minFreeMB || 100, dir: app.dir }), false]);
    if (git) {
      const what = fixed ? `${fixed.version}` : ref.trim() || app.branch;
      items.push(["fetch", fixed ? t("deploy.plan.checkoutExact", { ref: what }) : t("deploy.plan.fetch", { repo: app.repo, ref: what }), false]);
    } else {
      const tag = fixed ? fixed.data.target : ref.trim();
      items.push(["fetch", <>{app.type === "image" ? t("deploy.plan.genCompose", { dir: app.dir }) + " " : ""}{t("deploy.plan.setTag", { v: app.tagVar, tag })}</>, false]);
    }
    const nVars = (app.vars ?? []).length + (git ? 0 : 1);
    const nSec = (app.secrets ?? []).length;
    items.push(["env", nVars + nSec > 0 ? t("deploy.plan.env", { path: envPath(app), vars: nVars, secrets: nSec }) : t("deploy.plan.noEnv"), nVars + nSec === 0]);
    if (git) {
      items.push(["build", app.buildCmd ? cmdPreview(app.buildCmd) : t("deploy.plan.none"), !app.buildCmd]);
    } else {
      items.push(["build", <>{app.buildCmd && cmdPreview(app.buildCmd)}{app.skipPull ? t("deploy.plan.noPull") : <code className="deploy-cmd">docker compose pull</code>}</>, false]);
    }
    items.push(["test", app.testCmd ? <>{cmdPreview(app.testCmd)}<div className="hint">{t("deploy.plan.testAbort")}</div></> : t("deploy.plan.none"), !app.testCmd]);
    if (git) {
      items.push(["restart", app.restartCmd ? cmdPreview(app.restartCmd) : t("deploy.plan.none"), !app.restartCmd]);
    } else {
      items.push(["restart", <><code className="deploy-cmd">docker compose up -d --remove-orphans</code>{app.restartCmd && cmdPreview(app.restartCmd)}</>, false]);
    }
    const h = app.health;
    if (h.type === "http") {
      items.push(["health", t("deploy.plan.healthHttp", { url: h.url, status: h.expectStatus || "2xx/3xx", n: h.retries }), false]);
    } else if (h.type === "command") {
      items.push(["health", <>{cmdPreview(h.command)}<div className="hint">{t("deploy.plan.healthCmd", { n: h.retries })}</div></>, false]);
    } else {
      items.push(["health", t("deploy.plan.noHealth"), true]);
    }
    return items;
  }, [app, ref, fixed]);

  const start = async () => {
    if (!refValid) return;
    if (prod) {
      const ok = await confirmDanger({
        serverId: connId,
        title,
        message: isRollback ? t("deploy.confirmRollbackMsg", { name: app.name }) : t("deploy.confirmDeployMsg", { name: app.name, ref: fixed?.version ?? ref.trim() }),
        confirmText: isRollback ? t("deploy.rollback") : t("deploy.deploy"),
      });
      if (!ok) return;
    }
    setBusy(true);
    setError("");
    try {
      const s = await withSudo(connId, (pw) =>
        isRollback
          ? DeployService.Rollback(connId, app.id, mode.target?.id ?? "", pw)
          : DeployService.Deploy(connId, app.id, { ref: fixed ? "" : ref.trim(), redeployOf: mode.kind === "deploy" ? (mode.redeployOf?.id ?? "") : "" }, pw),
      );
      onStarted(s);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={
        <>
          {isRollback ? <RotateCcw size={16} /> : <Rocket size={16} />}
          {title}
        </>
      }
      size="wide"
      onClose={onClose}
      footer={
        <>
          {error && <span className="left err deploy-foot-err">{error}</span>}
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className={`btn ${isRollback ? "danger" : "primary"}`} disabled={busy || !refValid} onClick={start} autoFocus>
            {busy ? <span className="spinner" /> : isRollback ? <RotateCcw size={14} /> : <Rocket size={14} />}
            {isRollback ? t("deploy.rollback") : t("deploy.deploy")}
          </button>
        </>
      }
    >
      {prod && (
        <div className="deploy-prod">
          <AlertTriangle size={14} /> {t("deploy.prodWarn")}
        </div>
      )}
      {fixed ? (
        <div className="field">
          <label>{isRollback ? t("deploy.rollbackTo") : t("deploy.redeployVersion")}</label>
          <div className="deploy-target mono">{targetText(fixed)}</div>
        </div>
      ) : isRollback ? (
        <div className="field">
          <label>{t("deploy.rollbackTo")}</label>
          <div className="deploy-target">{t("deploy.rollbackPrevious")}</div>
        </div>
      ) : (
        <div className="field">
          <label>{git ? t("deploy.refLabel") : t("deploy.tagLabel")}</label>
          <input className={`input mono ${ref && !refValid ? "invalid" : ""}`} value={ref} onChange={(e) => setRef(e.target.value)} list="deploy-refs" spellCheck={false} autoFocus />
          <datalist id="deploy-refs">
            {(refs ?? []).map((r) => (
              <option key={r.kind + r.name} value={r.name}>
                {r.kind === "tag" ? t("deploy.refTag") : t("deploy.refBranch")} · {r.sha.slice(0, 7)}
              </option>
            ))}
          </datalist>
          {!refValid && <span className="error">{git ? t("deploy.v.ref") : t("deploy.v.tag")}</span>}
          {git && refs === null && !refsErr && <span className="hint">{t("deploy.refsLoading")}</span>}
          {refsErr && <span className="hint">{t("deploy.refsFailed")}</span>}
          {git && <span className="hint">{t("deploy.refHint")}</span>}
        </div>
      )}
      <div className="deploy-plan-title">{t("deploy.planTitle")}</div>
      <ol className="deploy-plan">
        {plan.map(([id, desc, skipped]) => (
          <li key={id} className={skipped ? "skipped" : ""}>
            <div className="deploy-plan-step">{stepLabel(app.type, id)}</div>
            <div className="deploy-plan-desc">{desc}</div>
          </li>
        ))}
      </ol>
      <div className="hint">{app.autoRollback && app.health.type !== "none" ? t("deploy.plan.autoRollbackOn") : t("deploy.plan.autoRollbackOff")}</div>
    </Modal>
  );
}

/** Live view of a running (or finished) deployment job. */
export function LiveDeploy({ connId, app, start, onClose, onChanged }: { connId: string; app: App; start: DeployStart; onClose: () => void; onChanged: () => void }) {
  const t = useT();
  const [info, setInfo] = useState<JobInfo | null>(null);
  const running = !info;

  const cancel = async () => {
    if (await confirmDialog(t("deploy.cancelQ"), t("deploy.cancelMsg"), t("deploy.cancelBtn"), true)) AppService.CancelJob(start.jobId);
  };

  const releaseLock = async () => {
    const ok = await confirmDanger({ serverId: connId, title: t("deploy.unlockTitle"), message: t("deploy.unlockMsg"), confirmText: t("deploy.unlock") });
    if (!ok) return;
    try {
      await withSudo(connId, (pw) => DeployService.ReleaseLock(connId, app.id, pw));
      toast(t("deploy.unlocked"), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const state = info?.state;
  return (
    <Modal
      title={
        <>
          {running ? <Loader2 size={16} className="spin-icon" /> : state === "done" ? <CheckCircle2 size={16} color="var(--ok)" /> : <XCircle size={16} color="var(--err)" />}
          {t("deploy.liveTitle", { name: app.name })}
        </>
      }
      size="xwide"
      onClose={onClose}
      footer={
        running ? (
          <>
            <span className="left muted">{t("deploy.bgHint")}</span>
            <button className="btn" onClick={onClose}>
              {t("job.background")}
            </button>
            <button className="btn danger" onClick={cancel}>
              {t("deploy.cancelBtn")}
            </button>
          </>
        ) : (
          <>
            <span className={`left job-state ${state}`}>
              {state === "done" ? t("deploy.result.ok") : state === "cancelled" ? t("job.cancelled") : formatAppError(info?.error)}
            </span>
            {info?.error?.code === "deploy.locked" && (
              <button className="btn" onClick={releaseLock}>
                <Unlock size={14} /> {t("deploy.unlock")}
              </button>
            )}
            <button className="btn primary" onClick={onClose} autoFocus>
              {t("common.close")}
            </button>
          </>
        )
      }
    >
      <div className="deploy-live">
        <LivePipeline connId={connId} jobId={start.jobId} firstRunId={start.runId} type={app.type} />
        <div className="deploy-live-log">
          <JobLog
            jobId={start.jobId}
            height="58vh"
            onDone={(i) => {
              setInfo(i);
              onChanged();
            }}
          />
        </div>
      </div>
    </Modal>
  );
}
