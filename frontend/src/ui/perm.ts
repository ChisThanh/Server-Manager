import { useRef } from "react";
import { useApp } from "../store/app";
import { useSettings } from "../store/settings";

export type Perm = "files" | "terminal" | "exec" | "services" | "docker" | "deploy" | "database" | "backup" | "restore" | "web" | "security" | "users";

/** Whether the server's role allows perm (the backend enforces it too). */
export function can(serverId: string, perm: Perm): boolean {
  const role = useApp.getState().servers.find((s) => s.id === serverId)?.role || "admin";
  const perms = useSettings.getState().rolePerms[role];
  if (!perms) return role === "admin";
  return perms.includes(perm);
}

/** Hook form of can(), re-rendering when the role changes. */
export function useCan(serverId: string, perm: Perm): boolean {
  const role = useApp((s) => s.servers.find((x) => x.id === serverId)?.role || "admin");
  const perms = useSettings((s) => s.rolePerms[role]);
  if (!perms) return role === "admin";
  return perms.includes(perm);
}

/** The server profile (reactive). With active=false (a hidden panel) it keeps
 *  the last value, so list refreshes don't re-render the panel. */
export function useServer(serverId: string, active = true) {
  const last = useRef<ReturnType<typeof find>>(undefined);
  const find = (s: ReturnType<typeof useApp.getState>) => s.servers.find((x) => x.id === serverId);
  return useApp((s) => {
    if (active || last.current === undefined) last.current = find(s);
    return last.current;
  });
}
