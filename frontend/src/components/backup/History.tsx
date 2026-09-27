import { useMemo, useState, type ReactNode } from "react";
import { FileText, History as HistoryIcon, Lock } from "lucide-react";
import { Modal } from "../Overlays";
import { Empty, ErrorBox, KV, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { JobLog } from "../../ui/JobLog";
import { useRemote } from "../../ui/hooks";
import { formatAppError } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { useT, type Key } from "../../i18n";
import { BackupService, durationText, fmtTime, typeLabel, type Job, type Run } from "./common";
import { Select } from "../../ui/Select";

type SortKey = "started" | "duration" | "size";

export function HistoryView({ connId, jobs, enabled, tick }: { connId: string; jobs: Job[]; enabled: boolean; tick: number }) {
  const t = useT();
  const [jobId, setJobId] = useState("");
  const [status, setStatus] = useState("");
  const [kind, setKind] = useState("");
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({
    key: "started",
    desc: true,
  });
  const [open, setOpen] = useState<Run | null>(null);
  const runs = useRemote(async () => (await BackupService.History(connId, jobId, 500)) ?? [], [connId, jobId, tick], { enabled, poll: 10000 });

  const list = useMemo(() => {
    const out = (runs.data ?? []).filter((r) => (!status || r.status === status) && (!kind || r.kind === kind));
    const val = (r: Run) => (sort.key === "started" ? r.started : sort.key === "size" ? r.objectSize || r.size : r.finished ? r.finished - r.started : 0);
    out.sort((a, b) => (sort.desc ? val(b) - val(a) : val(a) - val(b)));
    return out;
  }, [runs.data, status, kind, sort]);

  const th = (key: SortKey, label: string, num = false) => (
    <th className={`sortable ${num ? "num" : ""}`} onClick={() => setSort((s) => ({ key, desc: s.key === key ? !s.desc : true }))}>
      {label} {sort.key === key ? (sort.desc ? "↓" : "↑") : ""}
    </th>
  );

  return (
    <>
      <div className="toolbar">
        <Select value={jobId} onChange={setJobId} options={[{ value: "", label: t("backup.h.allJobs") }, ...jobs.map((j) => ({ value: j.id, label: j.name }))]} />
        <Select
          value={kind}
          onChange={setKind}
          options={[
            { value: "", label: t("backup.h.allKinds") },
            { value: "backup", label: t("backup.kind.backup") },
            { value: "backup.restore", label: t("backup.kind.restore") },
          ]}
        />
        <Select
          value={status}
          onChange={setStatus}
          options={[{ value: "", label: t("backup.h.allStatus") }, ...["ok", "failed", "cancelled", "running"].map((s) => ({ value: s, label: t(`backup.status.${s}` as Key) }))]}
        />
        <div className="grow" />
        <span className="muted">{t("backup.h.count", { n: list.length })}</span>
      </div>
      {runs.error && !runs.data ? (
        <ErrorBox error={runs.error} onRetry={runs.reload} />
      ) : !runs.data ? (
        <Loading />
      ) : list.length === 0 ? (
        <Empty icon={<HistoryIcon size={28} />} title={t("backup.h.none")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                {th("started", t("backup.h.started"))}
                <th>{t("backup.h.job")}</th>
                <th>{t("backup.h.kind")}</th>
                <th>{t("backup.h.status")}</th>
                {th("duration", t("backup.h.duration"), true)}
                {th("size", t("backup.size"), true)}
                <th>{t("backup.h.by")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <tr key={r.id} className="backup-row" onClick={() => setOpen(r)}>
                  <td title={fmtTime(r.started)}>{fmtTime(r.started)}</td>
                  <td>
                    {r.jobName} <span className="muted">· {typeLabel(r.type)}</span>
                  </td>
                  <td>
                    {r.kind === "backup.restore" ? t("backup.kind.restore") : t(`backup.trigger.${r.trigger || "manual"}` as Key)}
                    {r.encrypted && <Lock size={11} className="backup-inline-icon" />}
                  </td>
                  <td>
                    <StateBadge tone={r.status === "ok" ? "ok" : r.status === "failed" ? "err" : r.status === "running" ? "info" : "muted"}>
                      {t(`backup.status.${r.status}` as Key)}
                    </StateBadge>
                  </td>
                  <td className="num">{r.status === "running" ? "…" : durationText(r.started, r.finished)}</td>
                  <td className="num">{r.objectSize ? formatBytes(r.objectSize) : "–"}</td>
                  <td className="muted">{r.actor}</td>
                  <td>
                    <button className="icon-btn" title={t("backup.h.log")}>
                      <FileText size={13} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {open && <RunDetail connId={connId} run={open} onClose={() => setOpen(null)} />}
    </>
  );
}

function RunDetail({ connId, run, onClose }: { connId: string; run: Run; onClose: () => void }) {
  const t = useT();
  const live = run.status === "running" && !!run.coreJob;
  const log = useRemote(() => BackupService.RunLog(connId, run.id), [run.id], {
    enabled: !live,
  });
  const items: [string, ReactNode][] = [
    [t("backup.h.job"), `${run.jobName} (${typeLabel(run.type)})`],
    [
      t("backup.h.status"),
      <StateBadge key="s" state={run.status === "ok" ? "success" : run.status}>
        {t(`backup.status.${run.status}` as Key)}
      </StateBadge>,
    ],
    [t("backup.h.started"), fmtTime(run.started)],
    [t("backup.h.duration"), durationText(run.started, run.finished)],
    [t("backup.h.by"), run.actor],
  ];
  if (run.location || run.key)
    items.push([
      t("backup.h.location"),
      <span key="l" className="mono backup-break">
        {run.location || run.key}
      </span>,
    ]);
  if (run.size)
    items.push([
      t("backup.size"),
      `${formatBytes(run.objectSize || run.size)}${run.compression && run.compression !== "none" ? ` · ${run.compression}` : ""}${run.encrypted ? " · age" : ""}`,
    ]);
  if (run.sha256)
    items.push([
      "SHA-256",
      <span key="h" className="mono backup-break">
        {run.sha256}
      </span>,
    ]);
  if (run.deleted) items.push([t("backup.h.deleted"), String(run.deleted)]);
  if (run.warnings?.length) items.push([t("backup.h.warnings"), run.warnings.map((w) => t(`backup.warn.${w}` as Key)).join("; ")]);
  if (run.error)
    items.push([
      t("backup.h.error"),
      <span key="e" className="err">
        {formatAppError(run.error)}
      </span>,
    ]);
  return (
    <Modal
      title={
        <>
          {run.kind === "backup.restore" ? t("backup.kind.restore") : t("backup.kind.backup")} · {run.jobName}
        </>
      }
      size="xwide"
      onClose={onClose}
    >
      <KV items={items} />
      <div className="backup-sep">{t("backup.h.log")}</div>
      {live ? (
        <JobLog jobId={run.coreJob} height="45vh" />
      ) : log.error ? (
        <div className="hint-box err">{log.error}</div>
      ) : log.data === undefined ? (
        <Loading />
      ) : (
        <pre className="log-view">{log.data || t("backup.h.noLog")}</pre>
      )}
    </Modal>
  );
}
