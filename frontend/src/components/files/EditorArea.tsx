import { useEffect, useRef, useState } from "react";
import { Download, FileWarning, Lock, RotateCw, Save, ShieldAlert, WrapText, X } from "lucide-react";
import { FileService, errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { languages, monaco } from "../../lib/monaco";
import {
  closeAllTabs,
  closeTab,
  getModel,
  reloadFile,
  saveFile,
  setActive,
  setLanguage,
  useEditor,
  type EditorTab,
} from "../../store/editor";
import { toast, useUI } from "../../store/ui";
import { FileIcon } from "./icons";
import { configKind, saveAndApply } from "../../store/configApply";
import { useT } from "../../i18n";
import { Select } from "../../ui/Select";

// Read on each render: languages registered later (nginx, Caddyfile) show up.
const langList = () => languages();

function loadWrap() {
  try {
    return localStorage.getItem("sm.wordWrap") === "1";
  } catch {
    return false;
  }
}

export function EditorArea({ connId, visible }: { connId: string; visible: boolean }) {
  const t = useT();
  const conn = useEditor((s) => s.byConn[connId]);
  const tabs = conn?.tabs ?? [];
  const active = tabs.find((t) => t.path === conn?.active);
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);
  const viewStates = useRef(new Map<string, monaco.editor.ICodeEditorViewState | null>());
  const shownPath = useRef<string | null>(null);
  const [cursor, setCursor] = useState({ line: 1, col: 1, sel: 0 });
  const [wrap, setWrap] = useState(loadWrap);
  const [eol, setEol] = useState<"LF" | "CRLF">("LF");
  const showMenu = useUI((s) => s.showMenu);
  const activeRef = useRef(active);
  activeRef.current = active;

  const hostRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const editor = monaco.editor.create(hostRef.current!, {
      model: null,
      theme: "vs-dark",
      fontSize: 13,
      fontFamily: "'SF Mono', Menlo, Monaco, Consolas, monospace",
      minimap: { enabled: true, scale: 1 },
      automaticLayout: true,
      scrollBeyondLastLine: false,
      smoothScrolling: true,
      renderWhitespace: "selection",
      bracketPairColorization: { enabled: true },
      wordWrap: loadWrap() ? "on" : "off",
      tabSize: 4,
      detectIndentation: true,
      stickyScroll: { enabled: true },
      fixedOverflowWidgets: true,
    });
    onMount(editor);
    return () => {
      // Detach first so disposing the editor never touches file models.
      editor.setModel(null);
      editor.dispose();
      editorRef.current = null;
    };
  }, []);

  const onMount = (editor: monaco.editor.IStandaloneCodeEditor) => {
    editorRef.current = editor;
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => {
      const t = activeRef.current;
      if (t) saveFile(connId, t.path);
    });
    editor.onDidChangeCursorSelection((e) => {
      const s = e.selection;
      const model = editor.getModel();
      setCursor({
        line: s.positionLineNumber,
        col: s.positionColumn,
        sel: model ? model.getValueLengthInRange(s) : 0,
      });
    });
    syncModel();
  };

  // Swap the model shown by the single editor instance, remembering each
  // file's scroll/cursor state.
  const syncModel = () => {
    const editor = editorRef.current;
    if (!editor) return;
    const t = activeRef.current;
    const model = t && t.state === "ready" ? getModel(connId, t.path) : null;
    if (shownPath.current && shownPath.current !== t?.path) {
      viewStates.current.set(shownPath.current, editor.saveViewState());
    }
    if (editor.getModel() !== model) {
      editor.setModel(model);
      if (model && t) {
        const vs = viewStates.current.get(t.path);
        if (vs) editor.restoreViewState(vs);
        setEol(model.getEOL() === "\r\n" ? "CRLF" : "LF");
      }
    }
    shownPath.current = t?.path ?? null;
    if (model && t) editor.updateOptions({ readOnly: false });
  };

  useEffect(syncModel, [active?.path, active?.state]);

  // Jump to a line (from search results).
  useEffect(() => {
    const editor = editorRef.current;
    if (!editor || !active?.reveal || active.state !== "ready") return;
    const line = active.reveal.line;
    requestAnimationFrame(() => {
      editor.revealLineInCenter(line);
      editor.setPosition({ lineNumber: line, column: 1 });
      editor.focus();
    });
  }, [active?.reveal?.nonce, active?.state]);

  useEffect(() => {
    if (visible) requestAnimationFrame(() => editorRef.current?.layout());
  }, [visible]);

  useEffect(() => {
    editorRef.current?.updateOptions({ wordWrap: wrap ? "on" : "off" });
    try {
      localStorage.setItem("sm.wordWrap", wrap ? "1" : "0");
    } catch {
      /* ignore */
    }
  }, [wrap]);

  const tabMenu = (e: React.MouseEvent, tab: EditorTab) => {
    e.preventDefault();
    showMenu(e.clientX, e.clientY, [
      { label: t("ed.close"), onClick: () => closeTab(connId, tab.path) },
      { label: t("ed.closeOthers"), onClick: () => closeAllTabs(connId, tab.path) },
      { label: t("ed.closeAll"), onClick: () => closeAllTabs(connId) },
      { separator: true },
      { label: t("ed.reloadFromServer"), onClick: () => reloadFile(connId, tab.path) },
      {
        label: t("ed.copyPath"),
        onClick: () => {
          navigator.clipboard.writeText(tab.path);
          toast(t("common.copied"));
        },
      },
    ]);
  };

  const changeEol = (v: "LF" | "CRLF") => {
    const m = active && getModel(connId, active.path);
    if (!m) return;
    m.pushEOL(v === "LF" ? monaco.editor.EndOfLineSequence.LF : monaco.editor.EndOfLineSequence.CRLF);
    setEol(v);
  };

  const download = async (tab: EditorTab) => {
    try {
      await FileService.PickAndDownload(connId, tab.path, false, t("fx.pickSaveDir"));
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <div className="editor-area">
      {tabs.length > 0 && (
        <div className="tabs">
          {tabs.map((tab) => (
            <div
              key={tab.path}
              className={`tab ${tab.path === active?.path ? "active" : ""} ${tab.dirty ? "dirty" : ""}`}
              onClick={() => setActive(connId, tab.path)}
              onMouseDown={(e) => {
                if (e.button === 1) {
                  e.preventDefault();
                  closeTab(connId, tab.path);
                }
              }}
              onContextMenu={(e) => tabMenu(e, tab)}
              title={tab.path}
            >
              <FileIcon name={tab.name} isDir={false} size={14} />
              <span className="label">{tab.name}</span>
              <span
                className="close"
                onClick={(e) => {
                  e.stopPropagation();
                  closeTab(connId, tab.path);
                }}
              >
                <X size={13} className="x" />
                <span className="d" />
              </span>
            </div>
          ))}
        </div>
      )}

      {active && (
        <div className="breadcrumb">
          <span className="path">{active.path}</span>
          {active.sudo && (
            <span className="badge warn" title={t("ed.sudoTip")}>
              <ShieldAlert size={11} /> sudo
            </span>
          )}
          {active.readOnly && !active.sudo && (
            <span className="badge warn" title={t("ed.readOnlyTip")}>
              <Lock size={11} /> {t("ed.readOnly")}
            </span>
          )}
          {active.dirty && <span className="badge">{t("ed.unsaved")}</span>}
          <button className="icon-btn" title={t("ed.reloadFromServer")} onClick={() => reloadFile(connId, active.path)}>
            <RotateCw size={13} />
          </button>
          <button className="icon-btn" title={t("ed.saveTip")} disabled={!active.dirty} onClick={() => saveFile(connId, active.path)}>
            <Save size={14} />
          </button>
          {(() => {
            const ck = configKind(active.path);
            return ck ? (
              <button className="btn sm" title={t("app.ed.saveApplyTip")} onClick={() => saveAndApply(connId, active.path, ck)}>
                <Save size={12} /> {t("app.ed.saveApply", { name: ck.label })}
              </button>
            ) : null;
          })()}
        </div>
      )}

      <div className="editor-host">
        <div style={{ position: "absolute", inset: 0, visibility: active?.state === "ready" ? "visible" : "hidden" }}>
          <div ref={hostRef} style={{ position: "absolute", inset: 0 }} />
        </div>
        {!active && (
          <div className="center-msg">
            <FileIcon name="x.txt" isDir={false} size={40} />
            <div>{t("ed.empty")}</div>
            <div className="muted" style={{ fontSize: 12 }}>
              {t("ed.emptyHint")}
            </div>
          </div>
        )}
        {active?.state === "loading" && (
          <div className="center-msg">
            <span className="spinner lg" />
            {t("ed.opening", { name: active.name })}
          </div>
        )}
        {active?.state === "error" && (
          <div className="center-msg">
            <FileWarning size={36} color="var(--err)" />
            <div className="err">{active.error}</div>
            <button className="btn" onClick={() => reloadFile(connId, active.path)}>
              {t("common.retry")}
            </button>
          </div>
        )}
        {(active?.state === "binary" || active?.state === "tooLarge") && (
          <div className="center-msg">
            <FileWarning size={36} color="var(--warn)" />
            <div>
              {active.state === "binary" ? t("ed.binary") : t("ed.tooLarge", { size: formatBytes(active.size) })}
            </div>
            <button className="btn" onClick={() => download(active)}>
              <Download size={14} /> {t("ed.downloadLocal")}
            </button>
          </div>
        )}
      </div>

      {active?.state === "ready" && (
        <div className="statusbar">
          <span>
            {t("ed.cursor", { line: cursor.line, col: cursor.col })}
            {cursor.sel > 0 ? " " + t("ed.selected", { n: cursor.sel }) : ""}
          </span>
          <span className="grow" />
          <span>{formatBytes(active.size)}</span>
          <Select ghost value={eol} onChange={changeEol} title={t("ed.eol")} options={["LF", "CRLF"]} />
          <span>UTF-8</span>
          <Select ghost value={active.language} onChange={(v) => setLanguage(connId, active.path, v)} title={t("ed.language")} options={langList().map((l) => ({ value: l.id, label: l.name }))} />
          <button className={`icon-btn ${wrap ? "active" : ""}`} style={{ width: 20, height: 20 }} title={t("ed.wrap")} onClick={() => setWrap((w) => !w)}>
            <WrapText size={13} />
          </button>
        </div>
      )}
    </div>
  );
}
