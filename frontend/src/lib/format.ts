import { t } from "../i18n";

export function formatBytes(n: number, digits = 1): string {
  if (!Number.isFinite(n) || n <= 0) return "0 B";
  if (n < 1) return `${n.toFixed(1)} B`;
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / Math.pow(1024, i);
  return `${v.toFixed(i === 0 ? 0 : digits)} ${units[i]}`;
}

export function formatDate(unix: number): string {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const pad = (x: number) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function formatDuration(sec: number): string {
  if (!sec) return "–";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return t("dur.days", { d, h });
  if (h > 0) return t("dur.hours", { h, m });
  return t("dur.minutes", { m });
}

export function octal(perm: number): string {
  return (perm & 0o7777).toString(8).padStart(3, "0");
}

export function joinPath(dir: string, name: string): string {
  if (!dir || dir === "/") return "/" + name.replace(/^\/+/, "");
  return dir.replace(/\/+$/, "") + "/" + name.replace(/^\/+/, "");
}

export function dirname(p: string): string {
  const clean = p.replace(/\/+$/, "");
  const i = clean.lastIndexOf("/");
  if (i <= 0) return "/";
  return clean.slice(0, i);
}

export function basename(p: string): string {
  const clean = p.replace(/\/+$/, "");
  return clean.slice(clean.lastIndexOf("/") + 1) || "/";
}

/** Validates a single file/folder name typed by the user. */
export function validName(name: string): string | null {
  const n = name.trim();
  if (!n) return t("fx.nameEmpty");
  if (n === "." || n === ".." || n.includes("/") || n.includes("\0")) return t("fx.nameInvalid");
  return null;
}

export function isArchive(name: string): boolean {
  return /\.(zip|tar|tar\.gz|tgz|tar\.bz2|tbz2|tar\.xz|txz|gz)$/i.test(name);
}
