import { AlertTriangle, ShieldAlert } from "lucide-react";
import type { Danger, Preview } from "../../../bindings/server-manager/services/command";
import { confirmMany } from "../../ui/confirm";
import { useUI } from "../../store/ui";
import { hasKey, t, useT } from "../../i18n";

// CSI / OSC / two-byte escape sequences.
// eslint-disable-next-line no-control-regex
const ANSI = /\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]|\r(?!\n)/g;

/** Removes terminal escape sequences (colors, cursor moves) from output. */
export function stripAnsi(s: string): string {
  return s.replace(ANSI, "");
}

export function firstLine(s: string): string {
  const clean = stripAnsi(s);
  for (const line of clean.split("\n")) {
    if (line.trim()) return line.trim();
  }
  return "";
}

/** Milliseconds as "850 ms", "12.3 s", "2 m 05 s". */
export function formatMs(ms: number): string {
  if (!ms || ms < 0) return "–";
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  return `${m} m ${String(Math.floor(s % 60)).padStart(2, "0")} s`;
}

export function formatTime(ms: number): string {
  if (!ms) return "";
  const d = new Date(ms);
  const pad = (x: number) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** Translated label of a danger reason. */
export function dangerLabel(code: string): string {
  const key = `cmd.danger.${code}`;
  return hasKey(key) ? t(key) : code;
}

/** Translated label of an error code (without params). */
export function codeLabel(code: string): string {
  const key = `err.${code}`;
  return hasKey(key) ? t(key) : code;
}

export function DangerList({ dangers }: { dangers: Danger[] }) {
  const tr = useT();
  if (dangers.length === 0) return null;
  const severe = dangers.some((d) => d.level === "danger");
  return (
    <div className={`cmd-danger ${severe ? "" : "warn"}`}>
      <div className="cmd-danger-head">
        {severe ? <ShieldAlert size={15} /> : <AlertTriangle size={15} />}
        <span>{severe ? tr("cmd.dangerTitle") : tr("cmd.warnTitle")}</span>
      </div>
      <ul>
        {dangers.map((d) => (
          <li key={d.code}>
            <span>{dangerLabel(d.code)}</span>
            {d.match && <code>{d.match}</code>}
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * Asks before running. Dangerous commands always need the number of servers
 * typed; otherwise confirmMany applies the production rule.
 */
export async function confirmRun(pv: Preview, title: string, confirmText: string): Promise<boolean> {
  const servers = pv.servers ?? [];
  const names = servers.map((s) => s.name);
  const hasProduction = servers.some((s) => s.environment === "production");
  if (!pv.danger) return confirmMany(title, names, confirmText, hasProduction);
  const list = names.length > 12 ? names.slice(0, 12).join(", ") + ` … (+${names.length - 12})` : names.join(", ");
  const word = String(names.length);
  const dangers = pv.dangers ?? [];
  const r = await useUI.getState().openDialog({
    title,
    wide: true,
    danger: true,
    confirmText,
    message: (
      <>
        {dangers.length > 0 ? <DangerList dangers={dangers} /> : <div className="cmd-danger"><div className="cmd-danger-head"><ShieldAlert size={15} /><span>{t("cmd.dangerPreset")}</span></div></div>}
        <div className="cmd-confirm-list">{list}</div>
        {hasProduction && <div className="prod-warn">{t("cmd.prodIncluded")}</div>}
        <div className="prod-warn">{t("cmd.typeCountDanger", { n: names.length })}</div>
      </>
    ),
    fields: [{ name: "n", label: t("app.typeCount"), autoFocus: true, validate: (v) => (v.trim() === word ? null : t("app.countMismatch")) }],
  });
  return r?.action === "ok";
}
