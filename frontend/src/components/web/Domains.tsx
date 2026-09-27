import { useMemo, useState } from "react";
import {
  AlertTriangle,
  ArrowRightLeft,
  CheckCircle2,
  CornerUpRight,
  Eye,
  FileCode2,
  Folder,
  Globe,
  History as HistoryIcon,
  Lock,
  LockOpen,
  Pencil,
  Plus,
  Server as ServerIcon,
  Trash2,
  XCircle,
} from "lucide-react";
import { WebService } from "../../../bindings/server-manager/services/web";
import type { CertOverview, EngineInfo, Site, SitesResult, WebStatus } from "../../../bindings/server-manager/services/web/models";
import { Empty, ErrorBox, Loading, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import type { Remote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { confirmDanger } from "../../ui/confirm";
import { runJob } from "../../ui/jobs";
import { withSudo } from "../../store/sudo";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { formatDate } from "../../lib/format";
import { Modal } from "../Overlays";
import { useT } from "../../i18n";
import { certByPath, certStatusKey, certTone, daysText, editableEngines, engineLabel, kindKey } from "./util";
import { SiteForm } from "./SiteForm";
import { RawEditor } from "./RawEditor";

export function Domains(props: { connId: string; status: WebStatus; sites: Remote<SitesResult>; certs: CertOverview | undefined; onChanged: () => void }) {
  const { connId, status, sites } = props;
  const t = useT();
  const canEdit = useCan(connId, "web");
  const [q, setQ] = useState("");
  const [engine, setEngine] = useState<"all" | "nginx" | "caddy">("all");
  const [form, setForm] = useState<{ site?: Site; engine: string } | null>(null);
  const [raw, setRaw] = useState<{ site: Site; mode: "view" | "edit" | "history" } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [testOut, setTestOut] = useState<{ title: string; text: string } | null>(null);

  const editable = editableEngines(status.engines);
  const certs = useMemo(() => certByPath(props.certs?.certificates), [props.certs]);
  const list = sites.data?.sites ?? [];
  const engines = (status.engines ?? []).filter((e) => e.installed);
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return list.filter((s) => {
      if (engine !== "all" && s.engine !== engine) return false;
      if (!needle) return true;
      return [s.name, s.file, s.target, ...(s.domains ?? [])].some((x) => x.toLowerCase().includes(needle));
    });
  }, [list, q, engine]);

  const toggle = async (s: Site) => {
    if (s.enabled && !(await confirmDialog(t("web.disableQ", { name: s.name }), t("web.disableMsg"), t("web.disable"), true))) return;
    setBusy(s.id);
    try {
      const r = await withSudo(connId, (pw) => WebService.SetEnabled(connId, s.engine, s.file, !s.enabled, pw));
      toast(t(s.enabled ? "web.disabledToast" : "web.enabledToast", { name: s.name }) + (r.notRunning ? " " + t("web.notRunningNote") : ""), "success");
      props.onChanged();
    } catch (e) {
      toast(`${s.name}: ${errMsg(e)}`, "error");
    } finally {
      setBusy(null);
    }
  };

  const remove = async (s: Site) => {
    const ok = await confirmDanger({
      serverId: connId,
      title: t("web.deleteQ", { name: s.name }),
      message: (
        <>
          <div className="mono">{s.file}</div>
          <div style={{ marginTop: 6 }}>{t("web.deleteMsg")}</div>
        </>
      ),
      confirmText: t("common.delete"),
    });
    if (!ok) return;
    setBusy(s.id);
    try {
      await withSudo(connId, (pw) => WebService.DeleteSite(connId, s.engine, s.file, pw));
      toast(t("web.deletedToast", { name: s.name }), "success");
      props.onChanged();
    } catch (e) {
      toast(`${s.name}: ${errMsg(e)}`, "error");
    } finally {
      setBusy(null);
    }
  };

  const issue = async (s: Site) => {
    const info = await runJob(t("web.issueTitle", { name: s.name }), () => withSudo(connId, (pw) => WebService.IssueCertificate(connId, s.file, pw)));
    if (info) props.onChanged();
  };

  const newSite = (eng?: string) => setForm({ engine: eng ?? (editable.includes("nginx") ? "nginx" : editable[0]) });

  return (
    <>
      <EngineStrip engines={engines} status={status} onShowTest={(e) => setTestOut({ title: t("web.testOutput", { engine: engineLabel(e.name) }), text: e.testOutput })} />

      {engines.length === 0 ? (
        <Empty icon={<ServerIcon size={28} />} title={t("web.noEngine")} text={t("web.noEngineHint")} />
      ) : (
        <Section
          title={t("web.sites")}
          icon={<Globe size={14} />}
          actions={
            canEdit && editable.length > 0 ? (
              <button className="btn primary sm" onClick={() => newSite()}>
                <Plus size={13} /> {t("web.newSite")}
              </button>
            ) : null
          }
        >
          <div className="toolbar">
            <input className="input" placeholder={t("web.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
            {editable.length > 1 && (
              <div className="segmented" style={{ width: 240 }}>
                {(["all", "nginx", "caddy"] as const).map((k) => (
                  <button key={k} className={engine === k ? "on" : ""} onClick={() => setEngine(k)}>
                    {k === "all" ? t("web.all") : engineLabel(k)}
                  </button>
                ))}
              </div>
            )}
            <div className="grow" />
            <span className="muted">{t("web.siteCount", { n: rows.length })}</span>
          </div>
          {sites.error && !sites.data ? (
            <ErrorBox error={sites.error} onRetry={sites.reload} />
          ) : !sites.data ? (
            <Loading />
          ) : rows.length === 0 ? (
            <Empty
              icon={<Globe size={28} />}
              title={list.length ? t("web.noMatch") : t("web.noSites")}
              text={list.length ? undefined : t("web.noSitesHint")}
              action={
                canEdit && editable.length > 0 && !list.length ? (
                  <button className="btn primary" onClick={() => newSite()}>
                    <Plus size={13} /> {t("web.newSite")}
                  </button>
                ) : undefined
              }
            />
          ) : (
            <div className="web-cards">
              {rows.map((s) => (
                <SiteCard
                  key={s.id}
                  site={s}
                  canEdit={canEdit}
                  busy={busy === s.id}
                  certs={certs}
                  onToggle={() => toggle(s)}
                  onEdit={() => (s.managed && s.spec ? setForm({ site: s, engine: s.engine }) : setRaw({ site: s, mode: "edit" }))}
                  onView={() => setRaw({ site: s, mode: "view" })}
                  onHistory={() => setRaw({ site: s, mode: "history" })}
                  onDelete={() => remove(s)}
                  onIssue={() => issue(s)}
                />
              ))}
            </div>
          )}
        </Section>
      )}

      {form && (
        <SiteForm
          connId={connId}
          site={form.site}
          engine={form.engine}
          engines={editable}
          onEngine={(e) => setForm({ engine: e })}
          onClose={() => setForm(null)}
          onApplied={(res, pending, name) => {
            setForm(null);
            props.onChanged();
            if (pending && res.id && (form.site?.engine ?? form.engine) === "nginx") {
              confirmDialog(t("web.issueNowQ"), t("web.issueNowMsg"), t("web.issueNow")).then((ok) => {
                if (ok)
                  runJob(t("web.issueTitle", { name }), () => withSudo(connId, (pw) => WebService.IssueCertificate(connId, res.id, pw))).then(
                    (i) => i && props.onChanged(),
                  );
              });
            }
          }}
        />
      )}
      {raw && <RawEditor connId={connId} site={raw.site} mode={raw.mode} canEdit={canEdit} onClose={() => setRaw(null)} onSaved={props.onChanged} />}
      {testOut && (
        <Modal title={testOut.title} size="wide" onClose={() => setTestOut(null)} footer={<button className="btn" onClick={() => setTestOut(null)}>{t("common.close")}</button>}>
          <div className="log-view">{testOut.text || "—"}</div>
        </Modal>
      )}
    </>
  );
}

function EngineStrip({ engines, status, onShowTest }: { engines: EngineInfo[]; status: WebStatus; onShowTest: (e: EngineInfo) => void }) {
  const t = useT();
  if (engines.length === 0) return null;
  const running = engines.filter((e) => e.running && (e.name === "nginx" || e.name === "caddy" || e.name === "apache"));
  return (
    <>
      {running.length > 1 && (
        <div className="hint-box web-warn">
          <AlertTriangle size={14} /> {t("web.portConflict", { list: running.map((e) => engineLabel(e.name)).join(", ") })}
        </div>
      )}
      <div className="web-engines">
        {engines.map((e) => (
          <div key={e.name} className={`web-engine ${e.running ? "" : "stopped"}`}>
            <div className="web-engine-head">
              <ServerIcon size={14} />
              <b>{engineLabel(e.name)}</b>
              <span className="muted">{e.version}</span>
              <div className="grow" />
              <StateBadge tone={e.running ? "ok" : "muted"}>{e.running ? t("web.running") : t("web.stopped")}</StateBadge>
            </div>
            <div className="web-engine-body">
              {e.editable ? (
                <>
                  {e.testRun && (
                    <button className={`web-test ${e.testOk ? "ok" : "err"}`} onClick={() => onShowTest(e)} title={t("web.showTest")}>
                      {e.testOk ? <CheckCircle2 size={13} /> : <XCircle size={13} />}
                      {e.testOk ? t("web.configOk") : t("web.configBad")}
                    </button>
                  )}
                  {e.name === "nginx" && e.layout && <span className="chip">{e.layout === "debian" ? "sites-available / sites-enabled" : "conf.d"}</span>}
                  {e.name === "caddy" && !e.running && <span className="muted web-small">{t("web.caddyStoppedHint")}</span>}
                  {e.name === "caddy" && e.running && !e.importsSites && <span className="muted web-small">{t("web.caddyImportHint")}</span>}
                </>
              ) : (
                <span className="muted web-small">
                  {e.docker ? t("web.traefikDocker") + " " : ""}
                  {t("web.readOnlyEngine", { name: engineLabel(e.name) })}
                </span>
              )}
            </div>
          </div>
        ))}
        {!status.certbot && (
          <div className="web-engine stopped">
            <div className="web-engine-head">
              <Lock size={14} />
              <b>certbot</b>
              <div className="grow" />
              <StateBadge tone="warn">{t("web.notInstalled")}</StateBadge>
            </div>
            <div className="web-engine-body muted web-small">{t("web.certbotMissingHint")}</div>
          </div>
        )}
      </div>
    </>
  );
}

function SiteCard(props: {
  site: Site;
  canEdit: boolean;
  busy: boolean;
  certs: Map<string, import("../../../bindings/server-manager/services/web/models").Certificate>;
  onToggle: () => void;
  onEdit: () => void;
  onView: () => void;
  onHistory: () => void;
  onDelete: () => void;
  onIssue: () => void;
}) {
  const t = useT();
  const s = props.site;
  const domains = s.domains ?? [];
  const aliases = domains.slice(1);
  const cert = (s.certs ?? []).map((p) => props.certs.get(p)).find(Boolean);
  const KindIcon = s.kind === "proxy" ? ArrowRightLeft : s.kind === "static" ? Folder : s.kind === "redirect" ? CornerUpRight : FileCode2;
  return (
    <div className={`web-card ${s.enabled ? "" : "off"}`}>
      <div className="web-card-head">
        <Globe size={15} className="web-card-icon" />
        <div className="web-card-title" title={s.name}>
          {s.name}
        </div>
        <label className={`web-switch ${props.canEdit && s.canToggle ? "" : "disabled"}`} title={s.canToggle ? (s.enabled ? t("web.disable") : t("web.enable")) : t("web.cannotToggle")}>
          <input type="checkbox" checked={s.enabled} disabled={!props.canEdit || !s.canToggle || props.busy} onChange={props.onToggle} />
          <span />
        </label>
      </div>
      <div className="web-card-badges">
        <span className="badge">{engineLabel(s.engine)}</span>
        {s.managed ? <span className="badge ok">{t("web.managed")}</span> : <span className="badge">{t("web.manual")}</span>}
        {s.default && <span className="badge warn">default_server</span>}
        {s.http2 && <span className="badge">HTTP/2</span>}
        {!s.enabled && <span className="badge">{t("web.disabled")}</span>}
      </div>
      {aliases.length > 0 && (
        <div className="chips web-aliases">
          {aliases.slice(0, 4).map((d) => (
            <span key={d} className="chip mono">
              {d}
            </span>
          ))}
          {aliases.length > 4 && <span className="chip">+{aliases.length - 4}</span>}
        </div>
      )}
      <div className="web-card-line" title={s.target}>
        <KindIcon size={13} />
        <span className="muted">{t(kindKey(s.kind))}</span>
        <span className="mono web-ellipsis">{s.target || "—"}</span>
      </div>
      <div className="web-card-line">
        {s.pendingCert ? (
          <>
            <LockOpen size={13} color="var(--warn)" />
            <StateBadge tone="warn">{t("web.pendingCert")}</StateBadge>
            {props.canEdit && s.enabled && (
              <button className="btn sm ghost" onClick={props.onIssue}>
                {t("web.issueNow")}
              </button>
            )}
          </>
        ) : s.https ? (
          <>
            <Lock size={13} color="var(--ok)" />
            {cert ? (
              <>
                <StateBadge tone={certTone(cert.status)}>{cert.notAfter ? daysText(cert.daysLeft, t) : t(certStatusKey(cert.status))}</StateBadge>
                <span className="muted web-ellipsis" title={cert.path}>
                  {cert.issuer || cert.name}
                  {cert.notAfter ? ` · ${formatDate(cert.notAfter)}` : ""}
                </span>
              </>
            ) : (
              <span className="muted">HTTPS</span>
            )}
          </>
        ) : (
          <>
            <LockOpen size={13} className="muted" />
            <span className="muted">{t("web.httpOnly")}</span>
          </>
        )}
      </div>
      <div className="web-card-foot">
        <span className="muted mono web-ellipsis" title={s.file}>
          {s.file}
        </span>
        <div className="grow" />
        {props.busy ? (
          <span className="spinner" />
        ) : (
          <>
            <button className="icon-btn" title={t("web.viewConfig")} onClick={props.onView}>
              <Eye size={13} />
            </button>
            <button className="icon-btn" title={t("web.history")} onClick={props.onHistory}>
              <HistoryIcon size={13} />
            </button>
            {props.canEdit && (
              <>
                <button className="icon-btn" title={s.managed ? t("web.edit") : t("web.editRaw")} onClick={props.onEdit}>
                  <Pencil size={13} />
                </button>
                {s.canToggle && (
                  <button className="icon-btn" title={t("common.delete")} onClick={props.onDelete}>
                    <Trash2 size={13} />
                  </button>
                )}
              </>
            )}
          </>
        )}
      </div>
    </div>
  );
}
