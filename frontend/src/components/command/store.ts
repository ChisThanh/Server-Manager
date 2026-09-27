// Command Center state. Lives at module level so a run in progress and the
// form survive leaving the page; backend events are wired once at load.
import { create } from "zustand";
import { Events } from "@wailsio/runtime";
import { CommandService } from "../../../bindings/server-manager/services/command";
import type { Danger, FinishedEvent, Preview, RunInfo, ServerResult, Spec } from "../../../bindings/server-manager/services/command";
import { t } from "../../i18n";
import { toast } from "../../store/ui";

export type ActionKind = "command" | "preset" | "snippets";

export interface Filters {
  q: string;
  group: string; // "" = any, "\u0000" = no group
  env: string; // "" = any, "none" = no environment
  tag: string;
}

/** Live analysis of the action being edited. */
export interface Analysis {
  /** Script that will run (the command, or the rendered preset). */
  script: string;
  dangers: Danger[];
  /** The preset itself is dangerous (reboot). */
  presetDanger: boolean;
  /** The preset forces sudo. */
  presetSudo: boolean;
  /** Validation error of the preset parameters. */
  error: string;
}

interface CmdState {
  // Targets.
  selected: string[];
  filters: Filters;
  // Action.
  kind: ActionKind;
  command: string;
  /** Name of the snippet the command was loaded from (history label). */
  snippet: string;
  preset: string;
  params: Record<string, string>;
  // Options.
  sudo: boolean;
  timeoutSec: number;
  /** 0 = the default from settings. */
  concurrency: number;
  stopOnFailure: boolean;
  dryRun: boolean;
  analysis: Analysis;
  // Results.
  run: RunInfo | null;
  preview: Preview | null;
  historyVersion: number;
  set: (patch: Partial<CmdState>) => void;
  /** Shows a run (just started or reopened from history). */
  openRun: (id: string) => Promise<void>;
}

export const useCmd = create<CmdState>((set) => ({
  selected: [],
  filters: { q: "", group: "", env: "", tag: "" },
  kind: "command",
  command: "",
  snippet: "",
  preset: "disk",
  params: {},
  sudo: false,
  timeoutSec: 60,
  concurrency: 0,
  stopOnFailure: false,
  dryRun: false,
  analysis: { script: "", dangers: [], presetDanger: false, presetSudo: false, error: "" },
  run: null,
  preview: null,
  historyVersion: 0,
  set: (patch) => set(patch),
  openRun: async (id) => {
    const info = await CommandService.GetRun(id);
    set({ run: merge(info), preview: null });
  },
}));

// Events may arrive before Run() returns the id (or before GetRun's snapshot
// lands), so the newest state of recent runs is buffered and merged in.
const live = new Map<string, Map<string, ServerResult>>();
const finished = new Map<string, FinishedEvent>();

const RANK: Record<string, number> = { queued: 0, connecting: 1, running: 2 };
const rank = (s: string) => RANK[s] ?? 3;

function merge(info: RunInfo): RunInfo {
  const buf = live.get(info.id);
  const results = (info.results ?? []).map((r) => {
    const b = buf?.get(r.serverId);
    return b && rank(b.state) >= rank(r.state) ? b : r;
  });
  const fin = finished.get(info.id);
  const out: RunInfo = { ...info, results };
  if (fin && info.status === "running") {
    out.status = fin.status;
    out.summary = fin.summary;
    out.finished = fin.finished;
  } else {
    out.summary = summarize(results);
  }
  return out;
}

export function summarize(results: ServerResult[]) {
  const s = { total: results.length, queued: 0, running: 0, ok: 0, failed: 0, timeout: 0, cancelled: 0, skipped: 0 };
  for (const r of results) {
    switch (r.state) {
      case "queued":
        s.queued++;
        break;
      case "connecting":
      case "running":
        s.running++;
        break;
      case "done":
        s.ok++;
        break;
      case "failed":
        s.failed++;
        break;
      case "timeout":
        s.timeout++;
        break;
      case "cancelled":
        s.cancelled++;
        break;
      case "skipped":
        s.skipped++;
        break;
    }
  }
  return s;
}

Events.On("command:update", (ev) => {
  const r = ev.data as ServerResult;
  let m = live.get(r.runId);
  if (!m) {
    m = new Map();
    live.set(r.runId, m);
    // Keep the buffer small: only the latest few runs matter.
    while (live.size > 5) live.delete(live.keys().next().value!);
  }
  m.set(r.serverId, r);
  const st = useCmd.getState();
  if (st.run?.id !== r.runId) return;
  const results = (st.run.results ?? []).map((x) => (x.serverId === r.serverId ? r : x));
  useCmd.setState({ run: { ...st.run, results, summary: summarize(results) } });
});

Events.On("command:finished", (ev) => {
  const f = ev.data as FinishedEvent;
  finished.set(f.runId, f);
  while (finished.size > 5) finished.delete(finished.keys().next().value!);
  const st = useCmd.getState();
  useCmd.setState({ historyVersion: st.historyVersion + 1 });
  if (st.run?.id !== f.runId) return;
  useCmd.setState({ run: { ...st.run, status: f.status, finished: f.finished, summary: f.summary } });
  const s = f.summary;
  const msg = t("cmd.finishedToast", { ok: s.ok, total: s.total });
  toast(msg, f.status === "done" ? "success" : f.status === "cancelled" ? "info" : "error");
});

/** Builds the spec sent to the backend from the form. */
export function specFromForm(servers: string[]): Spec {
  const st = useCmd.getState();
  const preset = st.kind === "preset";
  return {
    servers,
    kind: preset ? "preset" : "command",
    command: preset ? "" : st.command,
    preset: preset ? st.preset : "",
    params: preset ? st.params : {},
    snippet: preset ? "" : st.snippet,
    sudo: st.sudo,
    timeoutSec: st.timeoutSec,
    concurrency: st.concurrency,
    stopOnFailure: st.stopOnFailure,
  };
}
