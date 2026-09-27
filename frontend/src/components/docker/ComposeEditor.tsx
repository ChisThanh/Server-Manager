import { useEffect, useRef, useState } from "react";
import { AlertTriangle, CheckCircle2, FileCode2, History, Rocket, ShieldCheck, Undo2 } from "lucide-react";
import { monaco } from "../../lib/monaco";
import { CodeEditor } from "../../ui/CodeEditor";
import { Loading } from "../../ui/Page";
import { useRemote } from "../../ui/hooks";
import { runJob } from "../../ui/jobs";
import { confirmDanger } from "../../ui/confirm";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { confirmDialog, toast } from "../../store/ui";
import { useT } from "../../i18n";
import { BigModal, DockerService, ds, fullDate, type ComposeFile, type ComposeSpec, type ComposeVersion, type ValidateResult } from "./common";
import { Select } from "../../ui/Select";

export const COMPOSE_TEMPLATE = `services:
  app:
    image: nginx:alpine
    restart: unless-stopped
    ports:
      - "8080:80"
    # environment:
    #   TZ: Asia/Ho_Chi_Minh
    # env_file: .env
    # volumes:
    #   - ./html:/usr/share/nginx/html:ro
    #   - data:/data

# volumes:
#   data: {}
`;

export interface EditorTarget {
  spec: ComposeSpec;
  /** "new": create the file (refused if it exists); "edit": existing file. */
  mode: "new" | "edit";
}

/**
 * Full-screen compose file editor: validate in place, save & deploy (with
 * the previous content kept in the history) and roll back to a stored version.
 */
export function ComposeEditor({ connId, target, canEdit, onClose, onDeployed }: { connId: string; target: EditorTarget; canEdit: boolean; onClose: () => void; onDeployed: () => void }) {
  const t = useT();
  const [spec, setSpec] = useState<ComposeSpec>(target.spec);
  const [mode, setMode] = useState(target.mode);
  const [file, setFile] = useState<ComposeFile | null>(null);
  const [loadErr, setLoadErr] = useState("");
  const [text, setText] = useState("");
  const [result, setResult] = useState<ValidateResult | null>(null);
  const [validating, setValidating] = useState(false);
  const [pull, setPull] = useState(false);
  const [history, setHistory] = useState(false);
  const dirty = file !== null && text !== (file.exists ? file.content : "");

  const load = async (path: string) => {
    setFile(null);
    setLoadErr("");
    setResult(null);
    try {
      const f = await ds(connId, (pw) => DockerService.ReadComposeFile(connId, path, pw));
      setFile(f);
      if (f.exists) {
        setText(f.content);
        if (mode === "new") setMode("edit");
      } else {
        setText(mode === "new" ? COMPOSE_TEMPLATE : "");
      }
    } catch (e) {
      setLoadErr(errMsg(e));
    }
  };

  useEffect(() => {
    load(spec.path);
  }, [spec.path]);

  const close = async () => {
    if (dirty && !(await confirmDialog(t("docker.ed.discardQ"), t("docker.ed.discardMsg"), t("docker.ed.discard"), true))) return;
    onClose();
  };

  const switchFile = async (path: string) => {
    if (path === spec.path) return;
    if (dirty && !(await confirmDialog(t("docker.ed.discardQ"), t("docker.ed.discardMsg"), t("docker.ed.discard"), true))) return;
    setSpec((s) => ({ ...s, path }));
  };

  const validate = async () => {
    setValidating(true);
    try {
      const r = await ds(connId, (pw) => DockerService.ValidateCompose(connId, spec, text, pw));
      setResult(r);
      return r;
    } catch (e) {
      toast(errMsg(e), "error");
      return null;
    } finally {
      setValidating(false);
    }
  };

  const deploy = async (content: string, kind: "save" | "new" | "rollback", versionId = "") => {
    if (!file) return false;
    // Someone else may have changed the file since it was opened.
    if (file.exists) {
      try {
        const now = await ds(connId, (pw) => DockerService.ReadComposeFile(connId, spec.path, pw));
        if (now.sha !== file.sha) {
          if (await confirmDialog(t("docker.ed.changedQ"), t("docker.ed.changedMsg"), t("docker.ed.reload"))) await load(spec.path);
          return false;
        }
      } catch (e) {
        toast(errMsg(e), "error");
        return false;
      }
    }
    if (kind !== "rollback") {
      const r = await validate();
      if (!r) return false;
      if (!r.ok) {
        toast(t("docker.ed.fixErrors"), "error");
        return false;
      }
    }
    const ok = await confirmDanger({
      serverId: connId,
      title: kind === "rollback" ? t("docker.ed.rollbackQ", { project: spec.project }) : t("docker.ed.deployQ", { project: spec.project }),
      message: t("docker.ed.deployMsg", { path: spec.path }),
      confirmText: kind === "rollback" ? t("docker.ed.rollback") : t("docker.ed.saveDeploy"),
    });
    if (!ok) return false;
    const title = kind === "rollback" ? t("docker.ed.rollbackTitle", { project: spec.project }) : t("docker.ed.deployTitle", { project: spec.project });
    const info = await runJob(title, () =>
      ds(connId, (pw) => DockerService.DeployCompose(connId, { spec, content, expectedSha: file.exists ? file.sha : "", pull, kind, versionId }, pw)),
    );
    if (!info) return false;
    // The file may have been written even if `up` failed afterwards.
    await load(spec.path);
    onDeployed();
    if (info.state === "done") {
      toast(t("docker.ed.deployed", { project: spec.project }), "success");
      return true;
    }
    return false;
  };

  const saveDeploy = () => deploy(text, mode === "new" ? "new" : "save");

  const files = spec.files?.length ? spec.files : [spec.path];
  const readOnly = !canEdit;

  return (
    <BigModal
      className="docker-editor"
      onClose={close}
      title={
        <>
          <FileCode2 size={16} />
          <span>{mode === "new" ? t("docker.ed.newTitle", { project: spec.project }) : t("docker.ed.title", { project: spec.project })}</span>
          {dirty && <span className="badge warn">{t("docker.ed.unsaved")}</span>}
        </>
      }
      footer={
        <>
          {mode === "edit" && (
            <button className="btn left" onClick={() => setHistory(true)} disabled={!file}>
              <History size={13} /> {t("docker.ed.history")}
            </button>
          )}
          {canEdit && (
            <>
              <label className="check" title={t("docker.ed.pullHint")}>
                <input type="checkbox" checked={pull} onChange={(e) => setPull(e.target.checked)} /> {t("docker.ed.pull")}
              </label>
              <button className="btn" onClick={validate} disabled={!file || validating}>
                {validating ? <span className="spinner" /> : <ShieldCheck size={13} />} {t("docker.ed.validate")}
              </button>
              <button className="btn primary" onClick={saveDeploy} disabled={!file || validating || (!dirty && mode === "edit" && !pull) || !text.trim()}>
                <Rocket size={13} /> {mode === "new" ? t("docker.ed.createDeploy") : t("docker.ed.saveDeploy")}
              </button>
            </>
          )}
          <button className="btn" onClick={close}>
            {t("common.close")}
          </button>
        </>
      }
    >
      <div className="docker-ed-meta">
        {files.length > 1 ? (
          <Select mono value={spec.path} onChange={switchFile} options={files} />
        ) : (
          <span className="mono">{spec.path}</span>
        )}
        {file?.exists && (
          <>
            <span className="chip" title={t("docker.ed.ownerMode")}>
              {file.owner} · {file.mode}
            </span>
            {!file.writable && <span className="chip docker-chip-warn">{t("docker.ed.viaSudo")}</span>}
          </>
        )}
        {file && !file.exists && <span className="chip">{t("docker.ed.newFile")}</span>}
        <div className="grow" />
        <span className="muted">{t("docker.ed.projectDir", { dir: spec.workingDir || "" })}</span>
      </div>
      {loadErr ? (
        <div className="hint-box err">{loadErr}</div>
      ) : !file ? (
        <Loading />
      ) : (
        <div className="docker-ed-main">
          <CodeEditor value={text} onChange={setText} language="yaml" readOnly={readOnly} height="100%" onSubmit={canEdit ? validate : undefined} />
          {result && (
            <div className={`docker-validate ${result.ok ? "ok" : "err"}`}>
              <div className="docker-validate-head">
                {result.ok ? <CheckCircle2 size={14} /> : <AlertTriangle size={14} />}
                {result.ok ? t("docker.ed.valid") : t("docker.ed.invalid")}
                <div className="grow" />
                <button className="btn sm ghost" onClick={() => setResult(null)}>
                  {t("common.hide")}
                </button>
              </div>
              {result.output && <pre className="docker-pre">{result.output}</pre>}
            </div>
          )}
        </div>
      )}
      {history && file && (
        <HistoryModal
          connId={connId}
          path={spec.path}
          current={file.exists ? file.content : ""}
          canEdit={canEdit}
          onClose={() => setHistory(false)}
          onRestore={async (v, content) => {
            const done = await deploy(content, "rollback", v.id);
            if (done) setHistory(false);
          }}
        />
      )}
    </BigModal>
  );
}

function HistoryModal(props: {
  connId: string;
  path: string;
  current: string;
  canEdit: boolean;
  onClose: () => void;
  onRestore: (v: ComposeVersion, content: string) => Promise<void>;
}) {
  const t = useT();
  const list = useRemote(() => DockerService.ComposeHistory(props.connId, props.path), [props.connId, props.path]);
  const [sel, setSel] = useState<ComposeVersion | null>(null);
  const [content, setContent] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!sel) return;
    setContent(null);
    DockerService.ComposeVersionContent(props.connId, props.path, sel.id)
      .then(setContent)
      .catch((e) => toast(errMsg(e), "error"));
  }, [sel?.id]);

  useEffect(() => {
    const first = list.data?.[0];
    if (first && !sel) setSel(first);
  }, [list.data]);

  const versions = list.data ?? [];
  return (
    <BigModal
      className="docker-history"
      onClose={props.onClose}
      title={
        <>
          <History size={16} /> {t("docker.h.title")} <span className="muted mono">{props.path}</span>
        </>
      }
      footer={
        <>
          <span className="left muted">{t("docker.h.hint")}</span>
          {props.canEdit && (
            <button
              className="btn danger"
              disabled={!sel || content === null || busy || content === props.current}
              onClick={async () => {
                if (!sel || content === null) return;
                setBusy(true);
                try {
                  await props.onRestore(sel, content);
                } finally {
                  setBusy(false);
                }
              }}
            >
              <Undo2 size={13} /> {t("docker.h.restore")}
            </button>
          )}
          <button className="btn" onClick={props.onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      {list.error ? (
        <div className="hint-box err">{list.error}</div>
      ) : !list.data ? (
        <Loading />
      ) : versions.length === 0 ? (
        <div className="docker-empty-small muted">{t("docker.h.empty")}</div>
      ) : (
        <div className="docker-history-main">
          <div className="docker-history-list">
            {versions.map((v) => (
              <div key={v.id} className={`list-row clickable ${sel?.id === v.id ? "docker-sel" : ""}`} onClick={() => setSel(v)}>
                <div className="grow">
                  <div>{fullDate(Math.floor(v.ts / 1000))}</div>
                  <div className="muted docker-sub">
                    {v.actor} · <span className="mono">{v.sha.slice(0, 10)}</span> · {formatBytes(v.size)}
                  </div>
                  <div className="muted docker-sub">{t(v.note === "rollback" ? "docker.h.noteRollback" : "docker.h.noteSave")}</div>
                </div>
              </div>
            ))}
          </div>
          <div className="docker-history-diff">
            <div className="docker-diff-head">
              <span>{t("docker.h.current")}</span>
              <span>{sel ? t("docker.h.version", { date: fullDate(Math.floor(sel.ts / 1000)) }) : ""}</span>
            </div>
            {content === null ? <Loading /> : <DiffView original={props.current} modified={content} />}
          </div>
        </div>
      )}
    </BigModal>
  );
}

/** Side-by-side read-only diff (Monaco diff editor). */
export function DiffView({ original, modified, language = "yaml" }: { original: string; modified: string; language?: string }) {
  const host = useRef<HTMLDivElement>(null);
  const ed = useRef<monaco.editor.IStandaloneDiffEditor | null>(null);
  useEffect(() => {
    const e = monaco.editor.createDiffEditor(host.current!, {
      theme: "vs-dark",
      readOnly: true,
      originalEditable: false,
      automaticLayout: true,
      renderSideBySide: true,
      minimap: { enabled: false },
      fontSize: 12.5,
      scrollBeyondLastLine: false,
    });
    ed.current = e;
    return () => {
      const m = e.getModel();
      e.dispose();
      m?.original.dispose();
      m?.modified.dispose();
    };
  }, []);
  useEffect(() => {
    const e = ed.current;
    if (!e) return;
    const old = e.getModel();
    e.setModel({ original: monaco.editor.createModel(original, language), modified: monaco.editor.createModel(modified, language) });
    old?.original.dispose();
    old?.modified.dispose();
  }, [original, modified, language]);
  return <div className="code-editor docker-diff" ref={host} />;
}
