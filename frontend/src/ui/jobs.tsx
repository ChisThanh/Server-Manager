import { Suspense, lazy, useState } from "react";
import { create } from "zustand";
import { CheckCircle2, Loader2, XCircle } from "lucide-react";
import { AppService } from "../../bindings/server-manager/services";
import type { JobInfo } from "../../bindings/server-manager/internal/core";
import { Modal } from "../components/Overlays";
import { errMsg, formatAppError } from "../lib/api";
import { t, useT } from "../i18n";
import { toast } from "../store/ui";
// xterm is only loaded when a job is shown.
const JobLog = lazy(() => import("./JobLog").then((m) => ({ default: m.JobLog })));

interface OpenJob {
  id: string;
  title: string;
  resolve: (info: JobInfo | null) => void;
  hidden: boolean;
}

const useJobModals = create<{ jobs: OpenJob[] }>(() => ({ jobs: [] }));

/**
 * Starts a backend job and shows its live output in a modal. Resolves with
 * the finished job (state done/error/cancelled), or null if it couldn't start.
 * The user may hide the modal; the job keeps running and the promise still
 * resolves when it ends.
 */
export async function runJob(title: string, start: () => Promise<string>): Promise<JobInfo | null> {
  let id: string;
  try {
    id = await start();
  } catch (e) {
    toast(errMsg(e), "error");
    return null;
  }
  return watchJob(id, title);
}

/** Shows an already started job. */
export function watchJob(id: string, title: string): Promise<JobInfo | null> {
  return new Promise((resolve) => {
    useJobModals.setState((s) => ({ jobs: [...s.jobs, { id, title, resolve, hidden: false }] }));
  });
}

function JobModal({ job }: { job: OpenJob }) {
  const tr = useT();
  const [info, setInfo] = useState<JobInfo | null>(null);
  const running = !info;
  const close = () => {
    useJobModals.setState((s) => ({ jobs: s.jobs.filter((j) => j.id !== job.id) }));
  };
  const hide = () => useJobModals.setState((s) => ({ jobs: s.jobs.map((j) => (j.id === job.id ? { ...j, hidden: true } : j)) }));
  const onDone = (i: JobInfo) => {
    setInfo(i);
    job.resolve(i);
    if (job.hidden) {
      close();
      if (i.state === "done") toast(t("job.doneToast", { title: job.title }), "success");
      else if (i.state === "error") toast(`${job.title}: ${formatAppError(i.error)}`, "error");
    }
  };
  return (
    <div style={{ display: job.hidden ? "none" : undefined }}>
      <Modal
        title={
          <>
            {running ? <Loader2 size={16} className="spin-icon" /> : info?.state === "done" ? <CheckCircle2 size={16} color="var(--ok)" /> : <XCircle size={16} color="var(--err)" />}
            {job.title}
          </>
        }
        size="xwide"
        onClose={running ? hide : close}
        footer={
          running ? (
            <>
              <button className="btn left" onClick={hide}>
                {tr("job.background")}
              </button>
              <button className="btn danger" onClick={() => AppService.CancelJob(job.id)}>
                {tr("job.cancel")}
              </button>
            </>
          ) : (
            <>
              <span className={`left job-state ${info?.state}`}>
                {info?.state === "done" ? tr("job.success") : info?.state === "cancelled" ? tr("job.cancelled") : `${tr("job.failed")}: ${formatAppError(info?.error)}`}
              </span>
              <button className="btn primary" onClick={close} autoFocus>
                {tr("common.close")}
              </button>
            </>
          )
        }
      >
        <Suspense fallback={<div className="job-log" style={{ height: "55vh" }} />}>
          <JobLog jobId={job.id} height="55vh" onDone={onDone} />
        </Suspense>
      </Modal>
    </div>
  );
}

/** Mount once (App). */
export function JobModals() {
  const jobs = useJobModals((s) => s.jobs);
  return (
    <>
      {jobs.map((j) => (
        <JobModal key={j.id} job={j} />
      ))}
    </>
  );
}
