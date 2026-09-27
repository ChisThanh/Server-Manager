import type { ReactNode } from "react";
import { create } from "zustand";
import { t } from "../i18n";

export type ToastKind = "info" | "success" | "error";
export interface Toast {
  id: number;
  kind: ToastKind;
  text: string;
}

export interface DialogField {
  name: string;
  label: string;
  type?: "text" | "password" | "checkbox" | "textarea";
  value?: string | boolean;
  placeholder?: string;
  autoFocus?: boolean;
  /** Returns an error message, or null when valid. */
  validate?: (v: string) => string | null;
}

export interface DialogSpec {
  title: string;
  message?: ReactNode;
  fields?: DialogField[];
  confirmText?: string;
  cancelText?: string;
  danger?: boolean;
  /** Extra buttons, each resolving the dialog with its id. */
  extra?: { id: string; label: string; danger?: boolean }[];
  wide?: boolean;
}

export type DialogResult = { action: string; values: Record<string, string | boolean> } | null;

interface OpenDialog extends DialogSpec {
  id: number;
  resolve: (r: DialogResult) => void;
}

export interface MenuItem {
  label?: string;
  icon?: ReactNode;
  shortcut?: string;
  danger?: boolean;
  disabled?: boolean;
  separator?: boolean;
  onClick?: () => void;
}

interface UIState {
  toasts: Toast[];
  dialogs: OpenDialog[];
  menu: { x: number; y: number; items: MenuItem[] } | null;
  toast: (text: string, kind?: ToastKind) => void;
  dismissToast: (id: number) => void;
  openDialog: (spec: DialogSpec) => Promise<DialogResult>;
  closeDialog: (id: number, r: DialogResult) => void;
  showMenu: (x: number, y: number, items: MenuItem[]) => void;
  hideMenu: () => void;
}

let seq = 1;

export const useUI = create<UIState>((set, get) => ({
  toasts: [],
  dialogs: [],
  menu: null,
  toast: (text, kind = "info") => {
    const id = seq++;
    set((s) => ({ toasts: [...s.toasts.slice(-4), { id, kind, text }] }));
    setTimeout(() => get().dismissToast(id), kind === "error" ? 7000 : 3500);
  },
  dismissToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
  openDialog: (spec) =>
    new Promise((resolve) => {
      const id = seq++;
      set((s) => ({ dialogs: [...s.dialogs, { ...spec, id, resolve }] }));
    }),
  closeDialog: (id, r) => {
    const d = get().dialogs.find((x) => x.id === id);
    set((s) => ({ dialogs: s.dialogs.filter((x) => x.id !== id) }));
    d?.resolve(r);
  },
  showMenu: (x, y, items) => set({ menu: { x, y, items } }),
  hideMenu: () => set({ menu: null }),
}));

// Convenience helpers usable outside React components.
export const toast = (text: string, kind?: ToastKind) => useUI.getState().toast(text, kind);

export async function confirmDialog(title: string, message?: ReactNode, confirmText = t("common.ok"), danger = false) {
  const r = await useUI.getState().openDialog({ title, message, confirmText, danger });
  return r?.action === "ok";
}

export async function promptDialog(
  title: string,
  label: string,
  value = "",
  opts: Partial<DialogField> & { message?: ReactNode; confirmText?: string } = {},
): Promise<string | null> {
  const { message, confirmText, ...field } = opts;
  const r = await useUI.getState().openDialog({
    title,
    message,
    confirmText,
    fields: [{ name: "v", label, value, autoFocus: true, ...field }],
  });
  if (r?.action !== "ok") return null;
  return String(r.values.v ?? "");
}
