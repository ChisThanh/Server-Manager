import type { ReactNode } from "react";
import { SecurityService } from "../../../bindings/server-manager/services/security";
import { useApp } from "../../store/app";
import { useSettings } from "../../store/settings";
import { withSudo } from "../../store/sudo";
import { toast, useUI, type DialogField } from "../../store/ui";
import { errCode, errMsg } from "../../lib/api";
import { formatDate } from "../../lib/format";
import { hasKey, t, type Params, type Key } from "../../i18n";

export { SecurityService };
export type * from "../../../bindings/server-manager/services/security/models";

export type SecTab = "overview" | "ssh" | "keys" | "firewall" | "access" | "ports" | "events" | "users";
export const SEC_TABS: SecTab[] = ["overview", "ssh", "keys", "firewall", "access", "ports", "events", "users"];

/** Prefill for a new firewall rule (from Open ports / Events). */
export interface RulePrefill {
  action?: "allow" | "deny";
  port?: string;
  proto?: string;
  from?: string;
  comment?: string;
  seq: number;
}

/** Props shared by the tabs of the security panel. */
export interface SecViewProps {
  connId: string;
  /** The tab is on screen and the connection is up. */
  active: boolean;
  canSec: boolean;
  canUsers: boolean;
  go: (tab: SecTab, opts?: { user?: string; rule?: Omit<RulePrefill, "seq"> }) => void;
}

/** Calls a SecurityService method with the sudo password (prompting if needed). */
export function sudo<T>(connId: string, fn: (pw: string) => Promise<T>): Promise<T> {
  return withSudo(connId, fn);
}

/** Translates a dynamic key, with a fallback when it doesn't exist. */
export function tk(key: string, params?: Params, fallback = ""): string {
  return hasKey(key) ? t(key as Key, params) : fallback || key;
}

/** Runs a change, toasting success/failure. Returns true on success. */
export async function act(fn: () => Promise<unknown>, okMsg?: string): Promise<boolean> {
  try {
    await fn();
    if (okMsg) toast(okMsg, "success");
    return true;
  } catch (e) {
    if (errMsg(e) !== t("sudo.cancelled")) toast(errMsg(e), "error");
    return false;
  }
}

export function isCancelled(e: unknown) {
  return errMsg(e) === t("sudo.cancelled");
}

export { errCode, errMsg };

export function fmtTime(unix: number): string {
  return unix ? formatDate(unix) : "—";
}

/** Asks for confirmation, with optional checkboxes; on production servers
 *  (setting on) the server name must be typed. `typeWord` always requires
 *  typing that word. Returns the checkbox values or null when cancelled. */
export async function confirmOpts(opts: {
  serverId: string;
  title: string;
  message?: ReactNode;
  confirmText: string;
  danger?: boolean;
  typeWord?: string;
  options?: { name: string; label: string; value?: boolean }[];
}): Promise<Record<string, boolean> | null> {
  const server = useApp.getState().servers.find((s) => s.id === opts.serverId);
  const danger = opts.danger ?? true;
  const strict = danger && server?.environment === "production" && useSettings.getState().settings.confirmProduction;
  const fields: DialogField[] = (opts.options ?? []).map((o) => ({ name: o.name, label: o.label, type: "checkbox", value: !!o.value }));
  if (opts.typeWord) {
    const w = opts.typeWord;
    fields.push({
      name: "__word",
      label: t("sec.typeToConfirm", { word: w }),
      placeholder: w,
      autoFocus: true,
      validate: (v) => (v.trim() === w ? null : t("sec.wordMismatch")),
    });
  } else if (strict && server) {
    fields.push({
      name: "__name",
      label: t("app.typeName"),
      placeholder: server.name,
      autoFocus: true,
      validate: (v) => (v.trim() === server.name ? null : t("app.nameMismatch")),
    });
  }
  const r = await useUI.getState().openDialog({
    title: opts.title,
    message: (
      <>
        {opts.message}
        {strict && server && !opts.typeWord ? <div className="prod-warn">{t("app.prodConfirm", { name: server.name })}</div> : null}
      </>
    ),
    confirmText: opts.confirmText,
    danger,
    fields,
  });
  if (r?.action !== "ok") return null;
  const out: Record<string, boolean> = {};
  for (const o of opts.options ?? []) out[o.name] = !!r.values[o.name];
  return out;
}

/** Status → badge tone. */
export function statusTone(s: string): "ok" | "warn" | "err" | "info" | "muted" {
  switch (s) {
    case "ok":
      return "ok";
    case "warn":
      return "warn";
    case "crit":
      return "err";
    case "info":
      return "info";
  }
  return "muted";
}

/** Simple client-side validators (the backend re-validates everything). */
export const reUser = /^[a-z_][a-z0-9_-]{0,31}$/;

export function validPortSpec(v: string, allowEmpty = true): boolean {
  const s = v.trim();
  if (!s) return allowEmpty;
  return s.split(",").every((p) => {
    const m = /^(\d{1,5})(?:[:-](\d{1,5}))?$/.exec(p.trim());
    if (!m) return false;
    const a = +m[1];
    const b = m[2] ? +m[2] : a;
    return a >= 1 && b <= 65535 && a <= b;
  });
}

export function validAddr(v: string): boolean {
  const s = v.trim();
  if (!s || s === "any") return true;
  const [ip, mask] = s.split("/");
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(ip);
  if (v4) {
    if (v4.slice(1).some((x) => +x > 255)) return false;
    return mask === undefined || (/^\d{1,2}$/.test(mask) && +mask <= 32);
  }
  if (/^[0-9a-fA-F:]+$/.test(ip) && ip.includes(":")) return mask === undefined || (/^\d{1,3}$/.test(mask) && +mask <= 128);
  return false;
}

/** Params of a coded backend error ({port}, {user}…). */
export function errParams(e: unknown): Record<string, string> {
  const cause = (e as { cause?: { params?: Record<string, string | undefined> | null } } | null)?.cause;
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(cause?.params ?? {})) out[k] = v ?? "";
  return out;
}

// ---- IP/CIDR helpers (display only; the backend decides) ----

function v4num(ip: string): number | null {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(ip);
  if (!m) return null;
  const p = m.slice(1).map(Number);
  if (p.some((x) => x > 255)) return null;
  return ((p[0] << 24) >>> 0) + (p[1] << 16) + (p[2] << 8) + p[3];
}

/** Whether outer (IP or CIDR) contains inner (IP or CIDR). IPv6 compares
 *  exact addresses only. */
export function addrCovers(outer: string, inner: string): boolean {
  if (outer === inner) return true;
  const [oip, omask] = outer.split("/");
  const [iip, imask] = inner.split("/");
  const o = v4num(oip);
  const i = v4num(iip);
  if (o === null || i === null) return oip.toLowerCase() === iip.toLowerCase() && omask === imask;
  const ob = omask === undefined ? 32 : +omask;
  const ib = imask === undefined ? 32 : +imask;
  if (ib < ob) return false;
  const mask = ob === 0 ? 0 : (~0 << (32 - ob)) >>> 0;
  return ((o & mask) >>> 0) === ((i & mask) >>> 0);
}

/** The /24 network of an IPv4 address ("" for IPv6). */
export function net24(ip: string): string {
  const n = v4num(ip);
  if (n === null) return "";
  return `${(n >>> 24) & 255}.${(n >>> 16) & 255}.${(n >>> 8) & 255}.0/24`;
}

/** Splits pasted text into address tokens (spaces, commas, new lines). */
export function addrTokens(text: string): string[] {
  return Array.from(new Set(text.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean)));
}

/** "1h", "1d"… for a number of seconds (-1 = forever). */
export function fmtSpan(sec: number): string {
  if (sec < 0) return t("sec.acc.dur.forever");
  if (sec % 604800 === 0) return t("sec.acc.dur.w", { n: sec / 604800 });
  if (sec % 86400 === 0) return t("sec.acc.dur.d", { n: sec / 86400 });
  if (sec % 3600 === 0) return t("sec.acc.dur.h", { n: sec / 3600 });
  return t("sec.acc.dur.m", { n: Math.round(sec / 60) });
}
