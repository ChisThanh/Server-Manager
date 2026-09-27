import { useEffect, useMemo, useState } from "react";
import { AlertTriangle, Copy, Eye, KeyRound, Save, Wand2 } from "lucide-react";
import { Modal } from "../Overlays";
import { useRemote } from "../../ui/hooks";
import { toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { useT, type Key } from "../../i18n";
import {
  BackupService,
  DestIcon,
  Field,
  JOB_TYPES,
  RE_DB,
  RE_HOST,
  RE_USER,
  RE_VOLUME,
  TypeIcon,
  fmtTime,
  generatePassphrase,
  isAbsPath,
  typeLabel,
  weekdayLabel,
  type CronPreview,
  type Destination,
  type Job,
  type JobType,
  type Schedule,
} from "./common";
import { Select } from "../../ui/Select";

function blankJob(connId: string, dests: Destination[]): Job {
  return {
    id: "",
    server: connId,
    name: "",
    type: "files",
    enabled: true,
    folder: "",
    paths: [],
    excludes: [],
    sudo: true,
    database: "",
    authMode: "peer",
    dbUser: "",
    dbHost: "",
    dbPort: 0,
    hasDbPassword: false,
    volume: "",
    stopContainers: false,
    redisDumpPath: "",
    command: "",
    ext: "bin",
    restoreCommand: "",
    compression: "gzip",
    encrypt: true,
    hasPassphrase: false,
    verifyAfter: false,
    schedule: { mode: "daily", minute: 0, time: "02:00", weekday: 0, cron: "" },
    retention: { keepLast: 7, maxAgeDays: 0 },
    destination: dests[0]?.id ?? "",
    preHook: "",
    postHook: "",
    timeoutMin: 120,
    created: 0,
    updated: 0,
  };
}

const SCHED_MODES = ["manual", "hourly", "daily", "weekly", "cron"] as const;

// Tools each job type needs on the server (for early warnings).
const NEEDS: Record<JobType, string[][]> = {
  files: [["tar"]],
  postgres: [["pg_dump"]],
  mysql: [["mysqldump", "mariadb-dump"]],
  volume: [["docker"], ["tar"]],
  redis: [["redis-cli"]],
  custom: [],
};

export function JobEditor(props: {
  connId: string;
  initial: Partial<Job> | null;
  dests: Destination[];
  onClose: () => void;
  onSaved: (j: Job) => void;
  goDestinations: () => void;
}) {
  const t = useT();
  const { connId, dests } = props;
  const isEdit = !!props.initial?.id;
  const [f, setF] = useState<Job>(() => {
    const base = blankJob(connId, dests);
    const init = { ...base, ...(props.initial ?? {}) } as Job;
    if (!isEdit && (init.type === "postgres" || init.type === "mysql")) {
      init.compression = init.type === "postgres" && init.database ? "none" : "gzip";
    }
    return init;
  });
  const [pathsText, setPathsText] = useState((f.paths ?? []).join("\n"));
  const [exText, setExText] = useState((f.excludes ?? []).join("\n"));
  const [pass, setPass] = useState("");
  const [pass2, setPass2] = useState("");
  const [showPass, setShowPass] = useState(false);
  const [revealed, setRevealed] = useState("");
  const [dbPass, setDbPass] = useState("");
  const [clearDbPass, setClearDbPass] = useState(false);
  const [advanced, setAdvanced] = useState(!!(f.preHook || f.postHook));
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState("");
  const [touched, setTouched] = useState(false);
  const [preview, setPreview] = useState<CronPreview | null>(null);
  const [previewErr, setPreviewErr] = useState("");

  const detect = useRemote(() => BackupService.Detect(connId), [connId]);
  const tools = new Set(detect.data?.tools ?? []);

  const set = <K extends keyof Job>(k: K, v: Job[K]) => setF((x) => ({ ...x, [k]: v }));
  const setSched = (patch: Partial<Schedule>) => setF((x) => ({ ...x, schedule: { ...x.schedule, ...patch } }));
  const type = f.type as JobType;
  const isDB = type === "postgres" || type === "mysql";
  const pwMode = f.authMode === "password";

  // Schedule preview (validated by the backend's cron parser).
  useEffect(() => {
    const h = setTimeout(() => {
      BackupService.PreviewSchedule(f.schedule, 3)
        .then((p) => {
          setPreview(p);
          setPreviewErr("");
        })
        .catch((e) => {
          setPreview(null);
          setPreviewErr(errMsg(e));
        });
    }, 250);
    return () => clearTimeout(h);
  }, [f.schedule.mode, f.schedule.minute, f.schedule.time, f.schedule.weekday, f.schedule.cron]);

  const paths = pathsText
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
  const excludes = exText
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);

  const errors = useMemo(() => {
    const e: Partial<Record<string, string>> = {};
    if (!f.name.trim()) e.name = t("backup.v.required");
    else if (f.name.trim().length > 64) e.name = t("backup.v.tooLong");
    if (!f.destination) e.destination = t("backup.v.required");
    switch (type) {
      case "files":
        if (paths.length === 0) e.paths = t("backup.v.required");
        else if (paths.some((p) => !isAbsPath(p))) e.paths = t("backup.v.absPath");
        break;
      case "postgres":
      case "mysql":
        if (f.database && !RE_DB.test(f.database)) e.database = t("backup.v.dbName");
        if (pwMode) {
          if (!RE_USER.test(f.dbUser)) e.dbUser = t("backup.v.required");
          if (!dbPass && (!f.hasDbPassword || clearDbPass)) e.dbPass = t("backup.v.required");
        }
        if (f.dbHost && !RE_HOST.test(f.dbHost)) e.dbHost = t("backup.v.host");
        break;
      case "volume":
        if (!RE_VOLUME.test(f.volume)) e.volume = t("backup.v.volume");
        break;
      case "redis":
        if (f.dbHost && !RE_HOST.test(f.dbHost)) e.dbHost = t("backup.v.host");
        if (f.redisDumpPath && !isAbsPath(f.redisDumpPath)) e.redisDumpPath = t("backup.v.absPath");
        break;
      case "custom":
        if (!f.command.trim()) e.command = t("backup.v.required");
        if (!/^[a-z0-9]{1,10}$/.test(f.ext)) e.ext = t("backup.v.ext");
        break;
    }
    if (f.dbPort < 0 || f.dbPort > 65535) e.dbPort = t("backup.v.port");
    if (f.encrypt) {
      const need = !isEdit || !f.hasPassphrase;
      if (need && !pass) e.pass = t("backup.v.required");
      if (pass && pass.length < 8) e.pass = t("backup.v.passShort");
      if (pass && pass !== pass2) e.pass2 = t("backup.v.passMismatch");
    }
    if (f.retention.keepLast < 0) e.keepLast = t("backup.v.number");
    if (f.retention.maxAgeDays < 0) e.maxAgeDays = t("backup.v.number");
    if (f.timeoutMin < 1 || f.timeoutMin > 10080) e.timeoutMin = t("backup.v.timeout");
    if (previewErr) e.schedule = previewErr;
    return e;
  }, [f, paths.length, pathsText, pass, pass2, dbPass, clearDbPass, previewErr, isEdit, type, pwMode, t]);
  const valid = Object.keys(errors).length === 0;
  const err = (k: string) => (touched ? (errors[k] ?? null) : null);

  const missing = (NEEDS[type] ?? []).filter((alts) => detect.data && !alts.some((a) => tools.has(a))).map((alts) => alts.join("/"));

  const save = async () => {
    setTouched(true);
    if (!valid) return;
    setSaving(true);
    setSaveErr("");
    try {
      const job: Job = { ...f, name: f.name.trim(), paths, excludes };
      const saved = await BackupService.SaveJob(connId, job, {
        passphrase: f.encrypt ? pass : "",
        dbPassword: dbPass,
        clearDbPassword: clearDbPass && !dbPass,
      });
      toast(t("backup.job.saved", { name: saved.name }), "success");
      props.onSaved(saved);
    } catch (e) {
      setSaveErr(errMsg(e));
    } finally {
      setSaving(false);
    }
  };

  const reveal = async () => {
    try {
      setRevealed(await BackupService.RevealPassphrase(connId, f.id));
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const changeType = (nt: JobType) => {
    setF((x) => ({
      ...x,
      type: nt,
      authMode: nt === "redis" ? "none" : "peer",
      compression: nt === "postgres" && x.database ? "none" : "gzip",
      sudo: nt === "files" || nt === "volume",
      ext: nt === "custom" ? x.ext || "bin" : x.ext,
    }));
  };

  return (
    <Modal
      size="xwide"
      title={
        <>
          <TypeIcon type={f.type} /> {isEdit ? t("backup.job.editTitle", { name: props.initial?.name ?? "" }) : t("backup.job.newTitle")}
        </>
      }
      onClose={props.onClose}
      footer={
        <>
          {saveErr && <span className="left backup-save-err">{saveErr}</span>}
          <button className="btn" onClick={props.onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save} disabled={saving || (touched && !valid)}>
            {saving ? <span className="spinner" /> : <Save size={14} />} {t("backup.save")}
          </button>
        </>
      }
    >
      <div className="backup-form">
        <div className="form-grid">
          <Field label={t("backup.f.name")} error={err("name")}>
            <input className={`input ${err("name") ? "invalid" : ""}`} value={f.name} onChange={(e) => set("name", e.target.value)} autoFocus maxLength={64} />
          </Field>
          <Field label={t("backup.f.destination")} error={err("destination")} hint={dests.length === 0 ? undefined : undefined}>
            {dests.length === 0 ? (
              <button className="btn sm" onClick={props.goDestinations}>
                {t("backup.dest.add")}
              </button>
            ) : (
              <Select
                value={f.destination}
                onChange={(v) => set("destination", v)}
                placeholder="—"
                invalid={!!err("destination")}
                options={dests.map((d) => ({ value: d.id, label: d.name, icon: <DestIcon type={d.type} size={13} />, hint: t(`backup.destType.${d.type}` as Key) }))}
              />
            )}
          </Field>
        </div>

        <Field label={t("backup.f.type")}>
          <div className="segmented backup-types">
            {JOB_TYPES.map((ty) => (
              <button key={ty} className={f.type === ty ? "on" : ""} onClick={() => !isEdit && changeType(ty)} disabled={isEdit && f.type !== ty}>
                <TypeIcon type={ty} size={13} /> {typeLabel(ty)}
              </button>
            ))}
          </div>
        </Field>
        {missing.length > 0 && (
          <div className="hint-box warn backup-hint-row">
            <AlertTriangle size={14} /> {t("backup.toolsMissing", { tools: missing.join(", ") })}
          </div>
        )}

        {/* ---- type-specific ---- */}
        {type === "files" && (
          <div className="form-grid">
            <Field label={t("backup.f.paths")} hint={t("backup.f.pathsHint")} error={err("paths")}>
              <textarea
                className="input mono backup-textarea"
                value={pathsText}
                onChange={(e) => setPathsText(e.target.value)}
                placeholder={"/etc\n/var/www"}
              />
            </Field>
            <Field label={t("backup.f.excludes")} hint={t("backup.f.excludesHint")}>
              <textarea
                className="input mono backup-textarea"
                value={exText}
                onChange={(e) => setExText(e.target.value)}
                placeholder={"*.log\n/var/www/cache"}
              />
            </Field>
            <label className="check">
              <input type="checkbox" checked={f.sudo} onChange={(e) => set("sudo", e.target.checked)} /> {t("backup.f.sudo")}
            </label>
          </div>
        )}

        {isDB && (
          <>
            <div className="form-grid">
              <Field label={t("backup.f.database")} hint={t("backup.f.databaseHint")} error={err("database")}>
                <input className="input mono" value={f.database} onChange={(e) => set("database", e.target.value)} placeholder={t("backup.allDatabases")} />
              </Field>
              <Field label={t("backup.f.auth")}>
                <div className="segmented">
                  <button className={!pwMode ? "on" : ""} onClick={() => set("authMode", "peer")}>
                    {type === "postgres" ? t("backup.auth.pgPeer") : t("backup.auth.mysqlSocket")}
                  </button>
                  <button className={pwMode ? "on" : ""} onClick={() => set("authMode", "password")}>
                    {t("backup.auth.password")}
                  </button>
                </div>
              </Field>
            </div>
            <div className="hint-box">
              {type === "postgres"
                ? f.database
                  ? t("backup.hint.pgCustom")
                  : t("backup.hint.pgAll")
                : f.database
                  ? t("backup.hint.mysqlOne")
                  : t("backup.hint.mysqlAll")}{" "}
              {!pwMode && (type === "postgres" ? t("backup.hint.pgPeer") : t("backup.hint.mysqlSocket"))}
            </div>
            {pwMode && (
              <div className="form-grid">
                <Field label={t("backup.f.dbUser")} error={err("dbUser")}>
                  <input className="input mono" value={f.dbUser} onChange={(e) => set("dbUser", e.target.value)} />
                </Field>
                <Field
                  label={t("backup.f.dbPassword")}
                  error={err("dbPass")}
                  hint={f.hasDbPassword && !clearDbPass ? t("backup.f.savedKeep") : t("backup.f.keychain")}
                >
                  <input
                    className="input"
                    type="password"
                    value={dbPass}
                    onChange={(e) => setDbPass(e.target.value)}
                    placeholder={f.hasDbPassword && !clearDbPass ? "••••••••" : ""}
                    autoComplete="new-password"
                  />
                </Field>
                <Field label={t("backup.f.dbHost")} error={err("dbHost")} hint={type === "postgres" ? "127.0.0.1" : t("backup.f.socketDefault")}>
                  <input className="input mono" value={f.dbHost} onChange={(e) => set("dbHost", e.target.value.trim())} />
                </Field>
                <Field label={t("backup.f.dbPort")} error={err("dbPort")}>
                  <input
                    className="input mono"
                    type="number"
                    value={f.dbPort || ""}
                    placeholder={type === "postgres" ? "5432" : "3306"}
                    onChange={(e) => set("dbPort", Number(e.target.value) || 0)}
                  />
                </Field>
              </div>
            )}
            {pwMode && (
              <label className="check">
                <input type="checkbox" checked={f.sudo} onChange={(e) => set("sudo", e.target.checked)} /> {t("backup.f.sudoDb")}
              </label>
            )}
          </>
        )}

        {type === "volume" && (
          <>
            <div className="form-grid">
              <Field label={t("backup.f.volume")} error={err("volume")}>
                <input className="input mono" list="backup-volumes" value={f.volume} onChange={(e) => set("volume", e.target.value.trim())} />
                <datalist id="backup-volumes">
                  {(detect.data?.volumes ?? []).map((v) => (
                    <option key={v} value={v} />
                  ))}
                </datalist>
              </Field>
            </div>
            <label className="check">
              <input type="checkbox" checked={f.stopContainers} onChange={(e) => set("stopContainers", e.target.checked)} /> {t("backup.f.stopContainers")}
            </label>
            <div className="hint-box warn backup-hint-row">
              <AlertTriangle size={14} /> {t("backup.hint.volume")}
            </div>
          </>
        )}

        {type === "redis" && (
          <>
            <div className="form-grid">
              <Field label={t("backup.f.dbHost")} error={err("dbHost")} hint="127.0.0.1">
                <input className="input mono" value={f.dbHost} onChange={(e) => set("dbHost", e.target.value.trim())} />
              </Field>
              <Field label={t("backup.f.dbPort")} error={err("dbPort")}>
                <input
                  className="input mono"
                  type="number"
                  value={f.dbPort || ""}
                  placeholder="6379"
                  onChange={(e) => set("dbPort", Number(e.target.value) || 0)}
                />
              </Field>
              <Field label={t("backup.f.redisAuth")} hint={f.hasDbPassword && !clearDbPass ? t("backup.f.savedKeep") : t("backup.f.redisAuthHint")}>
                <input
                  className="input"
                  type="password"
                  value={dbPass}
                  onChange={(e) => {
                    setDbPass(e.target.value);
                    set("authMode", e.target.value || (f.hasDbPassword && !clearDbPass) ? "password" : "none");
                  }}
                  placeholder={f.hasDbPassword && !clearDbPass ? "••••••••" : ""}
                  autoComplete="new-password"
                />
              </Field>
              <Field label={t("backup.f.redisPath")} hint={t("backup.f.redisPathHint")} error={err("redisDumpPath")}>
                <input
                  className="input mono"
                  value={f.redisDumpPath}
                  onChange={(e) => set("redisDumpPath", e.target.value.trim())}
                  placeholder="/var/lib/redis/dump.rdb"
                />
              </Field>
            </div>
            <div className="hint-box">{t("backup.hint.redis")}</div>
          </>
        )}
        {(isDB || type === "redis") && f.hasDbPassword && (
          <label className="check backup-small">
            <input type="checkbox" checked={clearDbPass} onChange={(e) => setClearDbPass(e.target.checked)} /> {t("backup.f.clearDbPassword")}
          </label>
        )}

        {type === "custom" && (
          <>
            <Field label={t("backup.f.command")} hint={t("backup.f.commandHint")} error={err("command")}>
              <textarea
                className="input mono backup-textarea"
                value={f.command}
                onChange={(e) => set("command", e.target.value)}
                placeholder="tar -cf - /srv/app/data"
              />
            </Field>
            <div className="form-grid">
              <Field label={t("backup.f.ext")} error={err("ext")}>
                <input className="input mono" value={f.ext} onChange={(e) => set("ext", e.target.value.trim().toLowerCase())} />
              </Field>
              <Field label={t("backup.f.restoreCommand")} hint={t("backup.f.restoreCommandHint")}>
                <input className="input mono" value={f.restoreCommand} onChange={(e) => set("restoreCommand", e.target.value)} placeholder="tar -xf - -C /" />
              </Field>
            </div>
            <label className="check">
              <input type="checkbox" checked={f.sudo} onChange={(e) => set("sudo", e.target.checked)} /> {t("backup.f.sudo")}
            </label>
          </>
        )}

        {/* ---- storage ---- */}
        <div className="backup-sep">{t("backup.sec.storage")}</div>
        <div className="form-grid">
          <Field
            label={t("backup.f.compression")}
            hint={
              f.compression === "zstd" && detect.data && !tools.has("zstd")
                ? t("backup.hint.noZstd")
                : type === "postgres" && f.database && f.compression !== "none"
                  ? t("backup.hint.pgCompressed")
                  : undefined
            }
          >
            <div className="segmented">
              {["none", "gzip", "zstd"].map((c) => (
                <button key={c} className={f.compression === c ? "on" : ""} onClick={() => set("compression", c)}>
                  {c === "none" ? t("backup.none") : c}
                </button>
              ))}
            </div>
          </Field>
          <Field label={t("backup.f.timeout")} error={err("timeoutMin")}>
            <input className="input" type="number" min={1} value={f.timeoutMin} onChange={(e) => set("timeoutMin", Number(e.target.value) || 0)} />
          </Field>
        </div>
        <label className="check">
          <input type="checkbox" checked={f.encrypt} onChange={(e) => set("encrypt", e.target.checked)} /> <KeyRound size={13} /> {t("backup.f.encrypt")}
        </label>
        {f.encrypt && (
          <div className="backup-indent">
            <div className="form-grid">
              <Field
                label={isEdit && f.hasPassphrase ? t("backup.f.newPassphrase") : t("backup.f.passphrase")}
                error={err("pass")}
                hint={isEdit && f.hasPassphrase ? t("backup.f.savedKeep") : t("backup.f.keychain")}
              >
                <div className="backup-input-row">
                  <input
                    className="input mono"
                    type={showPass ? "text" : "password"}
                    value={pass}
                    onChange={(e) => setPass(e.target.value)}
                    autoComplete="new-password"
                  />
                  <button className="icon-btn" title={t("backup.show")} onClick={() => setShowPass((v) => !v)}>
                    <Eye size={14} />
                  </button>
                  <button
                    className="icon-btn"
                    title={t("backup.generate")}
                    onClick={() => {
                      const p = generatePassphrase();
                      setPass(p);
                      setPass2(p);
                      setShowPass(true);
                    }}
                  >
                    <Wand2 size={14} />
                  </button>
                </div>
              </Field>
              <Field label={t("backup.f.passphrase2")} error={err("pass2")}>
                <input
                  className="input mono"
                  type={showPass ? "text" : "password"}
                  value={pass2}
                  onChange={(e) => setPass2(e.target.value)}
                  autoComplete="new-password"
                />
              </Field>
            </div>
            <div className="hint-box warn backup-hint-row">
              <AlertTriangle size={14} />
              <span>
                {t("backup.hint.passphrase")} {isEdit && f.hasPassphrase && pass ? t("backup.hint.passChange") : ""}
              </span>
            </div>
            {isEdit && f.hasPassphrase && (
              <div className="backup-hint-row backup-small">
                {revealed ? (
                  <>
                    <code className="mono backup-secret">{revealed}</code>
                    <button
                      className="icon-btn"
                      title={t("backup.copy")}
                      onClick={() => {
                        navigator.clipboard?.writeText(revealed);
                        toast(t("backup.copied"), "success");
                      }}
                    >
                      <Copy size={13} />
                    </button>
                  </>
                ) : (
                  <button className="btn sm ghost" onClick={reveal}>
                    <Eye size={13} /> {t("backup.revealPassphrase")}
                  </button>
                )}
              </div>
            )}
          </div>
        )}
        <label className="check">
          <input type="checkbox" checked={f.verifyAfter} onChange={(e) => set("verifyAfter", e.target.checked)} /> {t("backup.f.verifyAfter")}
        </label>

        {/* ---- schedule ---- */}
        <div className="backup-sep">{t("backup.sec.schedule")}</div>
        <label className="check">
          <input type="checkbox" checked={f.enabled} onChange={(e) => set("enabled", e.target.checked)} /> {t("backup.f.enabled")}
        </label>
        <div className="segmented backup-sched">
          {SCHED_MODES.map((m) => (
            <button key={m} className={f.schedule.mode === m || (!f.schedule.mode && m === "manual") ? "on" : ""} onClick={() => setSched({ mode: m })}>
              {t(`backup.sched.${m}` as Key)}
            </button>
          ))}
        </div>
        <div className="form-grid">
          {f.schedule.mode === "hourly" && (
            <Field label={t("backup.f.minute")}>
              <input
                className="input"
                type="number"
                min={0}
                max={59}
                value={f.schedule.minute}
                onChange={(e) =>
                  setSched({
                    minute: Math.max(0, Math.min(59, Number(e.target.value) || 0)),
                  })
                }
              />
            </Field>
          )}
          {(f.schedule.mode === "daily" || f.schedule.mode === "weekly") && (
            <Field label={t("backup.f.time")}>
              <input className="input" type="time" value={f.schedule.time} onChange={(e) => setSched({ time: e.target.value })} />
            </Field>
          )}
          {f.schedule.mode === "weekly" && (
            <Field label={t("backup.f.weekday")}>
              <Select value={f.schedule.weekday} onChange={(v) => setSched({ weekday: v })} options={[1, 2, 3, 4, 5, 6, 0].map((d) => ({ value: d, label: weekdayLabel(d) }))} />
            </Field>
          )}
          {f.schedule.mode === "cron" && (
            <Field label={t("backup.f.cron")} hint={t("backup.f.cronHint")}>
              <input className="input mono" value={f.schedule.cron} onChange={(e) => setSched({ cron: e.target.value })} placeholder="0 3 * * *" />
            </Field>
          )}
        </div>
        {previewErr ? (
          <div className="backup-preview err">{previewErr}</div>
        ) : preview && preview.cron ? (
          <div className="backup-preview">
            <span className="mono">{preview.cron}</span> · {t("backup.nextRuns")}: {(preview.next ?? []).map((n) => fmtTime(n)).join(" · ")}
          </div>
        ) : (
          <div className="backup-preview muted">{t("backup.sched.manualHint")}</div>
        )}

        {/* ---- retention ---- */}
        <div className="backup-sep">{t("backup.retention")}</div>
        <div className="form-grid">
          <Field label={t("backup.f.keepLast")} hint={t("backup.f.zeroUnlimited")} error={err("keepLast")}>
            <input
              className="input"
              type="number"
              min={0}
              value={f.retention.keepLast}
              onChange={(e) =>
                set("retention", {
                  ...f.retention,
                  keepLast: Number(e.target.value) || 0,
                })
              }
            />
          </Field>
          <Field label={t("backup.f.maxAgeDays")} hint={t("backup.f.zeroUnlimited")} error={err("maxAgeDays")}>
            <input
              className="input"
              type="number"
              min={0}
              value={f.retention.maxAgeDays}
              onChange={(e) =>
                set("retention", {
                  ...f.retention,
                  maxAgeDays: Number(e.target.value) || 0,
                })
              }
            />
          </Field>
        </div>
        <div className="hint-box">{t("backup.hint.retention")}</div>

        {/* ---- advanced ---- */}
        <button className="btn ghost sm" onClick={() => setAdvanced((v) => !v)}>
          {advanced ? "▾" : "▸"} {t("backup.sec.hooks")}
        </button>
        {advanced && (
          <div className="form-grid">
            <Field label={t("backup.f.preHook")} hint={t("backup.f.preHookHint")}>
              <textarea className="input mono backup-textarea" value={f.preHook} onChange={(e) => set("preHook", e.target.value)} />
            </Field>
            <Field label={t("backup.f.postHook")} hint={t("backup.f.postHookHint")}>
              <textarea className="input mono backup-textarea" value={f.postHook} onChange={(e) => set("postHook", e.target.value)} />
            </Field>
          </div>
        )}
        {isEdit && (
          <div className="backup-small muted">
            {t("backup.folder")}: <span className="mono">{f.folder}</span> · {t("backup.created")}: {fmtTime(f.created)}
          </div>
        )}
      </div>
    </Modal>
  );
}
