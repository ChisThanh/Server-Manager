import { useEffect, useMemo, useState } from "react";
import { Eye, History as HistoryIcon, Play, TerminalSquare } from "lucide-react";
import { CommandService } from "../../../bindings/server-manager/services/command";
import { Page, PageHeader } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { useRemote } from "../../ui/hooks";
import { useApp } from "../../store/app";
import { useSettings } from "../../store/settings";
import { toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { useT } from "../../i18n";
import { useCmd, specFromForm } from "../command/store";
import { TargetPicker, runnable } from "../command/TargetPicker";
import { ActionPanel } from "../command/ActionPanel";
import { PreviewView, Results } from "../command/Results";
import { History } from "../command/History";
import { confirmRun } from "../command/util";
import "../command/command.css";

type Tab = "run" | "history";

export default function CommandCenterPage() {
  const t = useT();
  const [tab, setTab] = useState<Tab>("run");
  const [busy, setBusy] = useState(false);
  const servers = useApp((s) => s.servers);
  const targets = useRemote(async () => (await CommandService.Targets()) ?? [], [servers], { poll: 5000 });
  const list = targets.data ?? [];
  const st = useCmd();
  const defaultConc = useSettings((s) => s.settings.commandConcurrency);

  // Reattach to a run started before the page was (re)opened.
  useEffect(() => {
    if (useCmd.getState().run) return;
    CommandService.Active()
      .then((runs) => {
        const r = (runs ?? []).sort((a, b) => b.started - a.started)[0];
        if (r && !useCmd.getState().run) useCmd.getState().openRun(r.id);
      })
      .catch(() => {});
  }, []);

  const chosen = useMemo(() => list.filter((x) => st.selected.includes(x.id)), [list, st.selected]);
  const effectiveSudo = st.kind === "preset" ? st.sudo || st.analysis.presetSudo : st.sudo;
  const sudoUnknown = effectiveSudo ? chosen.filter((x) => runnable(x) && x.sudo === "unknown").length : 0;
  const running = st.run?.status === "running";
  const timeoutOK = Number.isInteger(st.timeoutSec) && st.timeoutSec >= 5 && st.timeoutSec <= 3600;
  const concOK = st.concurrency === 0 || (Number.isInteger(st.concurrency) && st.concurrency >= 1 && st.concurrency <= 50);
  const hasAction = st.kind !== "snippets" && !st.analysis.error && (st.kind === "preset" || st.command.trim() !== "");
  const runnableCount = chosen.filter(runnable).length;

  let blocker = "";
  if (st.kind === "snippets") blocker = t("cmd.pickSnippet");
  else if (!hasAction) blocker = st.analysis.error || t("cmd.needCommand");
  else if (chosen.length === 0) blocker = t("cmd.needServers");
  else if (!timeoutOK) blocker = t("cmd.rangeError", { min: 5, max: 3600 });
  else if (!concOK) blocker = t("cmd.rangeError", { min: 1, max: 50 });
  else if (running && !st.dryRun) blocker = t("cmd.alreadyRunning");

  const start = async () => {
    if (blocker || busy) return;
    setBusy(true);
    try {
      const spec = specFromForm(chosen.map((x) => x.id));
      const pv = await CommandService.Preview(spec);
      if (st.dryRun) {
        useCmd.getState().set({ preview: pv });
        return;
      }
      const n = (pv.servers ?? []).length;
      const ok = await confirmRun(pv, t("cmd.confirmTitle", { n }), t("cmd.run"));
      if (!ok) return;
      const id = await CommandService.Run(spec);
      await useCmd.getState().openRun(id);
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const openFromHistory = async (id: string) => {
    try {
      await useCmd.getState().openRun(id);
      setTab("run");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <Page className="cmd-page">
      <PageHeader icon={<TerminalSquare size={18} />} title={t("app.page.command")} sub={t("cmd.subtitle")} />
      <SubTabs<Tab>
        items={[
          { id: "run", label: t("cmd.tab.run"), icon: <Play size={13} />, badge: running ? "●" : undefined },
          { id: "history", label: t("cmd.tab.history"), icon: <HistoryIcon size={13} /> },
        ]}
        value={tab}
        onChange={setTab}
      />
      {tab === "history" ? (
        <History onOpen={openFromHistory} />
      ) : (
        <>
          <div className="cmd-grid">
            {targets.error && !targets.data ? (
              <div className="panel-box">
                <div className="cmd-hint err">{targets.error}</div>
                <button className="btn sm" onClick={targets.reload}>
                  {t("common.retry")}
                </button>
              </div>
            ) : (
              <TargetPicker targets={list} />
            )}
            <div className="cmd-right">
              <ActionPanel onRun={start} />
              <div className="panel-box cmd-options">
                <div className="cmd-opts">
                  <label className="check" title={st.analysis.presetSudo && st.kind === "preset" ? t("cmd.needsRoot") : t("cmd.opt.sudoHint")}>
                    <input
                      type="checkbox"
                      checked={effectiveSudo}
                      disabled={st.kind === "preset" && st.analysis.presetSudo}
                      onChange={(e) => st.set({ sudo: e.target.checked })}
                    />
                    {t("cmd.opt.sudo")}
                  </label>
                  <label className="cmd-num">
                    <span>{t("cmd.opt.timeout")}</span>
                    <input
                      className={`input input-sm ${timeoutOK ? "" : "invalid"}`}
                      type="number"
                      min={5}
                      max={3600}
                      value={st.timeoutSec}
                      onChange={(e) => st.set({ timeoutSec: Number(e.target.value) })}
                    />
                    <span className="muted">s</span>
                  </label>
                  <label className="cmd-num">
                    <span>{t("cmd.opt.concurrency")}</span>
                    <input
                      className={`input input-sm ${concOK ? "" : "invalid"}`}
                      type="number"
                      min={1}
                      max={50}
                      placeholder={String(defaultConc)}
                      value={st.concurrency || ""}
                      onChange={(e) => st.set({ concurrency: e.target.value === "" ? 0 : Number(e.target.value) })}
                    />
                  </label>
                  <label className="check" title={t("cmd.opt.stopOnFailureHint")}>
                    <input type="checkbox" checked={st.stopOnFailure} onChange={(e) => st.set({ stopOnFailure: e.target.checked })} />
                    {t("cmd.opt.stopOnFailure")}
                  </label>
                  <label className="check" title={t("cmd.opt.dryRunHint")}>
                    <input type="checkbox" checked={st.dryRun} onChange={(e) => st.set({ dryRun: e.target.checked })} />
                    {t("cmd.opt.dryRun")}
                  </label>
                </div>
                {sudoUnknown > 0 && <div className="cmd-hint warn">{t("cmd.sudoUnknownWarn", { n: sudoUnknown })}</div>}
                <div className="cmd-runbar">
                  <span className="muted">{blocker || t("cmd.runSummary", { n: runnableCount, total: chosen.length })}</span>
                  <div className="grow" />
                  <button
                    className={`btn ${st.dryRun ? "" : st.analysis.dangers.length > 0 || st.analysis.presetDanger ? "danger" : "primary"}`}
                    disabled={!!blocker || busy}
                    onClick={start}
                    title="⌘/Ctrl+Enter"
                  >
                    {busy ? <span className="spinner" /> : st.dryRun ? <Eye size={14} /> : <Play size={14} />}{" "}
                    {st.dryRun ? t("cmd.preview") : t("cmd.runOn", { n: chosen.length })}
                  </button>
                </div>
              </div>
            </div>
          </div>
          {st.preview && <PreviewView pv={st.preview} onClose={() => st.set({ preview: null })} />}
          {st.run && <Results run={st.run} />}
        </>
      )}
    </Page>
  );
}
