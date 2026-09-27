import { useMemo, useState } from "react";
import { ExternalLink, Play, Power, PowerOff, RotateCcw, RefreshCw, Square } from "lucide-react";
import { LogsService, type LogLine } from "../../../bindings/server-manager/services/logs";
import { Modal } from "../Overlays";
import { ErrorBox, KV, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { CodeEditor } from "../../ui/CodeEditor";
import { useRemote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { errMsg } from "../../lib/api";
import { formatBytes, formatDuration } from "../../lib/format";
import { withSudo } from "../../store/sudo";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import { LogView, withIds } from "./LogView";
import { confirmServiceAction, serviceActionLabel } from "./serviceActions";
import "./logs.css";

/**
 * Service detail modal: state, resource usage, restart policy with the
 * auto-restart drop-in toggle, dependencies, read-only unit file and the
 * last journal lines. Polls every 5 s while open.
 */
export function ServiceDetailModal(props: {
  connId: string;
  name: string;
  onClose: () => void;
  onChanged?: () => void;
  navigate?: (panel: string, arg?: unknown) => void;
}) {
  const t = useT();
  const { connId, name } = props;
  const canChange = useCan(connId, "services");
  const d = useRemote(() => LogsService.ServiceDetail(connId, name), [connId, name], { poll: 5000 });
  const [busy, setBusy] = useState<string | null>(null);
  const [sudoLogs, setSudoLogs] = useState<LogLine[] | null>(null);
  const [logsErr, setLogsErr] = useState("");

  const act = async (action: string) => {
    const info = d.data;
    if (!(await confirmServiceAction(connId, name, action, info?.description))) return;
    setBusy(action);
    try {
      await withSudo(connId, (pw) => LogsService.ServiceControl(connId, name, action, pw));
      toast(t("svc.ok", { name, action: serviceActionLabel(action) }), "success");
      props.onChanged?.();
      await d.reload();
    } catch (e) {
      toast(`${name}: ${errMsg(e)}`, "error");
    } finally {
      setBusy(null);
    }
  };

  const toggleAuto = async (on: boolean) => {
    setBusy("auto");
    try {
      await withSudo(connId, (pw) => LogsService.SetAutoRestart(connId, name, on, pw));
      toast(t(on ? "logs.svc.d.autoRestartOn" : "logs.svc.d.autoRestartOff", { name }), "success");
      await d.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
    }
  };

  const loadLogsSudo = async () => {
    setBusy("logs");
    try {
      const r = await withSudo(connId, (pw) =>
        LogsService.Query(connId, { kind: "unit", target: name, lines: 50, since: 0, until: 0, level: "", search: "", regex: false, caseSensitive: false }, pw),
      );
      setSudoLogs(r.lines ?? []);
      setLogsErr("");
    } catch (e) {
      setLogsErr(errMsg(e));
    } finally {
      setBusy(null);
    }
  };

  const info = d.data;
  const logLines = useMemo(() => withIds(info && !info.logsNeedSudo ? info.logs : sudoLogs), [info?.logs, info?.logsNeedSudo, sudoLogs]);

  const openLogs = () => {
    props.navigate?.("logs", { source: "unit", unit: name });
    props.onClose();
  };

  const running = info?.activeState === "active" || info?.activeState === "reloading";
  const actBtn = (action: string, icon: React.ReactNode) => (
    <button key={action} className="btn sm" disabled={!!busy || !canChange} onClick={() => act(action)}>
      {busy === action ? <span className="spinner" /> : icon} {serviceActionLabel(action)}
    </button>
  );

  const deps: [string, string[] | null][] = info
    ? [
        ["Requires", info.requires],
        ["Wants", info.wants],
        ["After", info.after],
        ["Before", info.before],
        ["WantedBy", info.wantedBy],
        ["RequiredBy", info.requiredBy],
      ]
    : [];

  return (
    <Modal
      size="xwide"
      title={
        <div className="logs-sd-head">
          <span className="mono">{name}</span>
          {info && <StateBadge state={info.activeState === "failed" ? "failed" : info.subState || info.activeState} />}
          {info?.unitFileState && <span className={`logs-svc-fs ${info.unitFileState}`}>{info.unitFileState}</span>}
          {d.loading && info && <span className="spinner" />}
        </div>
      }
      onClose={props.onClose}
      footer={
        <>
          {props.navigate && (
            <button className="btn" onClick={openLogs}>
              <ExternalLink size={13} /> {t("logs.svc.d.openLogs")}
            </button>
          )}
          <div style={{ flex: 1 }} />
          <button className="btn" onClick={props.onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      {!info ? (
        d.error ? (
          <ErrorBox error={d.error} onRetry={d.reload} />
        ) : (
          <Loading />
        )
      ) : (
        <div className="logs-sd">
          {info.description && <div className="muted">{info.description}</div>}
          {!canChange && <div className="logs-hint">{t("logs.svc.readOnly")}</div>}
          <div className="logs-sd-actions">
            {running ? actBtn("stop", <Square size={12} />) : actBtn("start", <Play size={13} />)}
            {actBtn("restart", <RotateCcw size={13} />)}
            {running && actBtn("reload", <RefreshCw size={13} />)}
            {info.unitFileState === "enabled"
              ? actBtn("disable", <PowerOff size={13} />)
              : info.unitFileState === "disabled" && actBtn("enable", <Power size={13} />)}
          </div>

          <div className="cards">
            <div className="card">
              <div className="k">{t("logs.svc.d.cpu")}</div>
              <div className="v">{info.cpuPercent >= 0 && running ? `${info.cpuPercent.toFixed(1)}%` : "—"}</div>
            </div>
            <div className="card">
              <div className="k">{t("logs.svc.d.memory")}</div>
              <div className="v">{info.memory > 0 ? formatBytes(info.memory) : "—"}</div>
            </div>
            <div className="card">
              <div className="k">{t("logs.svc.d.uptime")}</div>
              <div className="v">{info.uptime >= 0 ? formatDuration(info.uptime) : t("logs.svc.d.notRunning")}</div>
            </div>
            <div className="card">
              <div className="k">{t("logs.svc.d.tasks")}</div>
              <div className="v">{info.tasks >= 0 ? info.tasks : "—"}</div>
            </div>
            <div className="card">
              <div className="k">{t("logs.svc.d.restarts")}</div>
              <div className="v">{info.nRestarts}</div>
            </div>
          </div>

          <div className="logs-sd-cols">
            <div>
              <KV
                items={[
                  [t("logs.svc.d.state"), `${info.activeState} (${info.subState})`],
                  [t("logs.svc.d.mainPid"), info.mainPid > 0 ? info.mainPid : "—"],
                  [t("logs.svc.d.since"), info.since || "—"],
                  [t("logs.svc.d.type"), info.type || "—"],
                  [t("logs.svc.d.user"), info.user || t("logs.svc.d.root")],
                  [t("logs.svc.d.restart"), `${info.restart || "no"}${info.restartSec ? ` · ${info.restartSec}` : ""}`],
                  [t("logs.svc.d.execStart"), <span className="logs-sd-exec">{info.execStart || "—"}</span>],
                  [t("logs.svc.d.fragment"), <span className="mono">{info.fragmentPath || "—"}</span>],
                  [
                    t("logs.svc.d.dropins"),
                    info.dropInPaths?.length ? (
                      <span className="mono">
                        {info.dropInPaths.map((p) => (
                          <div key={p}>{p}</div>
                        ))}
                      </span>
                    ) : (
                      t("logs.svc.d.none")
                    ),
                  ],
                ]}
              />
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              <div className="logs-sd-auto">
                <label className="check">
                  <input type="checkbox" checked={info.autoRestart} disabled={!!busy || !canChange} onChange={(e) => toggleAuto(e.target.checked)} />
                  <span>{t("logs.svc.d.autoRestart")}</span>
                  {busy === "auto" && <span className="spinner" />}
                </label>
                <div className="logs-hint">{t("logs.svc.d.autoRestartHint", { path: info.autoRestartPath })}</div>
                {!info.autoRestart && info.restart && info.restart !== "no" && (
                  <div className="logs-hint">{t("logs.svc.d.autoRestartOwn", { value: info.restart })}</div>
                )}
              </div>
              <div>
                <h4>{t("logs.svc.d.deps")}</h4>
                <div className="logs-sd-deps">
                  {deps
                    .filter(([, v]) => v && v.length)
                    .map(([k, v]) => (
                      <div key={k} style={{ display: "contents" }}>
                        <div className="k">{k}</div>
                        <div className="v">
                          {v!.map((x) => (
                            <span key={x} className="chip">
                              {x}
                            </span>
                          ))}
                        </div>
                      </div>
                    ))}
                  {deps.every(([, v]) => !v || !v.length) && <div className="muted">{t("logs.svc.d.none")}</div>}
                </div>
              </div>
            </div>
          </div>

          <div>
            <h4>
              {t("logs.svc.d.recentLogs")}
              <span className="grow" />
              {info.logsNeedSudo && !sudoLogs && (
                <button className="btn sm" onClick={loadLogsSudo} disabled={busy === "logs"}>
                  {busy === "logs" && <span className="spinner" />} {t("logs.svc.d.loadLogsSudo")}
                </button>
              )}
            </h4>
            {(logsErr || info.logsError) && <div className="logs-error">{logsErr || info.logsError}</div>}
            <div className="logs-sd-logs">
              <LogView lines={logLines} showSource={false} wrap empty={t("logs.svc.d.noLogs")} />
            </div>
          </div>

          <div>
            <h4>{t("logs.svc.d.unitFile")}</h4>
            <div className="logs-hint" style={{ marginBottom: 6 }}>
              {t("logs.svc.d.unitFileHint", { path: info.fragmentPath || "—", name })}
            </div>
            <CodeEditor value={info.unitFile} language="ini" readOnly height={260} />
          </div>
        </div>
      )}
    </Modal>
  );
}

