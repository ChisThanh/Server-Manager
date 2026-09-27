import { create } from "zustand";

function load(key: string, def: boolean) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? def : v === "1";
  } catch {
    return def;
  }
}

function save(key: string, v: boolean) {
  try {
    localStorage.setItem(key, v ? "1" : "0");
  } catch {
    /* storage unavailable */
  }
}

export const usePrefs = create<{ sidebarCollapsed: boolean; toggleSidebar: () => void; wsNavCollapsed: boolean; toggleWsNav: () => void }>((set, get) => ({
  sidebarCollapsed: load("sm.sidebarCollapsed", false),
  toggleSidebar: () => {
    const v = !get().sidebarCollapsed;
    save("sm.sidebarCollapsed", v);
    set({ sidebarCollapsed: v });
  },
  wsNavCollapsed: load("sm.wsNavCollapsed", false),
  toggleWsNav: () => {
    const v = !get().wsNavCollapsed;
    save("sm.wsNavCollapsed", v);
    set({ wsNavCollapsed: v });
  },
}));
