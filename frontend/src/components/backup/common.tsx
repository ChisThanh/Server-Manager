import type { ReactNode } from "react";
import { Box, Container, Database, FileArchive, Folder, HardDrive, Server, Terminal, Cloud, Zap } from "lucide-react";
import { BackupService } from "../../../bindings/server-manager/services/backup";
import type { Destination, Job, JobStatus, Schedule } from "../../../bindings/server-manager/services/backup/models";
import { t, type Key } from "../../i18n";
import { formatDate, formatDuration } from "../../lib/format";
import type { Tone } from "../../ui/Status";

export { BackupService };
export type {
  BackupItem,
  CronPreview,
  Destination,
  Job,
  JobSecrets,
  JobStatus,
  Manifest,
  RestoreOptions,
  Run,
  Schedule,
} from "../../../bindings/server-manager/services/backup/models";

export type JobType = "files" | "postgres" | "mysql" | "volume" | "redis" | "custom";
export const JOB_TYPES: JobType[] = ["files", "postgres", "mysql", "volume", "redis", "custom"];
export type DestType = "local" | "server" | "s3";

export function TypeIcon({ type, size = 16 }: { type: string; size?: number }) {
  switch (type) {
    case "files":
      return <Folder size={size} />;
    case "postgres":
    case "mysql":
      return <Database size={size} />;
    case "volume":
      return <Container size={size} />;
    case "redis":
      return <Zap size={size} />;
    case "custom":
      return <Terminal size={size} />;
  }
  return <FileArchive size={size} />;
}

export function DestIcon({ type, size = 14 }: { type: string; size?: number }) {
  switch (type) {
    case "local":
      return <HardDrive size={size} />;
    case "server":
      return <Server size={size} />;
    case "s3":
      return <Cloud size={size} />;
  }
  return <Box size={size} />;
}

export const typeLabel = (type: string) => t(`backup.type.${type}` as Key);
export const destTypeLabel = (type: string) => t(`backup.destType.${type}` as Key);

/** One-line description of what a job backs up. */
export function jobTarget(j: Job): string {
  switch (j.type) {
    case "files":
      return (j.paths ?? []).join(", ");
    case "postgres":
    case "mysql":
      return j.database || t("backup.allDatabases");
    case "volume":
      return j.volume;
    case "redis":
      return j.dbHost ? `${j.dbHost}:${j.dbPort || 6379}` : "redis";
    case "custom":
      return j.command;
  }
  return "";
}

const WEEKDAYS: Key[] = ["backup.wd.0", "backup.wd.1", "backup.wd.2", "backup.wd.3", "backup.wd.4", "backup.wd.5", "backup.wd.6"];
export const weekdayLabel = (d: number) => t(WEEKDAYS[d] ?? "backup.wd.0");

export function scheduleText(s: Schedule | undefined): string {
  if (!s || !s.cron) return t("backup.sched.manualOnly");
  switch (s.mode) {
    case "hourly":
      return t("backup.sched.hourlyAt", {
        m: String(s.minute).padStart(2, "0"),
      });
    case "daily":
      return t("backup.sched.dailyAt", { time: s.time });
    case "weekly":
      return t("backup.sched.weeklyAt", {
        day: weekdayLabel(s.weekday),
        time: s.time,
      });
  }
  return `cron: ${s.cron}`;
}

export function fmtTime(ms: number): string {
  return ms ? formatDate(Math.floor(ms / 1000)) : "–";
}

/** "3 h ago" style age. */
export function ago(ms: number): string {
  if (!ms) return "–";
  const sec = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (sec < 60) return t("backup.justNow");
  return t("backup.ago", { d: formatDuration(sec) });
}

/** "in 3 h" style. */
export function inFuture(ms: number): string {
  if (!ms) return "–";
  const sec = Math.max(0, Math.floor((ms - Date.now()) / 1000));
  if (sec < 60) return t("backup.soon");
  return t("backup.in", { d: formatDuration(sec) });
}

export function durationText(started: number, finished: number): string {
  if (!started || !finished) return "–";
  const s = Math.round((finished - started) / 1000);
  if (s < 60) return `${s}s`;
  return formatDuration(s);
}

/** Expected interval between runs (ms) for health coloring. */
function expectedInterval(st: JobStatus): number {
  const c = st.cron || "";
  if (!c) return 0;
  const f = c.trim().split(/\s+/);
  if (c.startsWith("@hourly") || (f.length === 5 && f[1] === "*")) return 3600e3;
  if (f.length === 5 && f[4] !== "*" && f[2] === "*") return 7 * 86400e3;
  if (c.startsWith("@weekly")) return 7 * 86400e3;
  if (c.startsWith("@monthly") || (f.length === 5 && f[2] !== "*")) return 31 * 86400e3;
  return 86400e3;
}

/** Tone of the "last successful backup" age. */
export function freshnessTone(st: JobStatus): Tone {
  if (st.lastStatus === "failed") return "err";
  if (!st.lastSuccess) return st.cron ? "warn" : "muted";
  const iv = expectedInterval(st);
  if (!iv) return "ok";
  const age = Date.now() - st.lastSuccess;
  if (age <= iv * 1.5 + 3600e3) return "ok";
  if (age <= iv * 3) return "warn";
  return "err";
}

export function destLocation(d: Destination): string {
  if (d.type === "s3") return `s3://${d.bucket}${d.prefix ? "/" + d.prefix : ""} @ ${d.endpoint}`;
  return d.path;
}

export function Field({
  label,
  hint,
  error,
  children,
  wide,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: string | null;
  children: ReactNode;
  wide?: boolean;
}) {
  return (
    <div className={`field ${wide ? "backup-wide" : ""}`}>
      <label>{label}</label>
      {children}
      {error ? <div className="error">{error}</div> : hint ? <div className="hint">{hint}</div> : null}
    </div>
  );
}

/** Random passphrase (base32-ish, ~130 bits). */
export function generatePassphrase(): string {
  const alphabet = "abcdefghijkmnpqrstuvwxyz23456789";
  const bytes = new Uint8Array(26);
  crypto.getRandomValues(bytes);
  let out = "";
  bytes.forEach((b, i) => {
    out += alphabet[b % alphabet.length];
    if (i % 5 === 4 && i < bytes.length - 1) out += "-";
  });
  return out;
}

export const RE_DB = /^[A-Za-z0-9_][A-Za-z0-9_$.-]{0,62}$/;
export const RE_USER = /^[A-Za-z0-9_][A-Za-z0-9_.@-]{0,62}$/;
export const RE_VOLUME = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$/;
export const RE_HOST = /^[A-Za-z0-9.:-]{1,253}$/;
export const isAbsPath = (p: string) => p.startsWith("/") && !/[\n\r\0]/.test(p);
