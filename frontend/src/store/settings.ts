import { create } from "zustand";
import { AppService } from "../../bindings/server-manager/services";
import type { Settings } from "../../bindings/server-manager/internal/core";
import { errMsg } from "../lib/api";
import { toast } from "./ui";

const defaults: Settings = {
  operatorName: "",
  collectInterval: 15,
  backgroundMode: false,
  terminalIdleMinutes: 0,
  recordTerminal: false,
  desktopNotify: true,
  confirmProduction: true,
  auditRetentionDays: 0,
  eventRetentionDays: 90,
  commandConcurrency: 5,
  notifyLang: "vi",
};

interface SettingsState {
  settings: Settings;
  rolePerms: Record<string, string[]>;
  load: () => Promise<void>;
  save: (s: Settings) => Promise<boolean>;
}

export const useSettings = create<SettingsState>((set) => ({
  settings: defaults,
  rolePerms: {},
  load: async () => {
    try {
      const [s, rp] = await Promise.all([AppService.GetSettings(), AppService.RolePerms()]);
      set({ settings: s, rolePerms: Object.fromEntries(Object.entries(rp ?? {}).map(([k, v]) => [k, v ?? []])) });
    } catch (e) {
      toast(errMsg(e), "error");
    }
  },
  save: async (s) => {
    try {
      set({ settings: await AppService.SaveSettings(s) });
      return true;
    } catch (e) {
      toast(errMsg(e), "error");
      return false;
    }
  },
}));
