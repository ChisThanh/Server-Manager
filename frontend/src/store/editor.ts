import { FileService, errMsg, isPermissionError, type FileContent, type SaveResult } from "../lib/api";
import { basename } from "../lib/format";
import { languageFor, modelUri, monaco } from "../lib/monaco";
import { withSudo } from "./sudo";
import { t } from "../i18n";
import { confirmDialog, toast, useUI } from "./ui";

import { useEditor, registerEditorHooks, type ConnEditors, type EditorTab } from "./editorTabs";
export { useEditor, hasDirtyTabs, type EditorTab, type TabState } from "./editorTabs";

const savedVersions = new Map<string, number>();
const listeners = new Map<string, monaco.IDisposable>();
const key = (connId: string, path: string) => connId + "\u0000" + path;

function patchConn(connId: string, fn: (c: ConnEditors) => ConnEditors) {
  useEditor.setState((s) => ({
    byConn: { ...s.byConn, [connId]: fn(s.byConn[connId] ?? { tabs: [], active: null }) },
  }));
}

function patchTab(connId: string, path: string, patch: Partial<EditorTab>) {
  patchConn(connId, (c) => ({ ...c, tabs: c.tabs.map((t) => (t.path === path ? { ...t, ...patch } : t)) }));
}

export function getTab(connId: string, path: string) {
  return useEditor.getState().byConn[connId]?.tabs.find((t) => t.path === path);
}

export function getModel(connId: string, path: string) {
  return monaco.editor.getModel(modelUri(connId, path));
}

function setModel(connId: string, path: string, content: string, language: string) {
  const uri = modelUri(connId, path);
  let model = monaco.editor.getModel(uri);
  if (model) {
    model.pushEditOperations([], [{ range: model.getFullModelRange(), text: content }], () => null);
    model.pushStackElement();
  } else {
    model = monaco.editor.createModel(content, language, uri);
  }
  const k = key(connId, path);
  savedVersions.set(k, model.getAlternativeVersionId());
  listeners.get(k)?.dispose();
  listeners.set(
    k,
    model.onDidChangeContent(() => {
      const dirty = model!.getAlternativeVersionId() !== savedVersions.get(k);
      if (getTab(connId, path)?.dirty !== dirty) patchTab(connId, path, { dirty });
    }),
  );
  return model;
}

function disposeModel(connId: string, path: string) {
  const k = key(connId, path);
  listeners.get(k)?.dispose();
  listeners.delete(k);
  savedVersions.delete(k);
  getModel(connId, path)?.dispose();
}

function applyContent(connId: string, path: string, fc: FileContent) {
  if (fc.binary) {
    patchTab(connId, path, { state: "binary", size: fc.size, modTime: fc.modTime });
    return;
  }
  if (fc.tooLarge) {
    patchTab(connId, path, { state: "tooLarge", size: fc.size, modTime: fc.modTime });
    return;
  }
  const tab = getTab(connId, path);
  setModel(connId, path, fc.content, tab?.language ?? languageFor(path));
  patchTab(connId, path, {
    state: "ready",
    size: fc.size,
    modTime: fc.modTime,
    sudo: fc.sudo,
    readOnly: fc.readOnly,
    dirty: false,
    error: undefined,
  });
}

async function load(connId: string, path: string) {
  try {
    const fc = await FileService.ReadFile(connId, path);
    applyContent(connId, path, fc);
  } catch (e) {
    if (isPermissionError(e)) {
      const ok = await confirmDialog(t("ed.noReadTitle"), t("ed.openSudoQ", { path }), t("ed.openSudo"));
      if (ok) {
        try {
          const fc = await withSudo(connId, (pw) => FileService.ReadFileSudo(connId, path, pw));
          applyContent(connId, path, fc);
          return;
        } catch (e2) {
          patchTab(connId, path, { state: "error", error: errMsg(e2) });
          return;
        }
      }
    }
    patchTab(connId, path, { state: "error", error: errMsg(e) });
  }
}

export async function openFile(connId: string, path: string, line?: number) {
  const existing = getTab(connId, path);
  const reveal = line ? { line, nonce: Date.now() } : undefined;
  if (existing) {
    patchConn(connId, (c) => ({ ...c, active: path }));
    if (reveal) patchTab(connId, path, { reveal });
    if (existing.state === "error") {
      patchTab(connId, path, { state: "loading" });
      await load(connId, path);
    }
    return;
  }
  const tab: EditorTab = {
    path,
    name: basename(path),
    language: languageFor(path),
    modTime: 0,
    size: 0,
    sudo: false,
    readOnly: false,
    dirty: false,
    state: "loading",
    reveal,
  };
  patchConn(connId, (c) => ({ tabs: [...c.tabs, tab], active: path }));
  await load(connId, path);
}

export function setActive(connId: string, path: string) {
  patchConn(connId, (c) => ({ ...c, active: path }));
}

export function setLanguage(connId: string, path: string, language: string) {
  const m = getModel(connId, path);
  if (m) monaco.editor.setModelLanguage(m, language);
  patchTab(connId, path, { language });
}

export async function reloadFile(connId: string, path: string) {
  const tab = getTab(connId, path);
  if (!tab) return;
  if (tab.dirty && !(await confirmDialog(t("ed.reloadTitle"), t("ed.reloadMsg"), t("ed.reload"), true))) return;
  patchTab(connId, path, { state: "loading" });
  if (tab.sudo) {
    try {
      applyContent(connId, path, await withSudo(connId, (pw) => FileService.ReadFileSudo(connId, path, pw)));
    } catch (e) {
      patchTab(connId, path, { state: "error", error: errMsg(e) });
    }
    return;
  }
  await load(connId, path);
}

function markSaved(connId: string, path: string, r: SaveResult) {
  const m = getModel(connId, path);
  if (m) savedVersions.set(key(connId, path), m.getAlternativeVersionId());
  patchTab(connId, path, { dirty: false, modTime: r.entry.modTime, size: r.entry.size });
}

export async function saveFile(connId: string, path: string, force = false): Promise<boolean> {
  const tab = getTab(connId, path);
  const model = getModel(connId, path);
  if (!tab || !model || tab.state !== "ready") return false;
  const content = model.getValue();
  const versionAtSave = model.getAlternativeVersionId();
  const write = (sudo: boolean) =>
    sudo
      ? withSudo(connId, (pw) => FileService.WriteFileSudo(connId, path, content, pw, tab.modTime, force))
      : FileService.WriteFile(connId, path, content, tab.modTime, force);

  try {
    let r: SaveResult;
    try {
      r = await write(tab.sudo);
    } catch (e) {
      if (tab.sudo || !isPermissionError(e)) throw e;
      const ok = await confirmDialog(t("ed.noWriteTitle"), t("ed.saveSudoQ", { path }), t("ed.saveSudo"));
      if (!ok) return false;
      r = await write(true);
      patchTab(connId, path, { sudo: true });
    }
    if (r.conflict) {
      const res = await useUI.getState().openDialog({
        title: t("ed.conflictTitle"),
        message: t("ed.conflictMsg", { path }),
        confirmText: t("ed.overwrite"),
        danger: true,
        extra: [{ id: "reload", label: t("ed.reloadFromServer") }],
      });
      if (res?.action === "ok") return saveFile(connId, path, true);
      if (res?.action === "reload") {
        savedVersions.set(key(connId, path), -1);
        patchTab(connId, path, { dirty: false });
        await reloadFile(connId, path);
      }
      return false;
    }
    // Only mark clean if nothing was typed while the save was in flight.
    if (model.getAlternativeVersionId() === versionAtSave) {
      markSaved(connId, path, r);
    } else {
      savedVersions.set(key(connId, path), versionAtSave);
      patchTab(connId, path, { modTime: r.entry.modTime, size: r.entry.size });
    }
    toast(t("ed.saved", { name: tab.name }), "success");
    return true;
  } catch (e) {
    toast(t("ed.saveFailed", { error: errMsg(e) }), "error");
    return false;
  }
}

export async function closeTab(connId: string, path: string) {
  const tab = getTab(connId, path);
  if (!tab) return;
  if (tab.dirty) {
    const res = await useUI.getState().openDialog({
      title: t("ed.saveChangesTitle"),
      message: t("ed.unsavedMsg", { name: tab.name }),
      confirmText: t("common.save"),
      extra: [{ id: "discard", label: t("ed.dontSave"), danger: true }],
    });
    if (!res) return;
    if (res.action === "ok" && !(await saveFile(connId, path))) return;
  }
  removeTabs(connId, (p) => p === path);
}

export async function closeAllTabs(connId: string, except?: string) {
  const tabs = useEditor.getState().byConn[connId]?.tabs ?? [];
  for (const t of tabs) {
    if (t.path !== except) await closeTab(connId, t.path);
  }
}

function removeTabs(connId: string, match: (p: string) => boolean) {
  const c = useEditor.getState().byConn[connId];
  if (!c) return;
  const removed = c.tabs.filter((t) => match(t.path));
  if (removed.length === 0) return;
  removed.forEach((t) => disposeModel(connId, t.path));
  patchConn(connId, (c) => {
    const idx = c.tabs.findIndex((t) => t.path === c.active);
    const tabs = c.tabs.filter((t) => !match(t.path));
    let active = c.active;
    if (active && match(active)) active = tabs[Math.min(Math.max(idx, 0), tabs.length - 1)]?.path ?? null;
    return { tabs, active };
  });
}

/** Closes tabs for a deleted file or anything under a deleted folder. */
export function forgetPath(connId: string, path: string) {
  removeTabs(connId, (p) => p === path || p.startsWith(path + "/"));
}

/** Keeps open tabs (and unsaved edits) pointing at the right place after a rename/move. */
export function renamePath(connId: string, from: string, to: string) {
  const c = useEditor.getState().byConn[connId];
  if (!c) return;
  const affected = c.tabs.filter((t) => t.path === from || t.path.startsWith(from + "/"));
  for (const t of affected) {
    const np = to + t.path.slice(from.length);
    const old = getModel(connId, t.path);
    const k = key(connId, t.path);
    if (old) {
      const value = old.getValue();
      const wasDirty = t.dirty;
      disposeModel(connId, t.path);
      const m = setModel(connId, np, value, t.language);
      if (wasDirty) savedVersions.set(key(connId, np), -1);
      void m;
    } else {
      listeners.get(k)?.dispose();
    }
    patchConn(connId, (c) => ({
      active: c.active === t.path ? np : c.active,
      tabs: c.tabs.map((x) => (x.path === t.path ? { ...x, path: np, name: basename(np) } : x)),
    }));
  }
}

// Lets the (Monaco-free) tab store dispose models when a connection closes.
registerEditorHooks({
  disposeConn(connId: string) {
    const c = useEditor.getState().byConn[connId];
    c?.tabs.forEach((t) => disposeModel(connId, t.path));
  },
});
