import { createElement } from "react";
import { FileService, SystemService, errMsg } from "../lib/api";
import { t } from "../i18n";
import { getModel, getTab, reloadFile, saveFile } from "./editor";
import { withSudo } from "./sudo";
import { toast, useUI } from "./ui";

export interface ConfigKind {
  kind: "nginx" | "caddy" | "apache" | "sshd" | "systemd";
  label: string;
  unit?: string;
}

/** Which service a config file belongs to, so the editor can offer "Save & reload". */
export function configKind(path: string): ConfigKind | null {
  if (/^\/etc\/nginx\//.test(path)) return { kind: "nginx", label: "nginx" };
  if (/^\/etc\/caddy\//.test(path)) return { kind: "caddy", label: "Caddy" };
  if (/^\/etc\/(apache2|httpd)\//.test(path)) return { kind: "apache", label: "Apache" };
  if (/^\/etc\/ssh\/sshd_config(\.d\/.*)?$/.test(path)) return { kind: "sshd", label: "sshd" };
  const m = path.match(/^\/(etc|lib|usr\/lib)\/systemd\/system\/([^/]+\.(service|timer|socket|mount|path))(\.d\/.*)?$/);
  if (m) return { kind: "systemd", label: "systemd", unit: m[2] };
  return null;
}

/**
 * Saves the file, validates the service's configuration and reloads it only
 * when valid. On failure the user can restore the content from before the
 * save, so a typo never leaves a broken config on disk.
 */
export async function saveAndApply(connId: string, path: string, ck: ConfigKind) {
  const tab = getTab(connId, path);
  if (!tab || !getModel(connId, path)) return;
  let previous: string | null = null;
  try {
    previous = (tab.sudo ? await withSudo(connId, (pw) => FileService.ReadFileSudo(connId, path, pw)) : await FileService.ReadFile(connId, path)).content;
  } catch {
    /* new or unreadable: no revert */
  }
  if (!(await saveFile(connId, path))) return;
  let r;
  try {
    r = await withSudo(connId, (pw) => SystemService.ApplyConfig(connId, ck.kind, ck.unit ?? "", pw));
  } catch (e) {
    toast(errMsg(e), "error");
    return;
  }
  if (r.ok) {
    toast(t("app.ed.applyOk", { name: ck.label }), "success");
    return;
  }
  const res = await useUI.getState().openDialog({
    title: t("app.ed.applyFailTitle", { name: ck.label }),
    message: createElement("div", null, createElement("p", null, t("app.ed.applyFailMsg")), createElement("pre", { className: "log-view err", style: { maxHeight: 260 } }, r.output)),
    danger: true,
    wide: true,
    confirmText: t("app.ed.keep"),
    extra: previous !== null ? [{ id: "revert", label: t("app.ed.revert"), danger: true }] : [],
  });
  if (res?.action === "revert" && previous !== null) {
    try {
      const cur = getTab(connId, path);
      if (cur?.sudo) await withSudo(connId, (pw) => FileService.WriteFileSudo(connId, path, previous!, pw, 0, true));
      else await FileService.WriteFile(connId, path, previous, 0, true);
      await reloadFile(connId, path);
      toast(t("app.ed.reverted"), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  }
}
