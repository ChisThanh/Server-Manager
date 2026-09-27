import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { X } from "lucide-react";
import { DockerService } from "../../../bindings/server-manager/services/docker";
import { useApp } from "../../store/app";
import { useSettings } from "../../store/settings";
import { withSudo } from "../../store/sudo";
import { useUI, type DialogField } from "../../store/ui";
import { hasKey, t, useT, type Key } from "../../i18n";
import { StateBadge, type Tone } from "../../ui/Status";

export { DockerService };
export type * from "../../../bindings/server-manager/services/docker/models";

/** Props shared by the sub-views of the Docker panel. */
export interface ViewProps {
  connId: string;
  /** The sub-view is on screen and the connection is up. */
  active: boolean;
  /** Poll every 5 s. */
  auto: boolean;
  canEdit: boolean;
  canShell: boolean;
  navigate: (panel: string, arg?: unknown) => void;
}

/** Calls a DockerService method with the sudo password (prompting if needed). */
export function ds<T>(connId: string, fn: (pw: string) => Promise<T>): Promise<T> {
  return withSudo(connId, fn);
}

export const POLL_MS = 5000;

/** Asks before an action, with optional checkboxes. On production servers
 *  (when the setting is on) the user must also type the server name, like
 *  confirmDanger. Returns the checkbox values, or null when cancelled. */
export async function confirmOpts(opts: {
  serverId: string;
  title: string;
  message?: ReactNode;
  confirmText: string;
  danger?: boolean;
  options?: { name: string; label: string; value?: boolean }[];
}): Promise<Record<string, boolean> | null> {
  const server = useApp.getState().servers.find((s) => s.id === opts.serverId);
  const danger = opts.danger ?? true;
  const strict = danger && server?.environment === "production" && useSettings.getState().settings.confirmProduction;
  const fields: DialogField[] = (opts.options ?? []).map((o) => ({ name: o.name, label: o.label, type: "checkbox", value: !!o.value }));
  if (strict && server) {
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
        {strict && server ? <div className="prod-warn">{t("app.prodConfirm", { name: server.name })}</div> : null}
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

/** Translated container state. */
export function stateLabel(state: string): string {
  const k = `docker.state.${state}`;
  return hasKey(k) ? t(k as Key) : state || "—";
}

export function StateCell({ state, health }: { state: string; health?: string }) {
  const tone: Tone | undefined = state === "running" && health === "unhealthy" ? "err" : state === "running" && health === "starting" ? "warn" : undefined;
  return (
    <span className="docker-state">
      <StateBadge state={state} tone={tone}>
        {stateLabel(state)}
      </StateBadge>
      {health && <HealthBadge health={health} />}
    </span>
  );
}

export function HealthBadge({ health }: { health: string }) {
  const tr = useT();
  const k = `docker.health.${health}`;
  const tone: Tone = health === "healthy" ? "ok" : health === "unhealthy" ? "err" : "warn";
  return <span className={`docker-health ${tone}`}>{hasKey(k) ? tr(k as Key) : health}</span>;
}

export function shortId(id: string): string {
  return id.replace(/^sha256:/, "").slice(0, 12);
}

/** "3 phút trước" from a unix time (seconds). */
export function ago(unix: number): string {
  if (!unix) return "—";
  const s = Math.max(0, Date.now() / 1000 - unix);
  if (s < 60) return t("docker.ago.sec");
  if (s < 3600) return t("docker.ago.min", { n: Math.floor(s / 60) });
  if (s < 86400) return t("docker.ago.hour", { n: Math.floor(s / 3600) });
  if (s < 86400 * 60) return t("docker.ago.day", { n: Math.floor(s / 86400) });
  return t("docker.ago.month", { n: Math.floor(s / (86400 * 30)) });
}

export function fullDate(unix: number): string {
  return unix ? new Date(unix * 1000).toLocaleString() : "";
}

/** Sortable table state. */
export function useSort<K extends string>(initial: K, desc = false) {
  const [sort, setSort] = useState<{ key: K; desc: boolean }>({ key: initial, desc });
  const th = (key: K, label: ReactNode, opts: { num?: boolean; defDesc?: boolean; style?: React.CSSProperties } = {}) => (
    <th
      className={`sortable ${opts.num ? "num" : ""}`}
      style={opts.style}
      onClick={() => setSort((s) => ({ key, desc: s.key === key ? !s.desc : !!opts.defDesc }))}
    >
      {label}
      {sort.key === key ? (sort.desc ? " ↓" : " ↑") : ""}
    </th>
  );
  return { sort, th };
}

export function cmp(a: unknown, b: unknown): number {
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a ?? "").localeCompare(String(b ?? ""), undefined, { numeric: true, sensitivity: "base" });
}

/** Large modal (editor, inspect) using the app's overlay styles. */
export function BigModal(props: { title: ReactNode; children: ReactNode; footer?: ReactNode; onClose: () => void; className?: string }) {
  const self = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      // Only the top-most overlay (a confirm dialog, a nested modal…) reacts.
      const shown = Array.from(document.querySelectorAll(".overlay")).filter((el) => el.getClientRects().length > 0);
      if (shown[shown.length - 1] !== self.current) return;
      e.stopPropagation();
      props.onClose();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [props.onClose]);
  return (
    <div className="overlay" ref={self}>
      <div className={`modal docker-big ${props.className ?? ""}`} role="dialog">
        <div className="modal-head">
          {props.title}
          <div className="grow" />
          <button className="icon-btn" onClick={props.onClose} title={t("common.close")}>
            <X size={15} />
          </button>
        </div>
        <div className="modal-body docker-big-body">{props.children}</div>
        {props.footer && <div className="modal-foot">{props.footer}</div>}
      </div>
    </div>
  );
}

/** Filters rows by a free-text needle over the given fields. */
export function useFilter<T>(rows: T[] | undefined, q: string, fields: (r: T) => (string | undefined | null)[]) {
  return useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = rows ?? [];
    if (!needle) return list;
    return list.filter((r) => fields(r).some((f) => (f ?? "").toLowerCase().includes(needle)));
  }, [rows, q]);
}

/** Validation for the name of a new Compose project. */
export const PROJECT_RE = /^[a-z0-9][a-z0-9_-]{0,62}$/;
export const PATH_RE = /^\/[A-Za-z0-9._@+=:~ /-]*$/;
export const IMAGE_RE = /^[A-Za-z0-9][A-Za-z0-9._\/:@-]*$/;
