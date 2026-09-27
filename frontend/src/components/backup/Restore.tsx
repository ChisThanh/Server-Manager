import { useMemo, useState } from "react";
import { AlertTriangle, Download, FileArchive, Lock, RotateCcw, ShieldCheck, Trash2 } from "lucide-react";
import { Modal } from "../Overlays";
import { Empty, ErrorBox, KV, Loading } from "../../ui/Page";
import { useRemote } from "../../ui/hooks";
import { runJob, watchJob } from "../../ui/jobs";
import { confirmDanger } from "../../ui/confirm";
import { withSudo } from "../../store/sudo";
import { toast, useUI } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { useT } from "../../i18n";
import { BackupService, Field, RE_DB, RE_VOLUME, TypeIcon, ago, fmtTime, isAbsPath, typeLabel, type BackupItem, type Job, type RestoreOptions } from "./common";

/** Lists the stored backups of a job with restore/verify/download/delete. */
export function BackupsBrowser({
  connId,
  job,
  canRestore,
  canBackup,
  onClose,
}: {
  connId: string;
  job: Job;
  canRestore: boolean;
  canBackup: boolean;
  onClose: () => void;
}) {
  const t = useT();
  const list = useRemote(async () => (await withSudo(connId, (pw) => BackupService.ListBackups(connId, job.id, pw))) ?? [], [connId, job.id]);
  const [restoring, setRestoring] = useState<BackupItem | null>(null);

  const verify = (b: BackupItem) =>
    runJob(t("backup.verifyTitle", { name: b.name }), () => withSudo(connId, (pw) => BackupService.Verify(connId, job.id, b.key, "", pw)));

  const download = async (b: BackupItem) => {
    let decrypt = false;
    if (b.encrypted) {
      const r = await useUI.getState().openDialog({
        title: t("backup.dl.title"),
        message: t("backup.dl.message"),
        confirmText: t("backup.dl.decrypted"),
        extra: [{ id: "raw", label: t("backup.dl.raw") }],
      });
      if (!r || (r.action !== "ok" && r.action !== "raw")) return;
      decrypt = r.action === "ok";
    }
    try {
      const id = await withSudo(connId, (pw) => BackupService.Download(connId, job.id, b.key, decrypt, "", pw));
      if (id) watchJob(id, t("backup.dl.jobTitle", { name: b.name }));
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const remove = async (b: BackupItem) => {
    const ok = await confirmDanger({
      serverId: connId,
      title: t("backup.b.deleteTitle"),
      message: b.name,
      confirmText: t("backup.delete"),
    });
    if (!ok) return;
    try {
      await withSudo(connId, (pw) => BackupService.DeleteBackup(connId, job.id, b.key, pw));
      toast(t("backup.b.deleted"), "success");
      list.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <Modal
      size="xwide"
      title={
        <>
          <TypeIcon type={job.type} /> {t("backup.b.title", { name: job.name })}
        </>
      }
      onClose={onClose}
      footer={
        <>
          <span className="left muted">{list.data ? t("backup.b.count", { n: list.data.length }) : ""}</span>
          <button className="btn" onClick={list.reload} disabled={list.loading}>
            {t("backup.refresh")}
          </button>
          <button className="btn primary" onClick={onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      {list.error && !list.data ? (
        <ErrorBox error={list.error} onRetry={list.reload} />
      ) : !list.data ? (
        <Loading label={t("backup.b.loading")} />
      ) : list.data.length === 0 ? (
        <Empty icon={<FileArchive size={28} />} title={t("backup.b.none")} text={t("backup.b.noneText")} />
      ) : (
        <div className="table-wrap backup-browser">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("backup.b.time")}</th>
                <th>{t("backup.b.file")}</th>
                <th className="num">{t("backup.size")}</th>
                <th>SHA-256</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data.map((b) => (
                <tr key={b.key}>
                  <td title={ago(b.time)}>{fmtTime(b.time)}</td>
                  <td className="cmd" title={b.key}>
                    {b.encrypted && <Lock size={11} className="backup-inline-icon" />} {b.name}
                    {!b.complete && <span className="badge warn backup-badge">{t("backup.b.incomplete")}</span>}
                  </td>
                  <td className="num">{formatBytes(b.size)}</td>
                  <td className="cmd muted" title={b.manifest?.sha256 ?? ""}>
                    {b.manifest ? b.manifest.sha256.slice(0, 12) + "…" : "–"}
                  </td>
                  <td className="backup-actions-cell">
                    {canRestore && (
                      <button className="btn sm primary" disabled={!b.complete} onClick={() => setRestoring(b)}>
                        <RotateCcw size={13} /> {t("backup.restore")}
                      </button>
                    )}
                    <button className="icon-btn" title={t("backup.verify")} disabled={!b.complete} onClick={() => verify(b)}>
                      <ShieldCheck size={14} />
                    </button>
                    {canBackup && (
                      <button className="icon-btn" title={t("backup.download")} disabled={!b.complete} onClick={() => download(b)}>
                        <Download size={14} />
                      </button>
                    )}
                    {canBackup && (
                      <button className="icon-btn" title={t("backup.delete")} onClick={() => remove(b)}>
                        <Trash2 size={14} />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {restoring && <RestoreDialog connId={connId} job={job} item={restoring} onClose={() => setRestoring(null)} />}
    </Modal>
  );
}

function stamp() {
  const d = new Date();
  const p = (x: number) => String(x).padStart(2, "0");
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

/** Restore wizard: options per backup type, then a typed confirmation. */
function RestoreDialog({ connId, job, item, onClose }: { connId: string; job: Job; item: BackupItem; onClose: () => void }) {
  const t = useT();
  const m = item.manifest;
  const type = m?.type ?? job.type;
  const format = m?.dumpFormat ?? "";
  const [o, setO] = useState<RestoreOptions>(() => ({
    jobId: job.id,
    key: item.key,
    target: type === "custom" && job.restoreCommand ? "command" : type === "redis" ? "staging" : "staging",
    dir: `/tmp/restore-${stamp()}`,
    database: m?.database || job.database || "",
    createDb: false,
    volume: m?.volume || job.volume || "",
    clearVolume: false,
    stopContainers: job.stopContainers,
    passphrase: "",
    tempDir: "/var/tmp",
  }));
  const [otherPass, setOtherPass] = useState(false);
  const [touched, setTouched] = useState(false);
  const set = <K extends keyof RestoreOptions>(k: K, v: RestoreOptions[K]) => setO((x) => ({ ...x, [k]: v }));

  const needsDir = (type === "files" || type === "redis" || type === "custom") && o.target === "staging";
  const needsDB = (type === "postgres" && format === "pg-custom") || (type === "mysql" && !!m?.database);
  const errors = useMemo(() => {
    const e: Record<string, string> = {};
    if (needsDir && (!isAbsPath(o.dir.trim()) || o.dir.trim() === "/")) e.dir = t("backup.v.absPath");
    if (needsDB && !RE_DB.test(o.database)) e.database = t("backup.v.dbName");
    if (type === "volume" && !RE_VOLUME.test(o.volume)) e.volume = t("backup.v.volume");
    if (!isAbsPath(o.tempDir)) e.tempDir = t("backup.v.absPath");
    if (otherPass && !o.passphrase) e.passphrase = t("backup.v.required");
    return e;
  }, [o, needsDir, needsDB, type, otherPass, t]);
  const valid = Object.keys(errors).length === 0;
  const err = (k: string) => (touched ? (errors[k] ?? null) : null);

  const describe = (): string => {
    switch (type) {
      case "files":
        return o.target === "original" ? t("backup.r.sumFilesOrig") : t("backup.r.sumFilesStaging", { dir: o.dir });
      case "postgres":
        return format === "pg-custom" ? t("backup.r.sumPg", { db: o.database }) : t("backup.r.sumPgAll");
      case "mysql":
        return m?.database ? t("backup.r.sumMy", { db: o.database }) : t("backup.r.sumMyAll");
      case "volume":
        return t("backup.r.sumVol", { vol: o.volume });
      case "redis":
        return o.target === "replace" ? t("backup.r.sumRedisReplace") : t("backup.r.sumStaging", { dir: o.dir });
      case "custom":
        return o.target === "command" ? t("backup.r.sumCommand") : t("backup.r.sumStaging", { dir: o.dir });
    }
    return "";
  };

  const start = async () => {
    setTouched(true);
    if (!valid) return;
    const ok = await confirmDanger({
      serverId: connId,
      title: t("backup.r.confirmTitle", { name: job.name }),
      message: (
        <>
          <b>{item.name}</b> ({fmtTime(item.time)})
          <br />
          {describe()}
        </>
      ),
      confirmText: t("backup.restore"),
    });
    if (!ok) return;
    const opts: RestoreOptions = {
      ...o,
      dir: o.dir.trim(),
      passphrase: otherPass ? o.passphrase : "",
    };
    const info = await runJob(t("backup.r.jobTitle", { name: job.name }), () => withSudo(connId, (pw) => BackupService.Restore(connId, opts, pw)));
    if (info?.state === "done") onClose();
  };

  const destructive =
    (type === "files" && o.target === "original") ||
    type === "postgres" ||
    type === "mysql" ||
    type === "volume" ||
    (type === "redis" && o.target === "replace") ||
    o.target === "command";

  return (
    <Modal
      size="wide"
      title={
        <>
          <RotateCcw size={16} /> {t("backup.r.title")}
        </>
      }
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className={`btn ${destructive ? "danger" : "primary"}`} onClick={start} disabled={touched && !valid}>
            <RotateCcw size={14} /> {t("backup.restore")}
          </button>
        </>
      }
    >
      <div className="backup-form">
        <KV
          items={[
            [
              t("backup.b.file"),
              <span key="f" className="mono">
                {item.name}
              </span>,
            ],
            [t("backup.b.time"), `${fmtTime(item.time)} (${ago(item.time)})`],
            [t("backup.f.type"), typeLabel(type) + (m?.database ? ` · ${m.database}` : m?.volume ? ` · ${m.volume}` : "")],
            [
              t("backup.size"),
              `${formatBytes(m?.size ?? item.size)}${m?.compression && m.compression !== "none" ? ` · ${m.compression}` : ""}${m?.encrypted ? " · age" : ""}`,
            ],
            [
              "SHA-256",
              <span key="h" className="mono backup-break">
                {m?.sha256 ?? "–"}
              </span>,
            ],
            ...(m?.tools && Object.keys(m.tools).length ? ([[t("backup.r.tools"), Object.values(m.tools).join("; ")]] as [string, string][]) : []),
          ]}
        />
        {!m && <div className="hint-box warn">{t("backup.r.noManifestLoaded")}</div>}
        <div className="hint-box backup-hint-row">
          <ShieldCheck size={14} /> {t("backup.r.verifyFirst")}
        </div>

        {type === "files" && (
          <>
            <div className="segmented">
              <button className={o.target === "staging" ? "on" : ""} onClick={() => set("target", "staging")}>
                {t("backup.r.staging")}
              </button>
              <button className={o.target === "original" ? "on" : ""} onClick={() => set("target", "original")}>
                {t("backup.r.original")}
              </button>
            </div>
            {o.target === "original" && (
              <div className="hint-box err backup-hint-row">
                <AlertTriangle size={14} />{" "}
                {t("backup.r.overwriteWarn", {
                  paths: (m?.paths ?? job.paths ?? []).join(", "),
                })}
              </div>
            )}
          </>
        )}
        {type === "redis" && (
          <>
            <div className="segmented">
              <button className={o.target === "staging" ? "on" : ""} onClick={() => set("target", "staging")}>
                {t("backup.r.redisStaging")}
              </button>
              <button className={o.target === "replace" ? "on" : ""} onClick={() => set("target", "replace")}>
                {t("backup.r.redisReplace")}
              </button>
            </div>
            <div className={`hint-box ${o.target === "replace" ? "warn" : ""}`}>
              {o.target === "replace" ? t("backup.r.redisReplaceHint") : t("backup.r.redisHowTo")}
            </div>
          </>
        )}
        {type === "custom" && (
          <div className="segmented">
            <button className={o.target === "staging" ? "on" : ""} onClick={() => set("target", "staging")}>
              {t("backup.r.staging")}
            </button>
            <button
              className={o.target === "command" ? "on" : ""}
              disabled={!job.restoreCommand}
              onClick={() => set("target", "command")}
              title={job.restoreCommand || t("backup.r.noRestoreCommand")}
            >
              {t("backup.r.command")}
            </button>
          </div>
        )}
        {needsDir && (
          <Field label={t("backup.r.dir")} hint={t("backup.r.dirHint")} error={err("dir")}>
            <input className="input mono" value={o.dir} onChange={(e) => set("dir", e.target.value)} />
          </Field>
        )}

        {type === "postgres" &&
          (format === "pg-custom" ? (
            <>
              <Field label={t("backup.r.targetDb")} hint={t("backup.r.pgHint")} error={err("database")}>
                <input className="input mono" value={o.database} onChange={(e) => set("database", e.target.value.trim())} />
              </Field>
              <label className="check">
                <input type="checkbox" checked={o.createDb} onChange={(e) => set("createDb", e.target.checked)} /> {t("backup.r.createDb")}
              </label>
              {o.database === (m?.database ?? job.database) && (
                <div className="hint-box err backup-hint-row">
                  <AlertTriangle size={14} /> {t("backup.r.pgOverwrite", { db: o.database })}
                </div>
              )}
            </>
          ) : (
            <div className="hint-box warn backup-hint-row">
              <AlertTriangle size={14} /> {t("backup.r.pgAllHint")}
            </div>
          ))}
        {type === "mysql" &&
          (m?.database ? (
            <>
              <Field label={t("backup.r.targetDb")} error={err("database")}>
                <input className="input mono" value={o.database} onChange={(e) => set("database", e.target.value.trim())} />
              </Field>
              <label className="check">
                <input type="checkbox" checked={o.createDb} onChange={(e) => set("createDb", e.target.checked)} /> {t("backup.r.createDb")}
              </label>
              <div className="hint-box warn backup-hint-row">
                <AlertTriangle size={14} /> {t("backup.r.myHint")}
              </div>
            </>
          ) : (
            <div className="hint-box warn backup-hint-row">
              <AlertTriangle size={14} /> {t("backup.r.myAllHint")}
            </div>
          ))}
        {type === "volume" && (
          <>
            <Field label={t("backup.r.targetVolume")} hint={t("backup.r.volumeHint")} error={err("volume")}>
              <input className="input mono" value={o.volume} onChange={(e) => set("volume", e.target.value.trim())} />
            </Field>
            <label className="check">
              <input type="checkbox" checked={o.stopContainers} onChange={(e) => set("stopContainers", e.target.checked)} /> {t("backup.f.stopContainers")}
            </label>
            <label className="check">
              <input type="checkbox" checked={o.clearVolume} onChange={(e) => set("clearVolume", e.target.checked)} /> {t("backup.r.clearVolume")}
            </label>
            <div className="hint-box warn backup-hint-row">
              <AlertTriangle size={14} /> {t("backup.r.volumeWarn")}
            </div>
          </>
        )}

        {item.encrypted && (
          <>
            <label className="check">
              <input type="checkbox" checked={otherPass} onChange={(e) => setOtherPass(e.target.checked)} /> {t("backup.r.otherPass")}
            </label>
            {otherPass && (
              <Field label={t("backup.f.passphrase")} error={err("passphrase")}>
                <input className="input mono" type="password" value={o.passphrase} onChange={(e) => set("passphrase", e.target.value)} autoComplete="off" />
              </Field>
            )}
          </>
        )}
        <Field label={t("backup.r.tempDir")} hint={t("backup.r.tempDirHint")} error={err("tempDir")}>
          <input className="input mono" value={o.tempDir} onChange={(e) => set("tempDir", e.target.value.trim())} />
        </Field>
      </div>
    </Modal>
  );
}
