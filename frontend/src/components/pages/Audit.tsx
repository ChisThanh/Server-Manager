import { useEffect, useState } from "react";
import { Download, ScrollText, ShieldCheck, ShieldAlert } from "lucide-react";
import { AppService } from "../../../bindings/server-manager/services";
import type { AuditEntry, AuditQuery, AuditVerification } from "../../../bindings/server-manager/internal/core";
import { Page, PageHeader, Empty, Loading, ErrorBox } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useApp } from "../../store/app";
import { toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { useT } from "../../i18n";
import { Modal } from "../Overlays";
import { actionLabel } from "../timeline/Timeline";
import { RecordingPlayer } from "../terminal/RecordingPlayer";
import "./pages.css";
import { Select } from "../../ui/Select";

const PAGE = 200;
const RANGES: [string, number][] = [
  ["24h", 86400e3],
  ["7d", 7 * 86400e3],
  ["30d", 30 * 86400e3],
  ["all", 0],
];

export default function AuditPage() {
  const t = useT();
  const servers = useApp((s) => s.servers);
  const [server, setServer] = useState("");
  const [action, setAction] = useState("");
  const [text, setText] = useState("");
  const [range, setRange] = useState("7d");
  const [rows, setRows] = useState<AuditEntry[] | null>(null);
  const [more, setMore] = useState(false);
  const [error, setError] = useState("");
  const [verify, setVerify] = useState<AuditVerification | null>(null);
  const [detail, setDetail] = useState<AuditEntry | null>(null);
  const [replay, setReplay] = useState<string | null>(null);
  const recording = detail?.detail.match(/recording: (\S+\.cast)/)?.[1];

  const query = (before = 0): AuditQuery => {
    const span = RANGES.find((r) => r[0] === range)?.[1] ?? 0;
    return { server, action, text: text.trim(), since: span ? Date.now() - span : 0, until: 0, limit: PAGE, before };
  };
  const load = async () => {
    try {
      const l = (await AppService.Audit(query())) ?? [];
      setRows(l);
      setMore(l.length >= PAGE);
      setError("");
    } catch (e) {
      setError(errMsg(e));
    }
  };
  useEffect(() => {
    const h = setTimeout(load, 250);
    return () => clearTimeout(h);
  }, [server, action, text, range]);

  const loadMore = async () => {
    const last = rows?.[rows.length - 1];
    if (!last) return;
    const l = (await AppService.Audit(query(last.id))) ?? [];
    setRows((r) => [...(r ?? []), ...l]);
    setMore(l.length >= PAGE);
  };
  const runVerify = async () => {
    try {
      setVerify(await AppService.VerifyAudit());
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  const exportCsv = async () => {
    try {
      const p = await AppService.ExportAudit(query(), t("audit.exportTitle"));
      if (p) toast(t("audit.exported", { path: p }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <Page className="page">
      <PageHeader
        icon={<ScrollText size={18} />}
        title={t("app.page.audit")}
        sub={t("audit.sub")}
        actions={
          <>
            <button className="btn sm" onClick={runVerify}>
              <ShieldCheck size={13} /> {t("audit.verify")}
            </button>
            <button className="btn sm" onClick={exportCsv}>
              <Download size={13} /> {t("audit.export")}
            </button>
          </>
        }
      />
      {verify && (
        <div className={`hint-box ${verify.ok ? "audit-ok" : "audit-bad"}`} style={{ marginBottom: 10, display: "flex", gap: 8, alignItems: "center" }}>
          {verify.ok ? <ShieldCheck size={16} /> : <ShieldAlert size={16} />}
          {verify.ok ? t("audit.verifyOk", { n: verify.checked }) : t("audit.verifyBad", { id: verify.brokenAt })}
        </div>
      )}
      <div className="toolbar">
        <input className="input" placeholder={t("audit.search")} value={text} onChange={(e) => setText(e.target.value)} />
        <Select value={server} onChange={setServer} options={[{ value: "", label: t("mon.allServers") }, ...servers.map((s) => ({ value: s.id, label: s.name }))]} />
        <Select
          value={action}
          onChange={setAction}
          options={[
            { value: "", label: t("audit.allActions") },
            ...["file.", "terminal.", "command.", "cmd.", "service.", "process.", "docker.", "deploy.", "web.", "sec.", "db.", "backup.", "server.", "alert.", "monitor.", "hostkey."].map((a) => ({ value: a, label: a.replace(/\.$/, "") })),
          ]}
        />
        <div className="segmented" style={{ width: "auto" }}>
          {RANGES.map(([id]) => (
            <button key={id} className={range === id ? "on" : ""} onClick={() => setRange(id)}>
              {id === "all" ? t("audit.all") : id}
            </button>
          ))}
        </div>
      </div>
      {error && <ErrorBox error={error} onRetry={load} />}
      {!rows && !error && <Loading />}
      {rows && rows.length === 0 && <Empty title={t("audit.empty")} />}
      {rows && rows.length > 0 && (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("audit.time")}</th>
                <th>{t("audit.actor")}</th>
                <th>{t("mon.server")}</th>
                <th>{t("audit.action")}</th>
                <th>{t("audit.target")}</th>
                <th>{t("audit.result")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((e) => (
                <tr key={e.id} onClick={() => setDetail(e)} style={{ cursor: "pointer" }}>
                  <td className="muted" style={{ whiteSpace: "nowrap" }}>
                    {new Date(e.ts).toLocaleString()}
                  </td>
                  <td>{e.actor}</td>
                  <td>{e.serverName || "–"}</td>
                  <td>{actionLabel(e.action)}</td>
                  <td className="audit-detail mono" title={e.target}>
                    {e.target}
                  </td>
                  <td>{e.ok ? <StateBadge tone="ok">OK</StateBadge> : <StateBadge tone="err">{t("audit.failed")}</StateBadge>}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {more && (
            <div style={{ display: "grid", placeItems: "center", padding: 10 }}>
              <button className="btn sm" onClick={loadMore}>
                {t("mon.loadMore")}
              </button>
            </div>
          )}
        </div>
      )}
      {detail && (
        <Modal
          title={actionLabel(detail.action)}
          size="wide"
          onClose={() => setDetail(null)}
          footer={
            <>
              {recording && (
                <button className="btn left" onClick={() => setReplay(recording)}>
                  {t("term.replay")}
                </button>
              )}
              <button className="btn" onClick={() => setDetail(null)}>
                {t("common.close")}
              </button>
            </>
          }
        >
          <div className="ui-kv">
            {(
              [
                [t("audit.time"), new Date(detail.ts).toLocaleString()],
                [t("audit.actor"), detail.actor],
                [t("mon.server"), detail.serverName || "–"],
                [t("audit.action"), `${actionLabel(detail.action)} (${detail.action})`],
                [t("audit.target"), detail.target],
                [t("audit.detail"), detail.detail || "–"],
                [t("audit.result"), detail.ok ? "OK" : detail.error],
                ["SHA-256", detail.hash],
              ] as [string, string][]
            ).map(([k, v]) => (
              <div className="ui-kv-row" key={k}>
                <div className="k">{k}</div>
                <div className="v mono" style={{ whiteSpace: "pre-wrap" }}>
                  {v}
                </div>
              </div>
            ))}
          </div>
        </Modal>
      )}
      {replay && <RecordingPlayer file={replay} onClose={() => setReplay(null)} />}
    </Page>
  );
}
