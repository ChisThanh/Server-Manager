import { useMemo, useState } from "react";
import { AlertTriangle, CalendarClock, CheckCircle2, Download, FlaskConical, Radar, RefreshCw, ShieldCheck, Trash2, Upload, XCircle, Ban } from "lucide-react";
import { WebService } from "../../../bindings/server-manager/services/web";
import type { CertInspect, CertOverview, Certificate, LiveCert, Site, WebStatus } from "../../../bindings/server-manager/services/web/models";
import { Empty, ErrorBox, KV, Loading, Section } from "../../ui/Page";
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
import { useT, type Key } from "../../i18n";
import { certStatusKey, certTone, daysText, sourceKey } from "./util";

type SortKey = "name" | "expiry" | "source";

export function SSL(props: { connId: string; status: WebStatus; overview: Remote<CertOverview>; sites: Site[]; onChanged: () => void }) {
  const { connId, overview } = props;
  const t = useT();
  const canEdit = useCan(connId, "web");
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<{ key: SortKey; asc: boolean }>({ key: "expiry", asc: true });
  const [live, setLive] = useState<Record<string, LiveCert>>({});
  const [checking, setChecking] = useState(false);
  const [install, setInstall] = useState<{ name: string; replace: boolean } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const data = overview.data;
  const certs = data?.certificates ?? [];
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = certs.filter((c) => !needle || [c.name, c.path, c.issuer, ...(c.domains ?? [])].some((x) => x.toLowerCase().includes(needle)));
    const dir = sort.asc ? 1 : -1;
    return [...list].sort((a, b) => {
      switch (sort.key) {
        case "name":
          return a.name.localeCompare(b.name) * dir;
        case "source":
          return a.source.localeCompare(b.source) * dir;
        default:
          return ((a.notAfter || Number.MAX_SAFE_INTEGER) - (b.notAfter || Number.MAX_SAFE_INTEGER)) * dir;
      }
    });
  }, [certs, q, sort]);

  const siteName = (file: string) => props.sites.find((s) => s.file === file)?.name ?? file.split("/").pop() ?? file;

  const checkLive = async () => {
    const domains = Array.from(new Set(certs.flatMap((c) => c.domains ?? []).filter((d) => d && !d.startsWith("*.") && !/^[\d.]+$/.test(d))));
    if (!domains.length) return;
    setChecking(true);
    try {
      const res = (await WebService.LiveCheck(domains)) ?? [];
      const m: Record<string, LiveCert> = {};
      for (const r of res) m[r.domain] = r;
      setLive(m);
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setChecking(false);
    }
  };

  const job = async (title: string, start: (pw: string) => Promise<string>) => {
    const info = await runJob(title, () => withSudo(connId, start));
    if (info) props.onChanged();
  };

  const renew = (c: Certificate, force: boolean) => job(t(force ? "web.forceRenewTitle" : "web.renewTitle", { name: c.name }), (pw) => WebService.RenewCertificate(connId, c.name, force, pw));
  const dryRun = (name: string) => job(t("web.dryRunTitle"), (pw) => WebService.TestRenewal(connId, name, pw));

  const revoke = async (c: Certificate) => {
    const ok = await confirmDanger({ serverId: connId, title: t("web.revokeQ", { name: c.name }), message: t("web.revokeMsg"), confirmText: t("web.revoke") });
    if (ok) job(t("web.revokeTitle", { name: c.name }), (pw) => WebService.RevokeCertificate(connId, c.name, pw));
  };

  const remove = async (c: Certificate) => {
    const ok = await confirmDanger({
      serverId: connId,
      title: t("web.deleteCertQ", { name: c.name }),
      message: c.source === "custom" ? t("web.deleteCustomMsg") : t("web.deleteCertMsg"),
      confirmText: t("common.delete"),
    });
    if (!ok) return;
    setBusy(c.path);
    try {
      await withSudo(connId, (pw) => (c.source === "custom" ? WebService.DeleteCustomCertificate(connId, c.name, pw) : WebService.DeleteCertificate(connId, c.name, pw)));
      toast(t("web.certDeleted", { name: c.name }), "success");
      props.onChanged();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
    }
  };

  const installCertbot = async () => {
    if (!(await confirmDialog(t("web.installCertbotQ"), t("web.installCertbotMsg"), t("web.install")))) return;
    job(t("web.installCertbot"), (pw) => WebService.InstallCertbot(connId, pw));
  };

  const th = (key: SortKey, label: string, cls = "") => (
    <th className={`sortable ${cls}`} onClick={() => setSort((s) => ({ key, asc: s.key === key ? !s.asc : true }))}>
      {label}
      {sort.key === key ? (sort.asc ? " ▲" : " ▼") : ""}
    </th>
  );

  if (overview.error && !data) return <ErrorBox error={overview.error} onRetry={overview.reload} />;
  if (!data) return <Loading />;

  const renewal = data.renewal;
  return (
    <>
      <div className="dash-cols">
        <div className="panel-box">
          <div className="box-title">
            <CalendarClock size={14} /> {t("web.autoRenew")}
            <div className="grow" />
            {data.certbot ? (
              <StateBadge tone={renewal.automatic ? "ok" : "warn"}>{renewal.automatic ? t("web.autoRenewOn") : t("web.autoRenewOff")}</StateBadge>
            ) : (
              <StateBadge tone="muted">{t("web.notInstalled")}</StateBadge>
            )}
          </div>
          {!data.certbot ? (
            <div className="muted web-small">
              {t("web.certbotMissingHint")}
              {canEdit && data.pkgManager && (
                <div style={{ marginTop: 8 }}>
                  <button className="btn sm" onClick={installCertbot}>
                    <Download size={12} /> {t("web.installCertbot")}
                  </button>
                </div>
              )}
            </div>
          ) : (
            <div className="list-rows">
              {(renewal.timers ?? []).map((tm) => (
                <div className="list-row" key={tm.unit}>
                  <span className="mono">{tm.unit}</span>
                  <StateBadge tone={tm.active ? "ok" : "muted"}>{tm.active ? t("web.active") : t("web.inactive")}</StateBadge>
                  <div className="grow" />
                  <span className="muted web-small">{tm.next ? t("web.nextRun", { when: formatDate(tm.next) }) : tm.nextText || "—"}</span>
                </div>
              ))}
              {(renewal.cron ?? []).map((c) => (
                <div className="list-row" key={c}>
                  <span className="badge">cron</span>
                  <span className="mono web-ellipsis web-small" title={c}>
                    {c}
                  </span>
                </div>
              ))}
              {(renewal.timers ?? []).length === 0 && (renewal.cron ?? []).length === 0 && <div className="muted web-small">{t("web.noAutoRenew")}</div>}
            </div>
          )}
        </div>
        <div className="panel-box">
          <div className="box-title">
            <ShieldCheck size={14} /> {t("web.certSummary")}
          </div>
          <div className="web-summary">
            {(["ok", "warn", "err", "expired"] as const).map((st) => (
              <div key={st} className="web-summary-item">
                <StateBadge tone={certTone(st)}>{t(certStatusKey(st))}</StateBadge>
                <b>{certs.filter((c) => c.status === st).length}</b>
              </div>
            ))}
          </div>
          {data.certbot && canEdit && (
            <button className="btn sm" style={{ marginTop: 10 }} onClick={() => dryRun("")}>
              <FlaskConical size={12} /> {t("web.dryRun")}
            </button>
          )}
        </div>
      </div>

      <Section
        title={t("web.certificates")}
        icon={<ShieldCheck size={14} />}
        actions={
          <>
            <button className="btn sm" onClick={checkLive} disabled={checking || certs.length === 0} title={t("web.checkLiveHint")}>
              {checking ? <span className="spinner" /> : <Radar size={12} />} {t("web.checkLive")}
            </button>
            {canEdit && (
              <button className="btn sm primary" onClick={() => setInstall({ name: "", replace: false })}>
                <Upload size={12} /> {t("web.installCert")}
              </button>
            )}
          </>
        }
      >
        <div className="toolbar">
          <input className="input" placeholder={t("web.filterCerts")} value={q} onChange={(e) => setQ(e.target.value)} />
          <div className="grow" />
          <span className="muted">{t("web.certCount", { n: rows.length })}</span>
        </div>
        {rows.length === 0 ? (
          <Empty icon={<ShieldCheck size={28} />} title={certs.length ? t("web.noMatch") : t("web.noCerts")} text={certs.length ? undefined : t("web.noCertsHint")} />
        ) : (
          <div className="table-wrap">
            <table className="grid web-cert-table">
              <thead>
                <tr>
                  {th("name", t("web.c.name"))}
                  <th>{t("web.c.domains")}</th>
                  {th("source", t("web.c.source"))}
                  <th>{t("web.c.issuer")}</th>
                  {th("expiry", t("web.c.expires"))}
                  <th>{t("web.c.live")}</th>
                  <th>{t("web.c.usedBy")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {rows.map((c) => (
                  <tr key={c.path}>
                    <td>
                      <div className="mono">{c.name}</div>
                      <div className="muted web-small mono web-ellipsis" title={c.path}>
                        {c.path}
                      </div>
                    </td>
                    <td style={{ whiteSpace: "normal" }}>
                      <div className="chips">
                        {(c.domains ?? []).slice(0, 4).map((d) => (
                          <span key={d} className="chip mono">
                            {d}
                          </span>
                        ))}
                        {(c.domains ?? []).length > 4 && <span className="chip">+{(c.domains ?? []).length - 4}</span>}
                      </div>
                    </td>
                    <td>
                      <span className="badge">{t(sourceKey(c.source))}</span>
                      {c.staging && <span className="badge warn" style={{ marginLeft: 4 }}>{t("web.staging")}</span>}
                    </td>
                    <td className="web-small">{c.issuer || "—"}</td>
                    <td>
                      {c.error ? (
                        <StateBadge tone="err">{t(c.error === "missing" ? "web.cert.missing" : "web.cert.unreadable")}</StateBadge>
                      ) : (
                        <>
                          <StateBadge tone={certTone(c.status)}>{c.notAfter ? daysText(c.daysLeft, t) : t(certStatusKey(c.status))}</StateBadge>
                          {c.notAfter > 0 && <div className="muted web-small">{formatDate(c.notAfter)}</div>}
                        </>
                      )}
                    </td>
                    <td>
                      <LiveCell cert={c} live={live} />
                    </td>
                    <td className="web-small" style={{ whiteSpace: "normal" }}>
                      {(c.usedBy ?? []).length === 0 ? <span className="muted">—</span> : (c.usedBy ?? []).map((f) => <div key={f}>{siteName(f)}</div>)}
                    </td>
                    <td className="web-actions">
                      {busy === c.path ? (
                        <span className="spinner" />
                      ) : (
                        canEdit && (
                          <CertActions
                            cert={c}
                            onRenew={() => renew(c, false)}
                            onForce={() => renew(c, true)}
                            onDryRun={() => dryRun(c.name)}
                            onRevoke={() => revoke(c)}
                            onDelete={() => remove(c)}
                            onReplace={() => setInstall({ name: c.name, replace: true })}
                          />
                        )
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
      {install && (
        <InstallCertModal
          connId={connId}
          initialName={install.name}
          replace={install.replace}
          onClose={() => setInstall(null)}
          onInstalled={() => {
            setInstall(null);
            props.onChanged();
          }}
        />
      )}
    </>
  );
}

function CertActions(props: { cert: Certificate; onRenew: () => void; onForce: () => void; onDryRun: () => void; onRevoke: () => void; onDelete: () => void; onReplace: () => void }) {
  const t = useT();
  const c = props.cert;
  if (c.source === "certbot") {
    return (
      <div className="web-btns">
        <button className="icon-btn" title={t("web.renew")} onClick={props.onRenew}>
          <RefreshCw size={13} />
        </button>
        <button className="icon-btn" title={t("web.forceRenew")} onClick={props.onForce}>
          <RefreshCw size={13} color="var(--warn)" />
        </button>
        <button className="icon-btn" title={t("web.dryRunOne")} onClick={props.onDryRun}>
          <FlaskConical size={13} />
        </button>
        <button className="icon-btn" title={t("web.revoke")} onClick={props.onRevoke}>
          <Ban size={13} />
        </button>
        <button className="icon-btn" title={t("common.delete")} onClick={props.onDelete}>
          <Trash2 size={13} />
        </button>
      </div>
    );
  }
  if (c.source === "custom" && c.path.startsWith("/etc/ssl/server-manager/")) {
    return (
      <div className="web-btns">
        <button className="icon-btn" title={t("web.replaceCert")} onClick={props.onReplace}>
          <Upload size={13} />
        </button>
        <button className="icon-btn" title={t("common.delete")} onClick={props.onDelete}>
          <Trash2 size={13} />
        </button>
      </div>
    );
  }
  return null;
}

function LiveCell({ cert, live }: { cert: Certificate; live: Record<string, LiveCert> }) {
  const t = useT();
  const results = (cert.domains ?? []).map((d) => live[d]).filter(Boolean) as LiveCert[];
  if (results.length === 0) return <span className="muted">—</span>;
  const bad = results.filter((r) => !r.ok);
  const title = results
    .map((r) => `${r.domain}: ${r.error ? r.error : r.verifyError ? r.verifyError : `${r.issuer} · ${formatDate(r.notAfter)}`}`)
    .join("\n");
  if (bad.length === 0) {
    const mismatch = results.some((r) => cert.notAfter && Math.abs(r.notAfter - cert.notAfter) > 86400);
    return (
      <span className="web-live ok" title={title}>
        <CheckCircle2 size={13} /> {mismatch ? t("web.liveOld") : t("web.liveOk")}
      </span>
    );
  }
  const unreachable = bad.every((r) => r.error);
  return (
    <span className={`web-live ${unreachable ? "muted" : "err"}`} title={title}>
      {unreachable ? <AlertTriangle size={13} /> : <XCircle size={13} />} {unreachable ? t("web.liveUnreachable") : t("web.liveInvalid")}
    </span>
  );
}

function InstallCertModal(props: { connId: string; initialName: string; replace: boolean; onClose: () => void; onInstalled: () => void }) {
  const t = useT();
  const [name, setName] = useState(props.initialName);
  const [chain, setChain] = useState("");
  const [key, setKey] = useState("");
  const [info, setInfo] = useState<CertInspect | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const inspect = async () => {
    setErr("");
    setInfo(null);
    try {
      const i = await WebService.InspectCertificate(chain, key);
      setInfo(i);
      if (!name && i.subject) setName(i.subject.replace(/^\*\./, "wildcard.").toLowerCase().replace(/[^a-z0-9._-]/g, "-"));
    } catch (e) {
      setErr(errMsg(e));
    }
  };

  const doInstall = async () => {
    setBusy(true);
    setErr("");
    try {
      await withSudo(props.connId, (pw) => WebService.InstallCertificate(props.connId, name.trim(), chain, key, props.replace, pw));
      toast(t("web.certInstalled", { name }), "success");
      setKey("");
      props.onInstalled();
    } catch (e) {
      setErr(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const warnKey = (w: string): Key =>
    w === "selfSigned" ? "web.w.selfSigned" : w === "noIntermediate" ? "web.w.noIntermediate" : w === "notYetValid" ? "web.w.notYetValid" : "web.w.expired";

  return (
    <Modal
      title={props.replace ? t("web.replaceCertTitle", { name: props.initialName }) : t("web.installCert")}
      size="wide"
      onClose={() => {
        setKey("");
        props.onClose();
      }}
      footer={
        <>
          <button className="btn left" onClick={inspect} disabled={!chain.trim() || busy}>
            <ShieldCheck size={13} /> {t("web.validate")}
          </button>
          <button className="btn" onClick={props.onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={doInstall} disabled={!info || !name.trim() || busy}>
            {busy ? <span className="spinner" /> : null} {t("web.install")}
          </button>
        </>
      }
    >
      <div className="hint-box">{t("web.installCertHint")}</div>
      <div className="field">
        <label>{t("web.certName")}</label>
        <input className="input mono" value={name} disabled={props.replace} placeholder="example.com" onChange={(e) => setName(e.target.value)} />
        <div className="hint">/etc/ssl/server-manager/{name || "<name>"}/fullchain.pem · privkey.pem</div>
      </div>
      <div className="row">
        <div className="field">
          <label>{t("web.pemChain")}</label>
          <textarea
            className="input mono web-pem"
            spellCheck={false}
            value={chain}
            placeholder="-----BEGIN CERTIFICATE-----"
            onChange={(e) => {
              setChain(e.target.value);
              setInfo(null);
            }}
          />
        </div>
        <div className="field">
          <label>{t("web.pemKey")}</label>
          <textarea
            className="input mono web-pem"
            spellCheck={false}
            autoComplete="off"
            value={key}
            placeholder="-----BEGIN PRIVATE KEY-----"
            onChange={(e) => {
              setKey(e.target.value);
              setInfo(null);
            }}
          />
        </div>
      </div>
      {err && (
        <div className="web-alert err">
          <AlertTriangle size={14} /> {err}
        </div>
      )}
      {info && (
        <div className="panel-box">
          <div className="box-title">
            <CheckCircle2 size={14} color="var(--ok)" /> {t("web.certValid")}
            <div className="grow" />
            <StateBadge tone={certTone(info.status)}>{daysText(info.daysLeft, t)}</StateBadge>
          </div>
          <KV
            items={[
              [t("web.c.subject"), <span className="mono">{info.subject || "—"}</span>],
              [t("web.c.domains"), <span className="mono">{(info.domains ?? []).join(", ") || "—"}</span>],
              [t("web.c.issuer"), info.issuer],
              [t("web.c.validity"), `${formatDate(info.notBefore)} → ${formatDate(info.notAfter)}`],
              [t("web.c.keyType"), info.keyType],
              [t("web.c.chain"), String(info.chainLen)],
              ["SHA-256", <span className="mono web-small web-break">{info.fingerprint}</span>],
            ]}
          />
          {(info.warnings ?? []).length > 0 && (
            <div className="web-alert warn" style={{ marginTop: 10 }}>
              <AlertTriangle size={14} />
              <div>
                {(info.warnings ?? []).map((w) => (
                  <div key={w}>{t(warnKey(w))}</div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}
    </Modal>
  );
}
