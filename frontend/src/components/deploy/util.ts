import type { App, Run, Step } from "../../../bindings/server-manager/services/deploy/models";
import type { Key } from "../../i18n";
import { t } from "../../i18n";

export const STEP_IDS = ["preflight", "fetch", "env", "build", "test", "restart", "health", "done"] as const;
export type StepId = (typeof STEP_IDS)[number];

/** Label of a pipeline step for the app type. */
export function stepLabel(type: string, id: string): string {
  if (type !== "git") {
    if (id === "fetch") return t("deploy.step.release");
    if (id === "build") return t("deploy.step.pull");
    if (id === "restart") return t("deploy.step.up");
  }
  return t(`deploy.step.${id}` as Key);
}

export function emptySteps(): Step[] {
  return STEP_IDS.map((id) => ({ id, state: "pending", startedAt: 0, finishedAt: 0, note: "" }));
}

/** "850 ms", "12 s", "3 m 05 s". */
export function fmtMs(ms: number): string {
  if (!ms || ms < 0) return "";
  if (ms < 1000) return `${ms} ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} m ${String(s % 60).padStart(2, "0")} s`;
  return `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, "0")} m`;
}

export function fmtTime(ms: number): string {
  if (!ms) return "";
  const d = new Date(ms);
  const pad = (x: number) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** "5 phút trước" style relative time. */
export function fmtAgo(ms: number): string {
  if (!ms) return "";
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return t("deploy.ago.now");
  if (s < 3600) return t("deploy.ago.min", { n: Math.floor(s / 60) });
  if (s < 86400) return t("deploy.ago.hour", { n: Math.floor(s / 3600) });
  return t("deploy.ago.day", { n: Math.floor(s / 86400) });
}

export function runDuration(r: Run): number {
  return r.finished ? r.finished - r.started : 0;
}

export function statusTone(status: string): "ok" | "err" | "warn" | "muted" {
  switch (status) {
    case "ok":
      return "ok";
    case "failed":
      return "err";
    case "running":
      return "warn";
    default:
      return "muted";
  }
}

export function statusLabel(status: string): string {
  switch (status) {
    case "ok":
    case "failed":
    case "running":
    case "cancelled":
    case "interrupted":
      return t(`deploy.status.${status}` as Key);
  }
  return status;
}

export function triggerLabel(trigger: string): string {
  switch (trigger) {
    case "manual":
    case "redeploy":
    case "rollback":
    case "auto-rollback":
      return t(`deploy.trigger.${trigger}` as Key);
  }
  return trigger;
}

export function typeLabel(type: string): string {
  return t(`deploy.type.${type === "git" || type === "compose" || type === "image" ? type : "git"}` as Key);
}

export function stageLabel(stage: string): string {
  return t(`deploy.stage.${stage === "staging" || stage === "development" ? stage : "production"}` as Key);
}

/** Mirrors the backend's choice of "previous version" for a rollback. */
export function previousTarget(runs: Run[], type: string): Run | null {
  const done = runs.filter((r) => r.status !== "running");
  const current = done.find((r) => r.status === "ok");
  if (!current || !done.length) return null;
  if (done[0].status !== "ok" && done[0].data.target !== current.data.target) return current;
  return done.find((r) => r.status === "ok" && r.started < current.started && r.data.target !== current.data.target && r.data.appType === type) ?? null;
}

// ---- validation (mirrors services/deploy/model.go) ----

const reEnv = /^[A-Za-z_][A-Za-z0-9_]*$/;
const rePath = /^[A-Za-z0-9._@+,=/-]+$/;
const reTag = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/;
const reImage =
  /^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]+)?\/)?[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$/;
const rePort = /^(?:(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]):)?(?:\d{1,5}(?:-\d{1,5})?:)?\d{1,5}(?:-\d{1,5})?(?:\/(?:tcp|udp|sctp))?$/;
const reSCP = /^[A-Za-z0-9._][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/@+][A-Za-z0-9._~/@+-]*$/;
const reProject = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const reContainer = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/;
const FORBIDDEN = new Set(["/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var", "/var/lib", "/var/www", "/usr/local", "/usr/bin", "/var/log"]);

export const validEnvName = (s: string) => s.length <= 128 && reEnv.test(s);
export const validTag = (s: string) => reTag.test(s);
export const validImage = (s: string) => s.length <= 255 && reImage.test(s);
export const validPort = (s: string) => rePort.test(s);
export const validProject = (s: string) => reProject.test(s);
export const validContainer = (s: string) => reContainer.test(s);

export function validDir(p: string): boolean {
  const d = p.replace(/\/+$/, "");
  if (!d.startsWith("/") || d.length > 1024 || !rePath.test(d)) return false;
  if (d.slice(1).split("/").some((s) => s === "" || s === "." || s === "..")) return false;
  return !FORBIDDEN.has(d);
}

export function validRelOrAbs(p: string): boolean {
  if (!p || p.length > 1024 || !rePath.test(p) || p.endsWith("/")) return false;
  return !p.replace(/^\//, "").split("/").some((s) => s === "" || s === "..");
}

/** git check-ref-format rules. */
export function validRef(r: string): boolean {
  if (!r || r.length > 200 || r === "@" || r.startsWith("-") || r.startsWith("/") || r.endsWith("/") || r.endsWith(".") || r.endsWith(".lock")) return false;
  if (r.includes("..") || r.includes("//") || r.includes("@{")) return false;
  for (const ch of r) {
    const c = ch.charCodeAt(0);
    if (c < 0x20 || c >= 0x7f || " ~^:?*[\\".includes(ch)) return false;
  }
  return !r.split("/").some((s) => s.startsWith(".") || s.endsWith(".lock"));
}

export function validRepo(s: string): boolean {
  if (!s || s.length > 2048 || s.startsWith("-")) return false;
  for (const ch of s) {
    const c = ch.charCodeAt(0);
    if (c <= 0x20 || c >= 0x7f || "'\"\\`".includes(ch)) return false;
  }
  if (s.startsWith("/")) return rePath.test(s) && !s.split("/").includes("..");
  if (reSCP.test(s)) return true;
  try {
    const u = new URL(s);
    if (u.protocol === "file:") return !u.host && rePath.test(u.pathname);
    if (!["https:", "http:", "ssh:", "git:"].includes(u.protocol) || !u.host) return false;
    return !u.password;
  } catch {
    return false;
  }
}

export function validUrl(s: string): boolean {
  try {
    const u = new URL(s);
    return (u.protocol === "http:" || u.protocol === "https:") && !!u.host && !/\s/.test(s);
  } catch {
    return false;
  }
}

export function validVolume(v: string): boolean {
  const parts = v.split(":");
  if (parts.length < 2 || parts.length > 3) return false;
  const [src, dst, opts] = parts;
  if (src.startsWith("/")) {
    if (!rePath.test(src) || src.split("/").includes("..")) return false;
  } else if (src.startsWith("./")) {
    if (src !== "./" && !validRelOrAbs(src.slice(2))) return false;
  } else if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(src)) return false;
  if (!dst.startsWith("/") || !rePath.test(dst) || dst.split("/").includes("..")) return false;
  if (opts !== undefined) return opts.split(",").every((o) => ["ro", "rw", "z", "Z", "cached", "delegated", "consistent", "nocopy"].includes(o));
  return true;
}

/** Whether a value can be written to the env file (see envfile.go). */
export function validEnvValue(v: string): boolean {
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x08\x0a-\x1f\x7f]/.test(v)) return false;
  if (!v.includes("'")) return true;
  return !/["\\$`]/.test(v);
}

const ENV_DENY_PREFIX = ["/etc/ssh/", "/etc/pam.d/", "/etc/sudoers", "/etc/security/", "/root/.ssh/", "/boot/", "/proc/", "/sys/", "/dev/", "/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/lib/", "/lib64/", "/usr/lib/"];
const ENV_DENY = new Set(["/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/hosts", "/etc/fstab", "/etc/crontab", "/etc/resolv.conf", "/etc/hostname", "/etc/environment", "/etc/profile"]);

/** The env file must not replace a system file (mirrors safeEnvPath). */
export function safeEnvPath(p: string): boolean {
  const dir = p.slice(0, p.lastIndexOf("/")) || "/";
  if (ENV_DENY.has(p) || p.includes("/.ssh/") || dir === "/" || (dir === "/etc" && !p.endsWith(".env"))) return false;
  return !ENV_DENY_PREFIX.some((pre) => p.startsWith(pre));
}

/** The env file path the backend will use. */
export function envPath(a: App): string {
  const dir = a.dir.replace(/\/+$/, "");
  if (!a.envFile) return `${dir}/.env`;
  if (a.envFile.startsWith("/")) return a.envFile;
  return `${dir}/${a.envFile.replace(/^\.\//, "")}`;
}
