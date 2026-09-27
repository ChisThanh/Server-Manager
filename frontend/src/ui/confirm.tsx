import type { ReactNode } from "react";
import { useApp } from "../store/app";
import { useSettings } from "../store/settings";
import { confirmDialog, useUI } from "../store/ui";
import { t } from "../i18n";

/**
 * Asks before a dangerous action. On production servers (when the setting is
 * on) the user must type the server's name, like GitHub's delete dialog.
 */
export async function confirmDanger(opts: { serverId?: string; title: string; message?: ReactNode; confirmText: string }): Promise<boolean> {
  const server = opts.serverId ? useApp.getState().servers.find((s) => s.id === opts.serverId) : undefined;
  const strict = server?.environment === "production" && useSettings.getState().settings.confirmProduction;
  if (!strict || !server) return confirmDialog(opts.title, opts.message, opts.confirmText, true);
  const r = await useUI.getState().openDialog({
    title: opts.title,
    message: (
      <>
        {opts.message}
        {opts.message ? <br /> : null}
        <div className="prod-warn">{t("app.prodConfirm", { name: server.name })}</div>
      </>
    ),
    confirmText: opts.confirmText,
    danger: true,
    fields: [
      {
        name: "name",
        label: t("app.typeName"),
        autoFocus: true,
        placeholder: server.name,
        validate: (v) => (v.trim() === server.name ? null : t("app.nameMismatch")),
      },
    ],
  });
  return r?.action === "ok";
}

/** Confirms an action on many servers (command center, bulk ops). */
export async function confirmMany(title: string, names: string[], confirmText: string, hasProduction: boolean): Promise<boolean> {
  const list = names.length > 12 ? names.slice(0, 12).join(", ") + ` … (+${names.length - 12})` : names.join(", ");
  if (!hasProduction || !useSettings.getState().settings.confirmProduction) {
    return confirmDialog(title, list, confirmText, true);
  }
  const word = String(names.length);
  const r = await useUI.getState().openDialog({
    title,
    message: (
      <>
        {list}
        <div className="prod-warn">{t("app.prodConfirmMany", { n: names.length })}</div>
      </>
    ),
    confirmText,
    danger: true,
    fields: [{ name: "n", label: t("app.typeCount"), autoFocus: true, validate: (v) => (v.trim() === word ? null : t("app.countMismatch")) }],
  });
  return r?.action === "ok";
}
