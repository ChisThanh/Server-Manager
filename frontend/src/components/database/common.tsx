import type { ReactNode } from "react";
import { Database, Layers, Zap } from "lucide-react";
import { DatabaseService } from "../../../bindings/server-manager/services/database";
import type { Target } from "../../../bindings/server-manager/services/database/models";
import { withSudo } from "../../store/sudo";
import { t } from "../../i18n";

export { DatabaseService };
export type * from "../../../bindings/server-manager/services/database/models";

export type Engine = "postgres" | "mysql" | "redis";

/** Props shared by the per-target views. */
export interface TargetViewProps {
  connId: string;
  target: Target;
  /** The view is on screen and the connection is up (poll only then). */
  active: boolean;
  canEdit: boolean;
  canShell: boolean;
  navigate: (panel: string, arg?: unknown) => void;
}

export const POLL_MS = 10000;

/** Calls a DatabaseService method with the sudo password (prompting if needed). */
export function dbs<T>(
  connId: string,
  fn: (pw: string) => Promise<T>,
): Promise<T> {
  return withSudo(connId, fn);
}

export function engineName(t: Pick<Target, "engine" | "name">): string {
  if (t.engine === "postgres") return "PostgreSQL";
  if (t.engine === "mysql") return t.name === "MariaDB" ? "MariaDB" : "MySQL";
  return "Redis";
}

export function EngineIcon({
  engine,
  size = 15,
}: {
  engine: string;
  size?: number;
}): ReactNode {
  if (engine === "postgres")
    return <Database size={size} className="db-ic pg" />;
  if (engine === "mysql") return <Layers size={size} className="db-ic my" />;
  return <Zap size={size} className="db-ic rd" />;
}

export function pct(v: number, digits = 1): string {
  if (v < 0 || !Number.isFinite(v)) return "–";
  return `${(v * 100).toFixed(digits)}%`;
}

export function num(v: number | undefined | null): string {
  if (v === undefined || v === null || !Number.isFinite(v)) return "–";
  return v.toLocaleString();
}

/** Short duration for query ages: 850 ms, 12.3 s, 4m 05s, 2h 10m, 3d 4h. */
export function shortDur(sec: number): string {
  if (sec < 0 || !Number.isFinite(sec)) return "–";
  if (sec < 1) return `${Math.round(sec * 1000)} ms`;
  if (sec < 60) return `${sec.toFixed(1)} s`;
  const m = Math.floor(sec / 60);
  if (m < 60) return `${m}m ${String(Math.floor(sec % 60)).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${String(m % 60).padStart(2, "0")}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

export function ms(v: number): string {
  if (!Number.isFinite(v)) return "–";
  if (v < 1) return `${v.toFixed(3)} ms`;
  if (v < 1000) return `${v.toFixed(1)} ms`;
  return shortDur(v / 1000);
}

/** Only the lower-case letters, digits and "_" we accept for new names. */
export const IDENT_RE = /^[A-Za-z_][A-Za-z0-9_]{0,62}$/;

export function identError(v: string): string | null {
  return IDENT_RE.test(v.trim()) ? null : t("db.nameRule");
}

/** Consoles the terminal can open for a target (null: not for this target). */
export function consoleExec(
  target: Target,
  database: string,
): {
  kind: "psql" | "mysql" | "redis" | "docker";
  target: string;
  title: string;
} | null {
  if (target.mode === "docker")
    return {
      kind: "docker",
      target: target.container,
      title: target.container,
    };
  // The terminal consoles use local admin access (sudo -u postgres psql, sudo mysql, redis-cli).
  if (target.auth !== "peer" || target.host || target.port) return null;
  const kind =
    target.engine === "postgres"
      ? "psql"
      : target.engine === "mysql"
        ? "mysql"
        : "redis";
  const db = target.engine === "redis" ? "" : database;
  return { kind, target: db, title: db ? `${kind} · ${db}` : kind };
}
