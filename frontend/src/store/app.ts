import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import { ServerService, errMsg, formatAppError, type Server, type TransferEvent } from "../lib/api";
import { t } from "../i18n";
import { confirmDialog, toast, useUI } from "./ui";
import { dropConn, hasDirtyTabs } from "./editorTabs";

export type ConnStatus = "connecting" | "connected" | "disconnected";

export interface ConnState {
  status: ConnStatus;
  message?: string;
  home?: string;
}

interface AppState {
  servers: Server[];
  /** Servers that have an open workspace, in tab order. */
  open: string[];
  active: string | null;
  conns: Record<string, ConnState>;
  transfers: Record<string, TransferEvent>;
  /** Sudo passwords typed during this session, per server. */
  sudo: Record<string, string>;
  /** Global page shown instead of a workspace (fleet, alerts…), or null. */
  page: string | null;
  setPage: (p: string | null) => void;

  loadServers: () => Promise<void>;
  setActive: (id: string | null) => void;
  connect: (id: string) => Promise<boolean>;
  disconnect: (id: string) => Promise<void>;
  reconnect: (id: string) => Promise<void>;
  closeWorkspace: (id: string) => Promise<void>;
  deleteServer: (id: string) => Promise<void>;
  setConn: (id: string, patch: Partial<ConnState>) => void;
  dismissTransfer: (id: string) => void;
}

export const useApp = create<AppState>((set, get) => ({
  servers: [],
  open: [],
  active: null,
  conns: {},
  transfers: {},
  sudo: {},
  page: null,
  setPage: (page) => set({ page }),

  loadServers: async () => {
    try {
      set({ servers: (await ServerService.List()) ?? [] });
    } catch (e) {
      toast(errMsg(e), "error");
    }
  },

  setActive: (id) => set({ active: id, page: null }),

  setConn: (id, patch) =>
    set((s) => ({
      conns: { ...s.conns, [id]: { ...(s.conns[id] ?? { status: "disconnected" }), ...patch } },
    })),

  connect: async (id) => {
    const server = get().servers.find((s) => s.id === id);
    if (!server) return false;
    const { setConn } = get();
    set((s) => ({ open: s.open.includes(id) ? s.open : [...s.open, id], active: id, page: null }));
    setConn(id, { status: "connecting", message: "" });

    let secret = "";
    let save = false;
    for (let attempt = 0; attempt < 6; attempt++) {
      try {
        const r = await ServerService.Connect(id, secret, save);
        if (r.status === "ok") {
          setConn(id, { status: "connected", home: r.home, message: "" });
          get().loadServers();
          return true;
        }
        if (r.status === "need-secret") {
          const isKey = server.authType === "key";
          const res = await useUI.getState().openDialog({
            title: t("conn.loginTitle", { name: server.name }),
            message: `${server.user}@${server.host}:${server.port}`,
            confirmText: t("conn.connect"),
            fields: [
              {
                name: "secret",
                label: isKey ? t("conn.passphrase") : t("conn.password"),
                type: "password",
                autoFocus: true,
              },
              { name: "save", label: t("conn.saveKeychain"), type: "checkbox", value: true },
            ],
          });
          if (res?.action !== "ok" || !res.values.secret) break;
          secret = String(res.values.secret);
          save = Boolean(res.values.save);
          continue;
        }
        if (r.status === "hostkey" && r.hostKey) {
          const hk = r.hostKey;
          const vars = { host: hk.host, port: hk.port };
          const ok = await confirmDialog(
            hk.mismatch ? t("conn.hostChangedTitle") : t("conn.hostNewTitle"),
            `${hk.mismatch ? t("conn.hostChangedMsg", vars) : t("conn.hostNewMsg", vars)}\n\n${hk.keyType}\n${hk.fingerprint}`,
            hk.mismatch ? t("conn.trustNew") : t("conn.trustConnect"),
            hk.mismatch,
          );
          if (!ok) break;
          await ServerService.TrustHostKey(hk.host, hk.port, hk.keyBase64);
          continue;
        }
        break;
      } catch (e) {
        setConn(id, { status: "disconnected", message: errMsg(e) });
        toast(errMsg(e), "error");
        return false;
      }
    }
    setConn(id, { status: "disconnected", message: t("conn.cancelled") });
    return false;
  },

  disconnect: async (id) => {
    await ServerService.Disconnect(id);
    get().setConn(id, { status: "disconnected", message: "" });
  },

  reconnect: async (id) => {
    if (get().conns[id]?.status === "connected") {
      get().setConn(id, { status: "connecting" });
      try {
        await ServerService.Reconnect(id);
        get().setConn(id, { status: "connected", message: "" });
        return;
      } catch {
        // fall through to a full connect (may prompt for credentials)
      }
    }
    await get().connect(id);
  },

  closeWorkspace: async (id) => {
    if (hasDirtyTabs(id) && !(await confirmDialog(t("conn.closeTitle"), t("conn.closeMsg"), t("conn.closeAnyway"), true))) {
      return;
    }
    await ServerService.Disconnect(id).catch(() => {});
    dropConn(id);
    set((s) => {
      const open = s.open.filter((x) => x !== id);
      const conns = { ...s.conns };
      delete conns[id];
      return { open, conns, active: s.active === id ? (open[open.length - 1] ?? null) : s.active };
    });
  },

  deleteServer: async (id) => {
    await get().closeWorkspace(id);
    await ServerService.Delete(id);
    await get().loadServers();
  },

  dismissTransfer: (id) =>
    set((s) => {
      const t = { ...s.transfers };
      delete t[id];
      return { transfers: t };
    }),
}));

// Backend → store wiring. Registered once at module load.
Events.On("conn:status", (ev) => {
  const { id, status } = ev.data;
  const message = formatAppError(ev.data.error);
  const st = useApp.getState();
  if (!st.open.includes(id)) return;
  const prev = st.conns[id]?.status;
  st.setConn(id, { status: status as ConnStatus, message });
  if (prev === "connected" && status === "disconnected" && message) {
    toast(`${st.servers.find((s) => s.id === id)?.name ?? "Server"}: ${message}`, "error");
  }
});

Events.On("transfer:progress", (ev) => {
  const tr = ev.data;
  useApp.setState((s) => ({ transfers: { ...s.transfers, [tr.id]: tr } }));
  const name = tr.more > 0 ? t("tr.more", { name: tr.name, n: tr.more }) : tr.name;
  if (tr.state === "done") {
    toast(t(tr.kind === "upload" ? "tr.doneUpload" : "tr.doneDownload", { name }), "success");
    setTimeout(() => useApp.getState().dismissTransfer(tr.id), 4000);
  } else if (tr.state === "error") {
    toast(t(tr.kind === "upload" ? "tr.errUpload" : "tr.errDownload", { name, error: formatAppError(tr.error) }), "error");
  }
});
