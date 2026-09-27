import { useEffect, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { Check, Circle, Loader2, Minus, X } from "lucide-react";
import { DeployService } from "../../../bindings/server-manager/services/deploy";
import type { Step, StepEvent } from "../../../bindings/server-manager/services/deploy/models";
import { useInterval } from "../../ui/hooks";
import { useT } from "../../i18n";
import { emptySteps, fmtMs, stepLabel, triggerLabel } from "./util";

export function StepIcon({ state }: { state: string }) {
  switch (state) {
    case "ok":
      return <Check size={14} className="deploy-si ok" />;
    case "failed":
      return <X size={14} className="deploy-si err" />;
    case "running":
      return <Loader2 size={14} className="deploy-si run spin-icon" />;
    case "skipped":
      return <Minus size={14} className="deploy-si skip" />;
    default:
      return <Circle size={12} className="deploy-si pend" />;
  }
}

/** Static list of steps with their state and duration. */
export function StepList({ steps, type, now }: { steps: Step[]; type: string; now?: number }) {
  const t = useT();
  return (
    <ol className="deploy-steps">
      {steps.map((s) => {
        const dur = s.state === "running" && now ? now - s.startedAt : s.finishedAt && s.startedAt ? s.finishedAt - s.startedAt : 0;
        return (
          <li key={s.id} className={`deploy-step ${s.state}`}>
            <StepIcon state={s.state} />
            <span className="deploy-step-label">{stepLabel(type, s.id)}</span>
            <span className="deploy-step-dur">{s.state === "skipped" ? t("deploy.skipped") : fmtMs(dur)}</span>
          </li>
        );
      })}
    </ol>
  );
}

interface LiveRunState {
  runId: string;
  trigger: string;
  version: string;
  steps: Step[];
}

/**
 * Live pipeline of every run of a deployment job (the deployment and, if it
 * happens, its automatic rollback), fed by "deploy:step" events.
 */
export function LivePipeline({ connId, jobId, firstRunId, type }: { connId: string; jobId: string; firstRunId: string; type: string }) {
  const t = useT();
  const [runs, setRuns] = useState<LiveRunState[]>([{ runId: firstRunId, trigger: "", version: "", steps: emptySteps() }]);
  const touched = useRef(new Set<string>()); // run/step keys set by events
  const [now, setNow] = useState(Date.now());
  const running = runs.some((r) => r.steps.some((s) => s.state === "running"));
  useInterval(() => setNow(Date.now()), running ? 500 : null);

  const load = (runId: string) => {
    DeployService.RunDetail(connId, runId)
      .then((d) => {
        setRuns((prev) =>
          prev.map((r) => {
            if (r.runId !== runId) return r;
            const steps = r.steps.map((s) => {
              if (touched.current.has(runId + "/" + s.id)) return s;
              return (d.run.data.steps ?? []).find((x) => x.id === s.id) ?? s;
            });
            return { ...r, trigger: d.run.data.trigger, version: d.run.version, steps };
          }),
        );
      })
      .catch(() => {
        /* the run row appears a moment after the event; events still update */
      });
  };

  useEffect(() => {
    load(firstRunId);
    const off = Events.On("deploy:step", (ev) => {
      const e: StepEvent = ev.data;
      if (e.jobId !== jobId && e.runId !== firstRunId) return;
      touched.current.add(e.runId + "/" + e.step);
      if (e.step === "done" && (e.state === "ok" || e.state === "failed")) setTimeout(() => load(e.runId), 300);
      setRuns((prev) => {
        let list = prev;
        if (!list.some((r) => r.runId === e.runId)) {
          list = [...list, { runId: e.runId, trigger: "", version: "", steps: emptySteps() }];
          setTimeout(() => load(e.runId), 300);
        }
        return list.map((r) =>
          r.runId !== e.runId
            ? r
            : { ...r, steps: r.steps.map((s) => (s.id === e.step ? { ...s, state: e.state, startedAt: e.startedAt, finishedAt: e.finishedAt, note: e.note } : s)) },
        );
      });
    });
    return () => off();
  }, [jobId, firstRunId]);

  return (
    <div className="deploy-pipeline">
      {runs.map((r, i) => (
        <div key={r.runId} className="deploy-pipeline-run">
          {(i > 0 || runs.length > 1) && (
            <div className="deploy-pipeline-title">
              {i === 0 ? t("deploy.pipeline.deploy") : triggerLabel(r.trigger || "auto-rollback")}
              {r.version && <span className="mono muted"> · {r.version}</span>}
            </div>
          )}
          <StepList steps={r.steps} type={type} now={now} />
        </div>
      ))}
    </div>
  );
}
