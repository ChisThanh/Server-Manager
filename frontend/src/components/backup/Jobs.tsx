import { useMemo, useState } from "react";
import { Archive, CalendarClock, Eye, Lock, Pencil, Play, Plus, RotateCcw, Trash2, Warehouse } from "lucide-react";
import type { Remote } from "../../ui/hooks";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { StateBadge, type Tone } from "../../ui/Status";
import { runJob, watchJob } from "../../ui/jobs";
import { confirmDanger } from "../../ui/confirm";
import { withSudo } from "../../store/sudo";
import { toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { useT, type Key } from "../../i18n";
import {
  BackupService,
  TypeIcon,
  DestIcon,
  ago,
  fmtTime,
  freshnessTone,
  inFuture,
  jobTarget,
  scheduleText,
  typeLabel,
  type Destination,
  type Job,
  type JobStatus,
} from "./common";
import { BackupsBrowser } from "./Restore";

export function JobsView(props: {
  connId: string;
  jobs: Remote<Job[]>;
  status: JobStatus[];
  dests: Destination[];
  canBackup: boolean;
  canRestore: boolean;
  onEdit: (job: Partial<Job> | null) => void;
  onChanged: () => void;
  goDestinations: () => void;
}) {
  const t = useT();
  const { connId, jobs, status, dests, canBackup } = props;
  const [filter, setFilter] = useState("");
  const [browse, setBrowse] = useState<Job | null>(null);
  const byId = useMemo(() => new Map(status.map((s) => [s.jobId, s])), [status]);
  const destById = useMemo(() => new Map(dests.map((d) => [d.id, d])), [dests]);

  const list = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return (jobs.data ?? []).filter((j) => !q || j.name.toLowerCase().includes(q) || jobTarget(j).toLowerCase().includes(q) || j.type.includes(q));
  }, [jobs.data, filter]);

  if (jobs.error && !jobs.data) return <ErrorBox error={jobs.error} onRetry={jobs.reload} />;
  if (!jobs.data) return <Loading />;

  const runNow = async (j: Job) => {
    const info = await runJob(t("backup.runTitle", { name: j.name }), () => withSudo(connId, (pw) => BackupService.RunNow(connId, j.id, pw)));
    if (info) props.onChanged();
  };

  const remove = async (j: Job) => {
    const ok = await confirmDanger({
      serverId: connId,
      title: t("backup.job.deleteTitle", { name: j.name }),
      message: t("backup.job.deleteMsg"),
      confirmText: t("backup.delete"),
    });
    if (!ok) return;
    try {
      await BackupService.DeleteJob(connId, j.id);
      toast(t("backup.job.deleted", { name: j.name }), "success");
      props.onChanged();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const noDest = dests.length === 0;

  return (
    <>
      <div className="toolbar">
        {(jobs.data.length > 0 || filter) && (
          <input className="input input-sm" placeholder={t("backup.filter")} value={filter} onChange={(e) => setFilter(e.target.value)} />
        )}
        <div className="grow" />
        {canBackup && (
          <button className="btn primary sm" onClick={() => props.onEdit(null)} disabled={noDest} title={noDest ? t("backup.needDestination") : undefined}>
            <Plus size={13} /> {t("backup.job.new")}
          </button>
        )}
      </div>
      {!canBackup && <div className="hint-box">{t("backup.readOnly")}</div>}
      {noDest && canBackup && (
        <div className="hint-box backup-hint-row">
          <Warehouse size={14} />
          <span>{t("backup.needDestination")}</span>
          <div className="grow" />
          <button className="btn sm" onClick={props.goDestinations}>
            {t("backup.dest.add")}
          </button>
        </div>
      )}
      {jobs.data.length === 0 ? (
        <Empty
          icon={<Archive size={28} />}
          title={t("backup.job.none")}
          text={t("backup.job.noneText")}
          action={
            canBackup && !noDest ? (
              <button className="btn primary" onClick={() => props.onEdit(null)}>
                <Plus size={14} /> {t("backup.job.new")}
              </button>
            ) : undefined
          }
        />
      ) : list.length === 0 ? (
        <Empty title={t("backup.noMatch")} />
      ) : (
        <div className="backup-cards">
          {list.map((j) => {
            const st = byId.get(j.id);
            const dest = destById.get(j.destination);
            return (
              <JobCard
                key={j.id}
                job={j}
                st={st}
                dest={dest}
                canBackup={canBackup}
                canRestore={props.canRestore}
                onRun={() => runNow(j)}
                onView={() => st?.runningJob && watchJob(st.runningJob, t("backup.runTitle", { name: j.name })).then(() => props.onChanged())}
                onRestore={() => setBrowse(j)}
                onEdit={() => props.onEdit(j)}
                onDelete={() => remove(j)}
              />
            );
          })}
        </div>
      )}
      {browse && <BackupsBrowser connId={connId} job={browse} canRestore={props.canRestore} canBackup={canBackup} onClose={() => setBrowse(null)} />}
    </>
  );
}

function JobCard(props: {
  job: Job;
  st?: JobStatus;
  dest?: Destination;
  canBackup: boolean;
  canRestore: boolean;
  onRun: () => void;
  onView: () => void;
  onRestore: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const t = useT();
  const { job: j, st, dest } = props;
  const tone: Tone = freshnessTone(
    st ??
      ({
        lastStatus: "",
        lastSuccess: 0,
        cron: j.schedule?.cron ?? "",
      } as JobStatus),
  );
  return (
    <div className={`backup-card ${!j.enabled ? "disabled" : ""}`}>
      <div className="backup-card-head">
        <div className={`backup-type-icon t-${j.type}`}>
          <TypeIcon type={j.type} />
        </div>
        <div className="backup-card-title">
          <div className="name" title={j.name}>
            {j.name}
          </div>
          <div className="muted backup-ellipsis" title={jobTarget(j)}>
            {typeLabel(j.type)} · {jobTarget(j)}
          </div>
        </div>
        {st?.running ? (
          <StateBadge tone="info">{t("backup.status.running")}</StateBadge>
        ) : st?.lastStatus ? (
          <StateBadge tone={st.lastStatus === "ok" ? "ok" : st.lastStatus === "failed" ? "err" : "muted"}>
            {t(`backup.status.${st.lastStatus}` as Key)}
          </StateBadge>
        ) : (
          <StateBadge tone="muted">{t("backup.status.never")}</StateBadge>
        )}
      </div>
      <div className="backup-card-grid">
        <div>
          <div className="k">{t("backup.lastSuccess")}</div>
          <div className={`v tone-${tone}`} title={fmtTime(st?.lastSuccess ?? 0)}>
            {st?.lastSuccess ? ago(st.lastSuccess) : t("backup.never")}
          </div>
        </div>
        <div>
          <div className="k">{t("backup.size")}</div>
          <div className="v">{st?.lastSize ? formatBytes(st.lastSize) : "–"}</div>
        </div>
        <div>
          <div className="k">
            <CalendarClock size={11} /> {t("backup.schedule")}
          </div>
          <div className="v" title={j.schedule?.cron || ""}>
            {j.enabled ? scheduleText(j.schedule) : t("backup.disabled")}
          </div>
        </div>
        <div>
          <div className="k">{t("backup.nextRun")}</div>
          <div className="v" title={fmtTime(st?.nextRun ?? 0)}>
            {st?.nextRun ? inFuture(st.nextRun) : "–"}
          </div>
        </div>
      </div>
      {st?.lastStatus === "failed" && st.lastFailure > (st.lastSuccess ?? 0) && (
        <div className="backup-card-err" title={st.lastError?.detail ?? ""}>
          {t("backup.lastFailed", { ago: ago(st.lastFailure) })}
        </div>
      )}
      <div className="backup-card-meta">
        <span className="chip" title={dest ? dest.name : t("backup.dest.missing")}>
          <DestIcon type={dest?.type ?? ""} size={11} /> {dest ? dest.name : t("backup.dest.missing")}
        </span>
        {j.encrypt && (
          <span className="chip ok" title={t("backup.encrypted")}>
            <Lock size={11} /> age
          </span>
        )}
        {j.compression !== "none" && <span className="chip">{j.compression}</span>}
        {(j.retention?.keepLast > 0 || j.retention?.maxAgeDays > 0) && (
          <span className="chip" title={t("backup.retention")}>
            {[
              j.retention.keepLast > 0 ? t("backup.keepN", { n: j.retention.keepLast }) : "",
              j.retention.maxAgeDays > 0 ? t("backup.maxDays", { n: j.retention.maxAgeDays }) : "",
            ]
              .filter(Boolean)
              .join(" · ")}
          </span>
        )}
      </div>
      <div className="backup-card-actions">
        {st?.running ? (
          <button className="btn sm" onClick={props.onView}>
            <Eye size={13} /> {t("backup.viewRun")}
          </button>
        ) : (
          props.canBackup && (
            <button className="btn sm primary" onClick={props.onRun}>
              <Play size={13} /> {t("backup.runNow")}
            </button>
          )
        )}
        <button className="btn sm" onClick={props.onRestore}>
          <RotateCcw size={13} /> {props.canRestore ? t("backup.restore") : t("backup.browse")}
        </button>
        <div className="grow" />
        {props.canBackup && (
          <>
            <button className="icon-btn" onClick={props.onEdit} title={t("backup.edit")}>
              <Pencil size={14} />
            </button>
            <button className="icon-btn" onClick={props.onDelete} title={t("backup.delete")} disabled={st?.running}>
              <Trash2 size={14} />
            </button>
          </>
        )}
      </div>
    </div>
  );
}
