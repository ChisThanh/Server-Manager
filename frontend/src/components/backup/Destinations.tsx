import { useMemo, useState } from "react";
import { CheckCircle2, FolderOpen, Pencil, Plug, Plus, Save, Trash2, Warehouse, XCircle } from "lucide-react";
import { Modal } from "../Overlays";
import type { Remote } from "../../ui/hooks";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { useT, type Key } from "../../i18n";
import { BackupService, DestIcon, Field, destLocation, isAbsPath, type Destination, type DestType, type Job } from "./common";
import { Select } from "../../ui/Select";

export function DestinationsView({ connId, dests, jobs, onChanged }: { connId: string; dests: Remote<Destination[]>; jobs: Job[]; onChanged: () => void }) {
  const t = useT();
  const [editing, setEditing] = useState<Destination | null | "new">(null);
  const [testing, setTesting] = useState<string | null>(null);
  const used = useMemo(() => {
    const m = new Map<string, number>();
    jobs.forEach((j) => m.set(j.destination, (m.get(j.destination) ?? 0) + 1));
    return m;
  }, [jobs]);

  if (dests.error && !dests.data) return <ErrorBox error={dests.error} onRetry={dests.reload} />;
  if (!dests.data) return <Loading />;

  const test = async (d: Destination) => {
    setTesting(d.id);
    try {
      const r = await BackupService.TestDestination(connId, d, "");
      toast(r.needsRoot ? t("backup.dest.testOkRoot", { ms: r.millis }) : t("backup.dest.testOk", { ms: r.millis }), "success");
    } catch (e) {
      toast(`${d.name}: ${errMsg(e)}`, "error");
    } finally {
      setTesting(null);
    }
  };

  const remove = async (d: Destination) => {
    if (!(await confirmDialog(t("backup.dest.deleteTitle", { name: d.name }), t("backup.dest.deleteMsg"), t("backup.delete"), true))) return;
    try {
      await BackupService.DeleteDestination(d.id);
      toast(t("backup.dest.deleted", { name: d.name }), "success");
      onChanged();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <>
      <div className="toolbar">
        <span className="muted">{t("backup.dest.globalHint")}</span>
        <div className="grow" />
        <button className="btn primary sm" onClick={() => setEditing("new")}>
          <Plus size={13} /> {t("backup.dest.add")}
        </button>
      </div>
      {dests.data.length === 0 ? (
        <Empty icon={<Warehouse size={28} />} title={t("backup.dest.none")} text={t("backup.dest.noneText")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("backup.f.name")}</th>
                <th>{t("backup.f.type")}</th>
                <th>{t("backup.dest.location")}</th>
                <th className="num">{t("backup.dest.jobs")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {dests.data.map((d) => (
                <tr key={d.id}>
                  <td>
                    <DestIcon type={d.type} /> {d.name}
                  </td>
                  <td>
                    {t(`backup.destType.${d.type}` as Key)}
                    {d.type === "s3" && !d.hasSecret && <span className="badge err backup-badge">{t("backup.dest.noSecret")}</span>}
                  </td>
                  <td className="cmd" title={destLocation(d)}>
                    {destLocation(d)}
                  </td>
                  <td className="num">{used.get(d.id) ?? 0}</td>
                  <td className="backup-actions-cell">
                    <button className="btn sm" onClick={() => test(d)} disabled={testing === d.id}>
                      {testing === d.id ? <span className="spinner" /> : <Plug size={13} />} {t("backup.dest.test")}
                    </button>
                    <button className="icon-btn" title={t("backup.edit")} onClick={() => setEditing(d)}>
                      <Pencil size={14} />
                    </button>
                    <button className="icon-btn" title={t("backup.delete")} onClick={() => remove(d)}>
                      <Trash2 size={14} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {editing && (
        <DestEditor
          connId={connId}
          initial={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            onChanged();
          }}
        />
      )}
    </>
  );
}

const PRESETS: Record<string, { endpoint: string; region: string; pathStyle: boolean }> = {
  aws: {
    endpoint: "s3.eu-central-1.amazonaws.com",
    region: "eu-central-1",
    pathStyle: false,
  },
  b2: {
    endpoint: "s3.us-west-004.backblazeb2.com",
    region: "us-west-004",
    pathStyle: false,
  },
  wasabi: {
    endpoint: "s3.eu-central-1.wasabisys.com",
    region: "eu-central-1",
    pathStyle: false,
  },
  r2: {
    endpoint: "<account-id>.r2.cloudflarestorage.com",
    region: "auto",
    pathStyle: true,
  },
  minio: {
    endpoint: "minio.example.com:9000",
    region: "us-east-1",
    pathStyle: true,
  },
  other: { endpoint: "s3.example.com", region: "", pathStyle: true },
};

function blankDest(): Destination {
  return {
    id: "",
    name: "",
    type: "local",
    path: "",
    endpoint: "",
    region: "",
    bucket: "",
    prefix: "backups",
    accessKey: "",
    pathStyle: false,
    useTls: true,
    hasSecret: false,
    created: 0,
    updated: 0,
  };
}

function DestEditor({ connId, initial, onClose, onSaved }: { connId: string; initial: Destination | null; onClose: () => void; onSaved: () => void }) {
  const t = useT();
  const [d, setD] = useState<Destination>(() => initial ?? blankDest());
  const [secret, setSecret] = useState("");
  const [preset, setPreset] = useState("other");
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState<"" | "save" | "test">("");
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const set = <K extends keyof Destination>(k: K, v: Destination[K]) => {
    setD((x) => ({ ...x, [k]: v }));
    setResult(null);
  };
  const type = d.type as DestType;
  const isEdit = !!initial;

  const errors: Record<string, string> = {};
  if (!d.name.trim()) errors.name = t("backup.v.required");
  if (type === "local" && !d.path.trim()) errors.path = t("backup.v.required");
  if (type === "server" && (!isAbsPath(d.path.trim()) || d.path.trim() === "/")) errors.path = t("backup.v.absPath");
  if (type === "s3") {
    if (!d.endpoint.trim() || d.endpoint.includes("<")) errors.endpoint = t("backup.v.required");
    if (!/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(d.bucket)) errors.bucket = t("backup.v.bucket");
    if (!d.accessKey.trim()) errors.accessKey = t("backup.v.required");
    if (!secret && !d.hasSecret) errors.secret = t("backup.v.required");
  }
  const valid = Object.keys(errors).length === 0;
  const err = (k: string) => (touched ? (errors[k] ?? null) : null);

  const browse = async () => {
    const p = await BackupService.PickLocalFolder(t("backup.dest.pickFolder"));
    if (p) set("path", p);
  };

  const test = async () => {
    setTouched(true);
    if (!valid) return;
    setBusy("test");
    setResult(null);
    try {
      const r = await BackupService.TestDestination(connId, d, secret);
      setResult({
        ok: true,
        text: r.needsRoot ? t("backup.dest.testOkRoot", { ms: r.millis }) : t("backup.dest.testOk", { ms: r.millis }),
      });
    } catch (e) {
      setResult({ ok: false, text: errMsg(e) });
    } finally {
      setBusy("");
    }
  };

  const save = async () => {
    setTouched(true);
    if (!valid) return;
    setBusy("save");
    try {
      const saved = await BackupService.SaveDestination(d, secret);
      toast(t("backup.dest.saved", { name: saved.name }), "success");
      onSaved();
    } catch (e) {
      setResult({ ok: false, text: errMsg(e) });
    } finally {
      setBusy("");
    }
  };

  return (
    <Modal
      size="wide"
      title={
        <>
          <DestIcon type={d.type} /> {isEdit ? t("backup.dest.editTitle", { name: initial!.name }) : t("backup.dest.newTitle")}
        </>
      }
      onClose={onClose}
      footer={
        <>
          <button className="btn left" onClick={test} disabled={!!busy}>
            {busy === "test" ? <span className="spinner" /> : <Plug size={14} />} {t("backup.dest.test")}
          </button>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save} disabled={!!busy || (touched && !valid)}>
            {busy === "save" ? <span className="spinner" /> : <Save size={14} />} {t("backup.save")}
          </button>
        </>
      }
    >
      <div className="backup-form">
        <Field label={t("backup.f.name")} error={err("name")}>
          <input className="input" value={d.name} onChange={(e) => set("name", e.target.value)} autoFocus maxLength={64} />
        </Field>
        <Field label={t("backup.f.type")}>
          <div className="segmented">
            {(["local", "server", "s3"] as DestType[]).map((ty) => (
              <button key={ty} className={type === ty ? "on" : ""} disabled={isEdit && type !== ty} onClick={() => !isEdit && set("type", ty)}>
                <DestIcon type={ty} size={12} /> {t(`backup.destType.${ty}` as Key)}
              </button>
            ))}
          </div>
        </Field>
        {type === "local" && (
          <Field label={t("backup.dest.localPath")} hint={t("backup.dest.localHint")} error={err("path")}>
            <div className="backup-input-row">
              <input className="input mono" value={d.path} onChange={(e) => set("path", e.target.value)} />
              <button className="btn sm" onClick={browse}>
                <FolderOpen size={13} /> {t("backup.dest.browse")}
              </button>
            </div>
          </Field>
        )}
        {type === "server" && (
          <>
            <Field label={t("backup.dest.serverPath")} hint={t("backup.dest.serverHint")} error={err("path")}>
              <input className="input mono" value={d.path} onChange={(e) => set("path", e.target.value)} placeholder="/var/backups/server-manager" />
            </Field>
            <div className="hint-box warn">{t("backup.dest.serverWarn")}</div>
          </>
        )}
        {type === "s3" && (
          <>
            <Field label={t("backup.dest.provider")}>
              <Select
                value={preset}
                onChange={(v) => {
                  const p = PRESETS[v];
                  setPreset(v);
                  setD((x) => ({
                    ...x,
                    pathStyle: p.pathStyle,
                    region: x.region || p.region,
                    useTls: true,
                  }));
                }}
                options={Object.keys(PRESETS).map((k) => ({ value: k, label: t(`backup.s3.${k}` as Key) }))}
              />
            </Field>
            <div className="form-grid">
              <Field label={t("backup.dest.endpoint")} hint={t("backup.dest.endpointHint")} error={err("endpoint")}>
                <input
                  className="input mono"
                  value={d.endpoint}
                  onChange={(e) => set("endpoint", e.target.value.trim())}
                  placeholder={PRESETS[preset].endpoint}
                />
              </Field>
              <Field label={t("backup.dest.region")}>
                <input
                  className="input mono"
                  value={d.region}
                  onChange={(e) => set("region", e.target.value.trim())}
                  placeholder={PRESETS[preset].region || "us-east-1"}
                />
              </Field>
              <Field label={t("backup.dest.bucket")} error={err("bucket")}>
                <input className="input mono" value={d.bucket} onChange={(e) => set("bucket", e.target.value.trim())} />
              </Field>
              <Field label={t("backup.dest.prefix")} hint={t("backup.dest.prefixHint")}>
                <input className="input mono" value={d.prefix} onChange={(e) => set("prefix", e.target.value.trim())} />
              </Field>
              <Field label={t("backup.dest.accessKey")} error={err("accessKey")}>
                <input className="input mono" value={d.accessKey} onChange={(e) => set("accessKey", e.target.value.trim())} autoComplete="off" />
              </Field>
              <Field label={t("backup.dest.secretKey")} error={err("secret")} hint={d.hasSecret ? t("backup.f.savedKeep") : t("backup.f.keychain")}>
                <input
                  className="input mono"
                  type="password"
                  value={secret}
                  onChange={(e) => {
                    setSecret(e.target.value);
                    setResult(null);
                  }}
                  placeholder={d.hasSecret ? "••••••••" : ""}
                  autoComplete="new-password"
                />
              </Field>
            </div>
            <label className="check">
              <input type="checkbox" checked={d.useTls} onChange={(e) => set("useTls", e.target.checked)} /> {t("backup.dest.tls")}
            </label>
            <label className="check">
              <input type="checkbox" checked={d.pathStyle} onChange={(e) => set("pathStyle", e.target.checked)} /> {t("backup.dest.pathStyle")}
            </label>
            <div className="hint-box">{t("backup.dest.s3Hint")}</div>
          </>
        )}
        {result && (
          <div className={`hint-box backup-hint-row ${result.ok ? "ok" : "err"}`}>
            {result.ok ? <CheckCircle2 size={14} color="var(--ok)" /> : <XCircle size={14} color="var(--err)" />}
            <span>{result.text}</span>
          </div>
        )}
      </div>
    </Modal>
  );
}
