import { useEffect, useState } from "react";
import { AlertTriangle, RotateCcw } from "lucide-react";
import { WebService } from "../../../bindings/server-manager/services/web";
import type { HistoryEntry, Site } from "../../../bindings/server-manager/services/web/models";
import { CodeEditor } from "../../ui/CodeEditor";
import { SubTabs } from "../../ui/Tabs";
import { Empty } from "../../ui/Page";
import { confirmDanger } from "../../ui/confirm";
import { withSudo } from "../../store/sudo";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { formatDate } from "../../lib/format";
import { Modal } from "../Overlays";
import { useT, type Key } from "../../i18n";
import { isTestFailure } from "./util";
import { langFor } from "./langs";

type Mode = "view" | "edit" | "history";

/** Shows a site's configuration file: read-only, raw edit (validated with
 * nginx -t / caddy validate), and previous versions with restore. */
export function RawEditor(props: { connId: string; site: Site; mode: Mode; canEdit: boolean; onClose: () => void; onSaved: () => void }) {
  const { connId, site } = props;
  const t = useT();
  const [mode, setMode] = useState<Mode>(props.mode === "edit" && !props.canEdit ? "view" : props.mode);
  const [orig, setOrig] = useState<string | null>(null);
  const [text, setText] = useState("");
  const [err, setErr] = useState("");
  const [saving, setSaving] = useState(false);
  const [testOut, setTestOut] = useState("");
  const [history, setHistory] = useState<HistoryEntry[] | null>(null);
  const [picked, setPicked] = useState<HistoryEntry | null>(null);

  const load = () => {
    setErr("");
    withSudo(connId, (pw) => WebService.ReadSite(connId, site.engine, site.file, pw))
      .then((c) => {
        setOrig(c);
        setText(c);
      })
      .catch((e) => setErr(errMsg(e)));
    WebService.History(connId, site.file)
      .then((h) => setHistory(h ?? []))
      .catch(() => setHistory([]));
  };
  useEffect(load, [connId, site.file]);

  const dirty = orig !== null && text !== orig;

  const save = async () => {
    setSaving(true);
    setTestOut("");
    try {
      const r = await withSudo(connId, (pw) => WebService.SaveRaw(connId, site.engine, site.file, text, pw));
      toast(t("web.savedToast", { name: site.name }) + (r.notRunning ? " " + t("web.notRunningNote") : ""), "success");
      props.onSaved();
      load();
    } catch (e) {
      if (isTestFailure(e)) setTestOut((e as { cause?: { detail?: string } }).cause?.detail ?? errMsg(e));
      else toast(errMsg(e), "error");
    } finally {
      setSaving(false);
    }
  };

  const restore = async (h: HistoryEntry) => {
    const ok = await confirmDanger({
      serverId: connId,
      title: t("web.restoreQ"),
      message: t("web.restoreMsg", { date: formatDate(Math.floor(h.ts / 1000)) }),
      confirmText: t("web.restore"),
    });
    if (!ok) return;
    setSaving(true);
    setTestOut("");
    try {
      await withSudo(connId, (pw) => WebService.RestoreVersion(connId, site.engine, site.file, h.ts, pw));
      toast(t("web.restoredToast", { name: site.name }), "success");
      setPicked(null);
      props.onSaved();
      load();
      setMode("view");
    } catch (e) {
      if (isTestFailure(e)) setTestOut((e as { cause?: { detail?: string } }).cause?.detail ?? errMsg(e));
      else toast(errMsg(e), "error");
    } finally {
      setSaving(false);
    }
  };

  const actionKey = (a: string): Key => (a === "delete" ? "web.hist.delete" : a === "restore" ? "web.hist.restore" : a === "raw" ? "web.hist.raw" : "web.hist.update");

  return (
    <Modal
      title={<span className="mono">{site.file}</span>}
      size="xwide"
      onClose={async () => {
        if (!dirty || (await confirmDialog(t("web.discardQ"), undefined, t("web.discard"), true))) props.onClose();
      }}
      footer={
        <>
          <button className="btn" onClick={props.onClose}>
            {t("common.close")}
          </button>
          {mode === "edit" && (
            <button className="btn primary" disabled={!dirty || saving} onClick={save}>
              {saving ? <span className="spinner" /> : null} {t("web.saveTest")}
            </button>
          )}
        </>
      }
    >
      <SubTabs<Mode>
        value={mode}
        onChange={(m) => {
          setMode(m);
          setPicked(null);
        }}
        items={[
          { id: "view", label: t("web.viewConfig") },
          { id: "edit", label: t("web.editRaw"), hidden: !props.canEdit },
          { id: "history", label: t("web.history"), badge: history?.length || undefined },
        ]}
      />
      {err ? (
        <div className="err">{err}</div>
      ) : orig === null ? (
        <div className="ui-center">
          <span className="spinner lg" />
        </div>
      ) : mode === "history" ? (
        <div className="web-history">
          <div className="list-rows web-history-list">
            {(history ?? []).length === 0 && <Empty title={t("web.noHistory")} text={t("web.noHistoryHint")} />}
            {(history ?? []).map((h) => (
              <div key={h.ts} className={`list-row clickable ${picked?.ts === h.ts ? "web-picked" : ""}`} onClick={() => setPicked(h)}>
                <div className="grow">
                  <div>{formatDate(Math.floor(h.ts / 1000))}</div>
                  <div className="muted web-small">
                    {t(actionKey(h.action))} · {h.actor}
                  </div>
                </div>
                {props.canEdit && (
                  <button
                    className="icon-btn"
                    title={t("web.restore")}
                    disabled={saving}
                    onClick={(e) => {
                      e.stopPropagation();
                      restore(h);
                    }}
                  >
                    <RotateCcw size={13} />
                  </button>
                )}
              </div>
            ))}
          </div>
          <div style={{ minWidth: 0 }}>
            {testOut && <pre className="log-view err web-testout">{testOut}</pre>}
            {picked ? <CodeEditor value={picked.content} language={langFor(site.engine)} readOnly height={460} /> : <div className="muted ui-center">{t("web.pickVersion")}</div>}
          </div>
        </div>
      ) : (
        <>
          {mode === "edit" && site.managed && (
            <div className="web-alert warn">
              <AlertTriangle size={14} /> {t("web.managedRawWarn")}
            </div>
          )}
          {mode === "edit" && (
            <div className="hint" style={{ marginBottom: 8 }}>
              {t(site.engine === "caddy" ? "web.rawHintCaddy" : "web.rawHint")}
            </div>
          )}
          {testOut && (
            <div className="web-alert err">
              <AlertTriangle size={14} />
              <div style={{ minWidth: 0, flex: 1 }}>
                <div>{t("web.testFailedMsg")}</div>
                <pre className="log-view err web-testout">{testOut}</pre>
              </div>
            </div>
          )}
          <CodeEditor value={mode === "edit" ? text : orig} onChange={mode === "edit" ? setText : undefined} language={langFor(site.engine)} readOnly={mode !== "edit"} height={480} />
        </>
      )}
    </Modal>
  );
}
