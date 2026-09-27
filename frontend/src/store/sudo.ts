import { errCode, errMsg, isPermissionError } from "../lib/api";
import { t } from "../i18n";
import { useApp } from "./app";
import { confirmDialog, promptDialog } from "./ui";

function needsPassword(e: unknown) {
  const c = errCode(e);
  return c === "sudo.required" || c === "sudo.wrongPassword";
}

/**
 * Runs fn with a sudo password. It first tries the password cached for this
 * session (or none, letting the backend use NOPASSWD sudo / the saved login
 * password), and prompts only if sudo actually asks for one.
 */
export async function withSudo<T>(connId: string, fn: (password: string) => Promise<T>): Promise<T> {
  const cached = useApp.getState().sudo[connId] ?? "";
  try {
    return await fn(cached);
  } catch (e) {
    if (!needsPassword(e)) throw e;
    let lastErr = e;
    for (let i = 0; i < 3; i++) {
      const pw = await promptDialog(t("sudo.title"), t("sudo.label"), "", {
        type: "password",
        message: errCode(lastErr) === "sudo.wrongPassword" && i > 0 ? t("sudo.wrong") : t("sudo.needRoot"),
        confirmText: t("common.confirm"),
      });
      if (pw === null) throw new Error(t("sudo.cancelled"));
      try {
        const r = await fn(pw);
        useApp.setState((s) => ({ sudo: { ...s.sudo, [connId]: pw } }));
        return r;
      } catch (e2) {
        if (!needsPassword(e2)) throw e2;
        lastErr = e2;
      }
    }
    throw lastErr;
  }
}

/**
 * Runs op; if it fails for lack of permissions, offers to retry it through
 * sudo via sudoOp. Returns false when the user declined.
 */
export async function withSudoFallback(connId: string, op: () => Promise<unknown>, sudoOp: (password: string) => Promise<unknown>): Promise<boolean> {
  try {
    await op();
    return true;
  } catch (e) {
    if (!isPermissionError(e)) throw e;
    const ok = await confirmDialog(t("sudo.retryTitle"), `${errMsg(e)}\n\n${t("sudo.retryMsg")}`, t("common.useSudo"));
    if (!ok) return false;
    await withSudo(connId, sudoOp);
    return true;
  }
}
