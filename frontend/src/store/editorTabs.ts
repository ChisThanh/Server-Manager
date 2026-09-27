import { create } from "zustand";

// Editor tab state without Monaco, so the app shell can check for unsaved
// files without loading the editor.

export type TabState = "loading" | "ready" | "binary" | "tooLarge" | "error";

export interface EditorTab {
  path: string;
  name: string;
  language: string;
  modTime: number;
  size: number;
  /** Loaded through sudo, so saves go through sudo too. */
  sudo: boolean;
  /** The login user can't write it without sudo. */
  readOnly: boolean;
  dirty: boolean;
  state: TabState;
  error?: string;
  reveal?: { line: number; nonce: number };
}

export interface ConnEditors {
  tabs: EditorTab[];
  active: string | null;
}

export interface EditorState {
  byConn: Record<string, ConnEditors>;
}

export const useEditor = create<EditorState>(() => ({ byConn: {} }));


export function hasDirtyTabs(connId?: string) {
  const all = useEditor.getState().byConn;
  const conns = connId ? [all[connId]] : Object.values(all);
  return conns.some((c) => c?.tabs.some((t) => t.dirty));
}

interface EditorHooks {
  disposeConn(connId: string): void;
}
let hooks: EditorHooks | null = null;

/** Called by store/editor once Monaco is loaded. */
export function registerEditorHooks(h: EditorHooks) {
  hooks = h;
}

/** Forgets a connection's tabs (and disposes their editor models). */
export function dropConn(connId: string) {
  hooks?.disposeConn(connId);
  useEditor.setState((s) => {
    const byConn = { ...s.byConn };
    delete byConn[connId];
    return { byConn };
  });
}
