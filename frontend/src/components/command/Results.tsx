import { Fragment, useMemo, useState } from "react";
import { Ban, CheckCircle2, ChevronDown, ChevronRight, Clock, Copy, RotateCcw, SkipForward, Square, XCircle } from "lucide-react";
import { CommandService } from "../../../bindings/server-manager/services/command";
import type { Preview, RunInfo, ServerResult } from "../../../bindings/server-manager/services/command";
import { EnvBadge } from "../EnvBadge";
import { StateBadge, type Tone } from "../../ui/Status";
import { Bar } from "../../ui/Status";
import { toast } from "../../store/ui";
import { errMsg, formatAppError } from "../../lib/api";
import { useT } from "../../i18n";
import { useCmd } from "./store";
import { codeLabel, confirmRun, DangerList, firstLine, formatMs, formatTime, stripAnsi } from "./util";
import { presetLabel } from "./ActionPanel";

const TONE: Record<string, Tone> = {
  queued: "muted",
  connecting: "info",
  running: "info",
  done: "ok",
  failed: "err",
  timeout: "warn",
  cancelled: "muted",
  skipped: "warn",
};

type Filter = "all" | "failed" | "ok";

const isActive = (s: string) => s === "queued" || s === "connecting" || s === "running";

export function runTitle(info: { spec: { kind: string; preset: string; snippet: string; params?: Record<string, string | undefined> | null }; command: string }) {
  if (info.spec.kind === "preset") {
    const params = Object.entries(info.spec.params ?? {})
      .filter(([, v]) => v)
      .map(([k, v]) => `${k}=${v}`)
      .join(" ");
    return presetLabel(info.spec.preset) + (params ? ` (${params})` : "");
  }
  if (info.spec.snippet) return info.spec.snippet;
  return firstLine(info.command);
}

function StateCell({ r }: { r: ServerResult }) {
  const t = useT();
  return (
    <StateBadge tone={TONE[r.state] ?? "muted"}>
      {(r.state === "connecting" || r.state === "running") && <span className="spinner" />}
      {t(`cmd.state.${r.state}` as "cmd.state.done")}
    </StateBadge>
  );
}

function summaryLine(r: ServerResult): string {
  if (r.error) return formatAppError(r.error);
  return firstLine(r.stdout) || firstLine(r.stderr);
}

export function Results({ run }: { run: RunInfo }) {
  const t = useT();
  const [filter, setFilter] = useState<Filter>("all");
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const results = run.results ?? [];
  const running = run.status === "running";
  const s = run.summary;
  const bad = s.failed + s.timeout + s.skipped + s.cancelled;
  const done = s.total - s.queued - s.running;

  const shown = useMemo(
    () =>
      results.filter((r) => {
        if (filter === "ok") return r.state === "done";
        if (filter === "failed") return !isActive(r.state) && r.state !== "done";
        return true;
      }),
    [results, filter],
  );

  const cancel = async () => {
    try {
      await CommandService.Cancel(run.id);
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const rerun = async (onlyFailed: boolean) => {
    setBusy(true);
    try {
      const pv = await CommandService.PreviewRerun(run.id, onlyFailed);
      const ok = await confirmRun(pv, t(onlyFailed ? "cmd.rerunFailedTitle" : "cmd.rerunTitle"), t("cmd.run"));
      if (!ok) return;
      const id = await CommandService.Rerun(run.id, onlyFailed);
      await useCmd.getState().openRun(id);
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const copyAll = () => {
    const parts = results.map((r) => {
      const head = `===== ${r.name} — ${t(`cmd.state.${r.state}` as "cmd.state.done")}${r.state === "done" || r.state === "failed" ? ` (exit ${r.exitCode})` : ""} =====`;
      const body = [stripAnsi(r.stdout).trimEnd(), r.stderr ? `--- stderr ---\n${stripAnsi(r.stderr).trimEnd()}` : "", r.error ? `--- ${formatAppError(r.error)}` : ""]
        .filter(Boolean)
        .join("\n");
      return `${head}\n${body}`;
    });
    navigator.clipboard.writeText(parts.join("\n\n"));
    toast(t("common.copied"));
  };

  return (
    <div className="panel-box cmd-results">
      <div className="box-title">
        <span className="cmd-run-title" title={run.command}>
          {runTitle(run)}
        </span>
        <StateBadge tone={running ? "info" : run.status === "done" ? "ok" : run.status === "cancelled" || run.status === "interrupted" ? "muted" : "err"}>
          {running && <span className="spinner" />}
          {t(`cmd.status.${run.status}` as "cmd.status.done")}
        </StateBadge>
        <div className="grow" />
        {running ? (
          <button className="btn sm danger" onClick={cancel}>
            <Square size={12} /> {t("cmd.cancel")}
          </button>
        ) : (
          <>
            {bad > 0 && (
              <button className="btn sm" disabled={busy} onClick={() => rerun(true)}>
                <RotateCcw size={12} /> {t("cmd.rerunFailed")}
              </button>
            )}
            <button className="btn sm ghost" disabled={busy} onClick={() => rerun(false)}>
              <RotateCcw size={12} /> {t("cmd.rerunAll")}
            </button>
          </>
        )}
        <button className="btn sm ghost" onClick={copyAll} disabled={results.length === 0}>
          <Copy size={12} /> {t("cmd.copyAll")}
        </button>
      </div>

      <div className="cmd-meta muted">
        <span>{formatTime(run.started)}</span>
        <span>{run.actor}</span>
        {run.sudo && <span className="badge warn">sudo</span>}
        <span>{t("cmd.meta.timeout", { n: run.spec.timeoutSec })}</span>
        <span>{t("cmd.meta.concurrency", { n: run.spec.concurrency })}</span>
        {run.spec.stopOnFailure && <span>{t("cmd.opt.stopOnFailure")}</span>}
        {run.finished > 0 && <span>{t("cmd.meta.took", { d: formatMs(run.finished - run.started) })}</span>}
      </div>
      <details className="cmd-script">
        <summary>{t("cmd.showCommand")}</summary>
        <pre className="log-view">{run.command}</pre>
      </details>
      {(run.dangers ?? []).length > 0 && <DangerList dangers={run.dangers ?? []} />}

      <div className="cmd-summary">
        <span className="ok" title={t("cmd.state.done")}>
          <CheckCircle2 size={14} /> {s.ok}
        </span>
        <span className="err" title={t("cmd.state.failed")}>
          <XCircle size={14} /> {s.failed}
        </span>
        <span className="warn" title={t("cmd.state.timeout")}>
          <Clock size={14} /> {s.timeout}
        </span>
        {s.skipped > 0 && (
          <span className="warn" title={t("cmd.state.skipped")}>
            <SkipForward size={14} /> {s.skipped}
          </span>
        )}
        {s.cancelled > 0 && (
          <span className="muted" title={t("cmd.state.cancelled")}>
            <Ban size={14} /> {s.cancelled}
          </span>
        )}
        {running && (
          <span className="muted">
            <span className="spinner" /> {t("cmd.progress", { done, total: s.total })}
          </span>
        )}
        <div className="grow" />
        <div className="segmented cmd-filter">
          {(["all", "failed", "ok"] as Filter[]).map((f) => (
            <button key={f} className={filter === f ? "on" : ""} onClick={() => setFilter(f)}>
              {t(`cmd.filter.${f}` as "cmd.filter.all")}
            </button>
          ))}
        </div>
      </div>
      {running && <Bar pct={s.total ? (done / s.total) * 100 : 0} warn={101} crit={101} />}

      <div className="table-wrap cmd-table">
        <table className="grid">
          <thead>
            <tr>
              <th style={{ width: 22 }} />
              <th>{t("cmd.col.server")}</th>
              <th>{t("cmd.col.state")}</th>
              <th className="num">{t("cmd.col.exit")}</th>
              <th className="num">{t("cmd.col.duration")}</th>
              <th>{t("cmd.col.output")}</th>
            </tr>
          </thead>
          <tbody>
            {shown.length === 0 && (
              <tr>
                <td colSpan={6} className="muted cmd-empty">
                  {t("cmd.noResults")}
                </td>
              </tr>
            )}
            {shown.map((r) => {
              const expanded = !!open[r.serverId];
              const canOpen = !!(r.stdout || r.stderr || r.error);
              const dur = r.startedAt && r.finishedAt ? r.finishedAt - r.startedAt : 0;
              const line = summaryLine(r);
              return (
                <Fragment key={r.serverId}>
                  <tr className={`cmd-row ${canOpen ? "clickable" : ""}`} onClick={() => canOpen && setOpen({ ...open, [r.serverId]: !expanded })}>
                    <td>{canOpen ? expanded ? <ChevronDown size={13} /> : <ChevronRight size={13} /> : null}</td>
                    <td>
                      <span className="cmd-srv">
                        {r.name} <EnvBadge env={r.environment} short />
                      </span>
                    </td>
                    <td>
                      <StateCell r={r} />
                    </td>
                    <td className="num mono">{r.state === "done" || r.state === "failed" || r.state === "timeout" ? (r.exitCode >= 0 ? r.exitCode : "–") : ""}</td>
                    <td className="num">{dur ? formatMs(dur) : ""}</td>
                    <td className={`cmd ${r.error || r.state === "failed" ? "cmd-line-err" : ""}`} title={line}>
                      {line}
                    </td>
                  </tr>
                  {expanded && (
                    <tr className="cmd-out-row">
                      <td colSpan={6}>
                        <OutputPanel r={r} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function OutputPanel({ r }: { r: ServerResult }) {
  const t = useT();
  const [tab, setTab] = useState<"stdout" | "stderr">(r.stdout || !r.stderr ? "stdout" : "stderr");
  const text = stripAnsi(tab === "stdout" ? r.stdout : r.stderr);
  const copy = () => {
    navigator.clipboard.writeText(text);
    toast(t("common.copied"));
  };
  return (
    <div className="cmd-output">
      {r.error && (
        <div className="cmd-hint err">
          {formatAppError(r.error)}
          {r.error.code === "auth.needSecret" && <div className="muted">{t("cmd.ready.needSecretHint")}</div>}
          {r.error.code === "sudo.required" && <div className="muted">{t("cmd.sudoRequiredHint")}</div>}
        </div>
      )}
      <div className="cmd-output-tabs">
        <div className="segmented">
          <button className={tab === "stdout" ? "on" : ""} onClick={() => setTab("stdout")}>
            stdout {r.stdout ? `(${r.stdout.split("\n").filter(Boolean).length})` : ""}
          </button>
          <button className={tab === "stderr" ? "on" : ""} onClick={() => setTab("stderr")}>
            stderr {r.stderr ? `(${r.stderr.split("\n").filter(Boolean).length})` : ""}
          </button>
        </div>
        {r.truncated && <span className="badge warn">{t("cmd.truncated")}</span>}
        <div className="grow" />
        <button className="btn sm ghost" onClick={copy} disabled={!text}>
          <Copy size={12} /> {t("cmd.copy")}
        </button>
      </div>
      <pre className={`log-view cmd-log ${tab === "stderr" ? "err" : ""}`}>{text || <span className="muted">{t("cmd.noOutput")}</span>}</pre>
    </div>
  );
}

/** Dry-run result: what would run where. */
export function PreviewView({ pv, onClose }: { pv: Preview; onClose: () => void }) {
  const t = useT();
  const servers = pv.servers ?? [];
  const skip = servers.filter((s) => s.action === "skip").length;
  return (
    <div className="panel-box cmd-results">
      <div className="box-title">
        <span>{t("cmd.dryRunTitle")}</span>
        <span className="badge">{t("cmd.dryRunBadge")}</span>
        <div className="grow" />
        <button className="btn sm ghost" onClick={onClose}>
          {t("common.close")}
        </button>
      </div>
      <div className="cmd-meta muted">
        <span>{t("cmd.dryRunSummary", { run: servers.length - skip, skip })}</span>
        {pv.sudo && <span className="badge warn">sudo</span>}
        <span>{t("cmd.meta.timeout", { n: pv.timeoutSec })}</span>
        <span>{t("cmd.meta.concurrency", { n: pv.concurrency })}</span>
      </div>
      <div className="field">
        <label>{t("cmd.finalCommand")}</label>
        <pre className="log-view cmd-rendered">{pv.command}</pre>
      </div>
      <DangerList dangers={pv.dangers ?? []} />
      <div className="table-wrap cmd-table">
        <table className="grid">
          <thead>
            <tr>
              <th>{t("cmd.col.server")}</th>
              <th>{t("cmd.col.action")}</th>
              <th>{t("cmd.col.via")}</th>
              <th>{t("cmd.col.sudo")}</th>
              <th>{t("cmd.col.exact")}</th>
            </tr>
          </thead>
          <tbody>
            {servers.map((s) => (
              <tr key={s.serverId}>
                <td>
                  <span className="cmd-srv">
                    {s.name} <EnvBadge env={s.environment} short />
                  </span>
                </td>
                <td>
                  {s.action === "run" ? (
                    <StateBadge tone="ok">{t("cmd.willRun")}</StateBadge>
                  ) : (
                    <StateBadge tone="warn">
                      {t("cmd.willSkip")}: {codeLabel(s.reason)}
                    </StateBadge>
                  )}
                </td>
                <td className="muted">{s.via ? t(`cmd.via.${s.via}` as "cmd.via.ui") : ""}</td>
                <td className="muted">{pv.sudo ? t(`cmd.sudo.${s.sudo}` as "cmd.sudo.root") : "–"}</td>
                <td className="cmd">
                  <details>
                    <summary>{t("cmd.show")}</summary>
                    <pre className="log-view cmd-exact">{s.command}</pre>
                  </details>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
