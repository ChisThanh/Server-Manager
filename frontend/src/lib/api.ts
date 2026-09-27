export {
  FileService,
  ServerService,
  SystemService,
  TerminalService,
} from "../../bindings/server-manager/services";
export type {
  ConnectResult,
  DiskInfo,
  FileContent,
  FileEntry,
  HostKeyInfo,
  Process,
  SaveResult,
  SearchHit,
  ServiceUnit,
  SysInfo,
  TransferEvent,
} from "../../bindings/server-manager/services";
export type { ExecResult } from "../../bindings/server-manager/internal/sshx";
export { AuthType } from "../../bindings/server-manager/internal/store";
export type { Server } from "../../bindings/server-manager/internal/store";

export type { Error as AppError } from "../../bindings/server-manager/internal/apperr";
import type { Error as AppError } from "../../bindings/server-manager/internal/apperr";
import { hasKey, t } from "../i18n";

/** Renders a coded backend error in the current language. */
export function formatAppError(ae: AppError | null | undefined): string {
  if (!ae) return "";
  const key = "err." + ae.code;
  const params = Object.fromEntries(Object.entries(ae.params ?? {}).map(([k, v]) => [k, v ?? ""]));
  const base = hasKey(key) ? t(key, params) : "";
  if (base && ae.detail) return `${base}: ${ae.detail}`;
  return base || ae.detail || t("common.unknownError");
}

function appError(e: unknown): AppError | null {
  const cause = (e as { cause?: unknown } | null)?.cause;
  if (cause && typeof cause === "object" && typeof (cause as AppError).code === "string") return cause as AppError;
  return null;
}

/** Extracts a readable, translated message from a rejected binding call. */
export function errMsg(e: unknown): string {
  if (!e) return t("common.unknownError");
  const ae = appError(e);
  if (ae) return formatAppError(ae);
  if (typeof e === "string") return e;
  if (e instanceof Error) return e.message;
  const m = (e as { message?: unknown }).message;
  return typeof m === "string" ? m : String(e);
}

/** The backend error code of a rejected call, if any. */
export function errCode(e: unknown): string | undefined {
  return appError(e)?.code;
}

export function isPermissionError(e: unknown): boolean {
  if (errCode(e) === "fs.permission") return true;
  return /permission denied|operation not permitted/i.test(errMsg(e));
}
