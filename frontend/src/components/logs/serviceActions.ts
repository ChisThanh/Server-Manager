import { confirmDanger } from "../../ui/confirm";
import { t, type Key } from "../../i18n";

const LABELS: Record<string, Key> = {
  start: "svc.start",
  stop: "svc.stop",
  restart: "svc.restart",
  reload: "svc.reload",
  enable: "svc.enable",
  disable: "svc.disable",
};

/** Human label of a service action ("Khởi động lại"). */
export function serviceActionLabel(action: string): string {
  return LABELS[action] ? t(LABELS[action]) : action;
}

/** Asks before stop/disable (typed confirmation on production servers). */
export async function confirmServiceAction(connId: string, name: string, action: string, description?: string): Promise<boolean> {
  if (action !== "stop" && action !== "disable") return true;
  const stop = action === "stop";
  return confirmDanger({
    serverId: connId,
    title: t(stop ? "svc.stopQ" : "svc.disableQ", { name }),
    message: [description, t(stop ? "logs.svc.stopMsg" : "logs.svc.disableMsg")].filter(Boolean).join("\n"),
    confirmText: t(stop ? "svc.stopBtn" : "svc.disableBtn"),
  });
}
