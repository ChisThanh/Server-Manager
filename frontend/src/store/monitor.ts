import { useRef } from "react";
import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import { MonitorService } from "../../bindings/server-manager/services/monitor";
import type { Alert, SampleEvent } from "../../bindings/server-manager/services/monitor";
import { useAlertsBadge } from "./alerts";

/** Latest sample per server, pushed by the backend collector. */
export const useSamples = create<{ samples: Record<string, SampleEvent> }>(() => ({ samples: {} }));

/**
 * The latest sample of a server. With active=false the component is not
 * re-rendered for new samples (hidden panels).
 */
export function useSample(serverId: string, active = true): SampleEvent | undefined {
  const last = useRef<SampleEvent | undefined>(undefined);
  return useSamples((s) => {
    if (active || last.current === undefined) last.current = s.samples[serverId];
    return last.current;
  });
}

let started = false;

/** Subscribes to monitor events once (called from App). */
export function startMonitorFeed() {
  if (started) return;
  started = true;
  Events.On("monitor:sample", (ev) => {
    const d = ev.data;
    useSamples.setState((s) => ({ samples: { ...s.samples, [d.server]: d } }));
  });
  Events.On("monitor:alerts", (ev) => useAlertsBadge.setState({ count: ev.data.unacked, critical: ev.data.critical }));
  MonitorService.AlertCount()
    .then((c) => useAlertsBadge.setState({ count: c.unacked, critical: c.critical }))
    .catch(() => {});
  startAlertsFeed();
}

/** Loads the current sample of a server (e.g. when a panel opens). */
export async function primeSample(serverId: string) {
  try {
    const [ev, ok] = await MonitorService.Latest(serverId);
    if (ok && !useSamples.getState().samples[serverId]) {
      useSamples.setState((s) => ({ samples: { ...s.samples, [serverId]: ev } }));
    }
  } catch {
    /* not collecting yet */
  }
}

/** Active (pending + firing) alerts, refreshed on changes and every 20 s. */
export const useActiveAlerts = create<{ alerts: Alert[]; loaded: boolean; reload: () => Promise<void> }>((set) => ({
  alerts: [],
  loaded: false,
  reload: async () => {
    try {
      set({ alerts: (await MonitorService.ActiveAlerts()) ?? [], loaded: true });
    } catch {
      /* ignore */
    }
  },
}));

let alertsFeed = false;
export function startAlertsFeed() {
  if (alertsFeed) return;
  alertsFeed = true;
  const reload = () => useActiveAlerts.getState().reload();
  reload();
  Events.On("monitor:alerts", reload);
  setInterval(reload, 20000);
}
