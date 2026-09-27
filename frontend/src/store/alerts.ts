import { create } from "zustand";

/** Firing, unacknowledged alerts (sidebar badge), fed by store/monitor. */
export const useAlertsBadge = create<{ count: number; critical: number }>(() => ({ count: 0, critical: 0 }));

export function useAlertCount() {
  return useAlertsBadge((s) => s.count);
}
