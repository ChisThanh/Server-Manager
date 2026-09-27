import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { AlertTriangle, Info, Plus, X } from "lucide-react";
import { WebService } from "../../../bindings/server-manager/services/web";
import type { ApplyResult, AuthUser, GenContext, Header, Preview, Site, SiteSpec } from "../../../bindings/server-manager/services/web/models";
import { CodeEditor } from "../../ui/CodeEditor";
import { runJob } from "../../ui/jobs";
import { withSudo } from "../../store/sudo";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { Modal } from "../Overlays";
import { useT, type Key } from "../../i18n";
import { completeSpec, engineLabel, isTestFailure, newSpec } from "./util";
import { langFor } from "./langs";
import { Select } from "../../ui/Select";

type SetSpec = (patch: Partial<SiteSpec>) => void;

export function SiteForm(props: {
  connId: string;
  site?: Site;
  engine: string;
  engines: string[];
  onEngine: (e: string) => void;
  onClose: () => void;
  onApplied: (res: ApplyResult, pendingCert: boolean, name: string) => void;
}) {
  const { connId, site } = props;
  const t = useT();
  const editing = !!site;
  const [gc, setGc] = useState<GenContext | null>(null);
  const [gcErr, setGcErr] = useState("");
  const [spec, setSpecState] = useState<SiteSpec | null>(null);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [previewErr, setPreviewErr] = useState("");
  const [applying, setApplying] = useState(false);
  const [applyErr, setApplyErr] = useState<{ msg: string; detail: string } | null>(null);
  const [showExtras, setShowExtras] = useState(false);
  // Validation messages appear once the user has changed something.
  const [touched, setTouched] = useState(editing);

  // Server facts for this engine (layout, nginx version, certificates…).
  useEffect(() => {
    let live = true;
    setGc(null);
    setGcErr("");
    withSudo(connId, (pw) => WebService.FormContext(connId, props.engine, pw))
      .then((g) => {
        if (!live) return;
        setGc(g);
        setSpecState((prev) => {
          if (prev && !editing) return { ...prev, engine: props.engine };
          return site?.spec ? completeSpec(site.spec, g.email) : newSpec(props.engine, g.email);
        });
      })
      .catch((e) => live && setGcErr(errMsg(e)));
    return () => {
      live = false;
    };
  }, [connId, props.engine]);

  const setSpec: SetSpec = (patch) => {
    setTouched(true);
    setSpecState((s) => (s ? { ...s, ...patch } : s));
  };

  // Live preview (computed by the backend generator, no server access).
  const seq = useRef(0);
  useEffect(() => {
    if (!gc || !spec) return;
    const my = ++seq.current;
    const h = setTimeout(() => {
      WebService.Preview(gc, site?.id ?? "", clean(spec))
        .then((p) => {
          if (my !== seq.current) return;
          setPreview(p);
          setPreviewErr("");
        })
        .catch((e) => my === seq.current && setPreviewErr(errMsg(e)));
    }, 250);
    return () => clearTimeout(h);
  }, [gc, spec]);

  const apply = async () => {
    if (!spec) return;
    setApplying(true);
    setApplyErr(null);
    try {
      const res = await withSudo(connId, (pw) => WebService.Apply(connId, site?.id ?? "", clean(spec), pw));
      const name = spec.domains?.[0] ?? "";
      const notes: string[] = [];
      if (res.notRunning) notes.push(t("web.notRunningNote"));
      if ((res.warnings ?? []).includes("caddyImportAdded")) notes.push(t("web.caddyImportAdded"));
      toast(t(editing ? "web.updatedToast" : "web.createdToast", { name }) + (notes.length ? " " + notes.join(" ") : ""), "success");
      props.onApplied(res, res.pendingCert, name);
    } catch (e) {
      if (isTestFailure(e)) {
        const ae = (e as { cause?: { detail?: string } }).cause;
        setApplyErr({ msg: t("web.testFailedMsg"), detail: ae?.detail ?? "" });
      } else {
        setApplyErr({ msg: errMsg(e), detail: "" });
      }
    } finally {
      setApplying(false);
    }
  };

  const installCertbot = async () => {
    if (!(await confirmDialog(t("web.installCertbotQ"), t("web.installCertbotMsg"), t("web.install")))) return;
    const info = await runJob(t("web.installCertbot"), () => withSudo(connId, (pw) => WebService.InstallCertbot(connId, pw)));
    if (info?.state === "done") setGc((g) => (g ? { ...g, certbot: true } : g));
  };

  const title = editing ? t("web.editSite", { name: site?.name ?? "" }) : t("web.newSite");
  const caddy = props.engine === "caddy";

  return (
    <Modal
      title={title}
      size="xwide"
      onClose={props.onClose}
      footer={
        <>
          {preview && <span className="muted left mono web-small">{site?.file || preview.file || ""}</span>}
          <button className="btn" onClick={props.onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={apply} disabled={!spec || !gc || applying || !!previewErr}>
            {applying ? <span className="spinner" /> : null} {editing ? t("web.saveApply") : t("web.createApply")}
          </button>
        </>
      }
    >
      {gcErr ? (
        <div className="err">{gcErr}</div>
      ) : !spec || !gc ? (
        <div className="ui-center">
          <span className="spinner lg" />
        </div>
      ) : (
        <div className="web-form">
          <div className="web-form-main">
            {!editing && props.engines.length > 1 && (
              <FormSection title={t("web.f.engine")}>
                <div className="segmented" style={{ maxWidth: 260 }}>
                  {props.engines.map((e) => (
                    <button key={e} className={props.engine === e ? "on" : ""} onClick={() => props.onEngine(e)}>
                      {engineLabel(e)}
                    </button>
                  ))}
                </div>
              </FormSection>
            )}
            <DomainsSection spec={spec} setSpec={setSpec} />
            <TypeSection spec={spec} setSpec={setSpec} caddy={caddy} />
            <SSLSection spec={spec} setSpec={setSpec} gc={gc} caddy={caddy} onInstallCertbot={installCertbot} />
            <SecuritySection spec={spec} setSpec={setSpec} caddy={caddy} editing={editing} />
            <FormSection title={t("web.f.responseHeaders")}>
              <HeaderList list={spec.responseHeaders ?? []} onChange={(l) => setSpec({ responseHeaders: l })} />
            </FormSection>
            <FormSection title={t("web.f.logs")}>
              <div className="form-grid">
                <Field label={t("web.f.accessLog")} hint={t("web.f.logHint")}>
                  <input className="input mono" value={spec.accessLog} placeholder={`${gc.logDir}/…access.log`} onChange={(e) => setSpec({ accessLog: e.target.value })} />
                </Field>
                {!caddy && (
                  <Field label={t("web.f.errorLog")}>
                    <input className="input mono" value={spec.errorLog} placeholder={`${gc.logDir}/…error.log`} onChange={(e) => setSpec({ errorLog: e.target.value })} />
                  </Field>
                )}
              </div>
            </FormSection>
            <FormSection title={t("web.f.extra")}>
              <div className="hint">{t(caddy ? "web.f.extraHintCaddy" : "web.f.extraHint")}</div>
              <textarea className="input mono web-textarea" rows={5} spellCheck={false} value={spec.extra} onChange={(e) => setSpec({ extra: e.target.value })} />
            </FormSection>
          </div>
          <div className="web-form-side">
            <div className="web-preview-head">
              <b>{t("web.preview")}</b>
              <div className="grow" />
              {(preview?.extras?.length ?? 0) > 0 && (
                <label className="check web-small">
                  <input type="checkbox" checked={showExtras} onChange={(e) => setShowExtras(e.target.checked)} />
                  {t("web.showExtras", { n: preview?.extras?.length ?? 0 })}
                </label>
              )}
            </div>
            {previewErr && touched && (
              <div className="web-alert err">
                <AlertTriangle size={14} /> {previewErr}
              </div>
            )}
            {preview?.pendingCert && (
              <div className="web-alert warn">
                <Info size={14} /> {t("web.pendingCertPreview")}
              </div>
            )}
            {applyErr && (
              <div className="web-alert err">
                <AlertTriangle size={14} />
                <div style={{ minWidth: 0 }}>
                  <div>{applyErr.msg}</div>
                  {applyErr.detail && <pre className="log-view err web-testout">{applyErr.detail}</pre>}
                </div>
              </div>
            )}
            <CodeEditor
              value={
                showExtras && preview
                  ? [preview.content, ...(preview.extras ?? []).map((x) => `# ─── ${x.path}\n${x.content}`)].join("\n")
                  : (preview?.content ?? "")
              }
              language={langFor(props.engine)}
              readOnly
              height="calc(100% - 40px)"
            />
          </div>
        </div>
      )}
    </Modal>
  );
}

/** Drops empty rows and trims before sending the spec. */
function clean(sp: SiteSpec): SiteSpec {
  const out: SiteSpec = { ...sp };
  out.domains = (sp.domains ?? []).map((d) => d.trim()).filter(Boolean);
  out.upstreams = (sp.upstreams ?? []).map((d) => d.trim()).filter(Boolean);
  out.requestHeaders = (sp.requestHeaders ?? []).filter((h) => h.name.trim() || h.value.trim());
  out.responseHeaders = (sp.responseHeaders ?? []).filter((h) => h.name.trim() || h.value.trim());
  out.authUsers = (sp.authUsers ?? []).filter((u) => u.user.trim() || u.password);
  return out;
}

function FormSection({ title, children, right }: { title: ReactNode; children: ReactNode; right?: ReactNode }) {
  return (
    <div className="web-fsec">
      <div className="web-fsec-title">
        <span>{title}</span>
        <div className="grow" />
        {right}
      </div>
      {children}
    </div>
  );
}

function Field({ label, hint, children }: { label: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <div className="field">
      <label>{label}</label>
      {children}
      {hint && <div className="hint">{hint}</div>}
    </div>
  );
}

function Check({ checked, onChange, children, disabled }: { checked: boolean; onChange: (v: boolean) => void; children: ReactNode; disabled?: boolean }) {
  return (
    <label className="check web-check">
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      {children}
    </label>
  );
}

function num(v: string): number {
  const n = parseInt(v, 10);
  return Number.isFinite(n) && n >= 0 ? n : 0;
}

function DomainsSection({ spec, setSpec }: { spec: SiteSpec; setSpec: SetSpec }) {
  const t = useT();
  const domains = spec.domains ?? [""];
  const [alias, setAlias] = useState("");
  const aliases = domains.slice(1);
  const addAlias = () => {
    const parts = alias
      .split(/[\s,]+/)
      .map((x) => x.trim())
      .filter(Boolean)
      .filter((x) => !domains.includes(x));
    if (parts.length) setSpec({ domains: [domains[0] ?? "", ...aliases, ...parts] });
    setAlias("");
  };
  return (
    <FormSection title={t("web.f.domains")}>
      <Field label={t("web.f.primary")} hint={t("web.f.primaryHint")}>
        <input className="input mono" autoFocus value={domains[0] ?? ""} placeholder="app.example.com" onChange={(e) => setSpec({ domains: [e.target.value, ...aliases] })} />
      </Field>
      <Field label={t("web.f.aliases")}>
        <div className="row" style={{ gap: 6 }}>
          <input
            className="input mono"
            value={alias}
            placeholder="www.example.com"
            onChange={(e) => setAlias(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === ",") {
                e.preventDefault();
                addAlias();
              }
            }}
            onBlur={() => alias.trim() && addAlias()}
          />
          <button className="btn" style={{ flex: "none" }} onClick={addAlias} disabled={!alias.trim()}>
            <Plus size={13} />
          </button>
        </div>
        {aliases.length > 0 && (
          <div className="chips" style={{ marginTop: 6 }}>
            {aliases.map((a) => (
              <span key={a} className="chip mono">
                {a}
                <button onClick={() => setSpec({ domains: [domains[0] ?? "", ...aliases.filter((x) => x !== a)] })}>
                  <X size={11} />
                </button>
              </span>
            ))}
          </div>
        )}
      </Field>
    </FormSection>
  );
}

function TypeSection({ spec, setSpec, caddy }: { spec: SiteSpec; setSpec: SetSpec; caddy: boolean }) {
  const t = useT();
  const ups = spec.upstreams ?? [];
  return (
    <FormSection title={t("web.f.type")}>
      <div className="segmented" style={{ marginBottom: 12 }}>
        {(
          [
            ["proxy", "web.kind.proxy"],
            ["static", "web.kind.static"],
            ["redirect", "web.kind.redirect"],
          ] as [string, Key][]
        ).map(([k, l]) => (
          <button key={k} className={spec.type === k ? "on" : ""} onClick={() => setSpec({ type: k })}>
            {t(l)}
          </button>
        ))}
      </div>
      {spec.type === "proxy" && (
        <>
          <Field label={t("web.f.upstreams")} hint={t(caddy ? "web.f.upstreamHintCaddy" : "web.f.upstreamHint")}>
            {ups.map((u, i) => (
              <div className="row web-listrow" key={i}>
                <input className="input mono" value={u} placeholder="http://127.0.0.1:3000" onChange={(e) => setSpec({ upstreams: ups.map((x, j) => (j === i ? e.target.value : x)) })} />
                {ups.length > 1 && (
                  <button className="icon-btn" style={{ flex: "none" }} onClick={() => setSpec({ upstreams: ups.filter((_, j) => j !== i) })}>
                    <X size={13} />
                  </button>
                )}
              </div>
            ))}
            <button className="btn sm ghost" style={{ alignSelf: "flex-start" }} onClick={() => setSpec({ upstreams: [...ups, ""] })}>
              <Plus size={12} /> {t("web.f.addUpstream")}
            </button>
          </Field>
          <div className="form-grid">
            {ups.length > 1 && (
              <Field label={t("web.f.lb")}>
                <Select
                  value={spec.lbMethod}
                  onChange={(v) => setSpec({ lbMethod: v })}
                  options={[
                    { value: "", label: t("web.lb.rr") },
                    { value: "least_conn", label: t("web.lb.least") },
                    { value: "ip_hash", label: t("web.lb.iphash") },
                  ]}
                />
              </Field>
            )}
            <Field label={t("web.f.connectTimeout")} hint={t("web.f.secondsDefault")}>
              <input className="input" type="number" min={0} value={spec.connectTimeout || ""} onChange={(e) => setSpec({ connectTimeout: num(e.target.value) })} />
            </Field>
            <Field label={t("web.f.readTimeout")} hint={t("web.f.secondsDefault")}>
              <input className="input" type="number" min={0} value={spec.readTimeout || ""} onChange={(e) => setSpec({ readTimeout: num(e.target.value) })} />
            </Field>
            <Field label={t("web.f.sendTimeout")} hint={t("web.f.secondsDefault")}>
              <input className="input" type="number" min={0} value={spec.sendTimeout || ""} onChange={(e) => setSpec({ sendTimeout: num(e.target.value) })} />
            </Field>
          </div>
          {!caddy && (
            <Check checked={spec.webSocket} onChange={(v) => setSpec({ webSocket: v })}>
              {t("web.f.websocket")}
            </Check>
          )}
          {caddy && <div className="hint">{t("web.f.websocketCaddy")}</div>}
          <div className="hint" style={{ margin: "6px 0 10px" }}>
            {t("web.f.proxyHeadersNote")}
          </div>
          <Field label={t("web.f.requestHeaders")}>
            <HeaderList list={spec.requestHeaders ?? []} onChange={(l) => setSpec({ requestHeaders: l })} />
          </Field>
        </>
      )}
      {spec.type === "static" && (
        <>
          <div className="form-grid">
            <Field label={t("web.f.root")}>
              <input className="input mono" value={spec.root} placeholder="/var/www/site" onChange={(e) => setSpec({ root: e.target.value })} />
            </Field>
            <Field label={t("web.f.index")}>
              <input className="input mono" value={spec.index} onChange={(e) => setSpec({ index: e.target.value })} />
            </Field>
          </div>
          <Check checked={spec.spa} onChange={(v) => setSpec({ spa: v })}>
            {t("web.f.spa")}
          </Check>
        </>
      )}
      {spec.type === "redirect" && (
        <>
          <div className="form-grid">
            <Field label={t("web.f.redirectTo")}>
              <input className="input mono" value={spec.redirectTo} placeholder="https://new.example.com" onChange={(e) => setSpec({ redirectTo: e.target.value })} />
            </Field>
            <Field label={t("web.f.redirectCode")}>
              <Select
                value={spec.redirectCode}
                onChange={(v) => setSpec({ redirectCode: v })}
                options={[301, 302, 307, 308].map((c) => ({ value: c, label: `${c} — ${t(`web.code.${c}` as "web.code.301")}` }))}
              />
            </Field>
          </div>
          <Check checked={spec.preservePath} onChange={(v) => setSpec({ preservePath: v })}>
            {t("web.f.preservePath")}
          </Check>
        </>
      )}
    </FormSection>
  );
}

function SSLSection({ spec, setSpec, gc, caddy, onInstallCertbot }: { spec: SiteSpec; setSpec: SetSpec; gc: GenContext; caddy: boolean; onInstallCertbot: () => void }) {
  const t = useT();
  const modes: [string, Key][] = [
    ["none", "web.ssl.none"],
    ["letsencrypt", "web.ssl.le"],
    ["custom", "web.ssl.custom"],
  ];
  if (caddy) modes.push(["internal", "web.ssl.internal"]);
  const installed = useMemo(
    () => [
      ...(gc.customCerts ?? []).map((n) => ({ key: "c:" + n, label: `${n} (${t("web.src.custom")})`, cert: `/etc/ssl/server-manager/${n}/fullchain.pem`, key2: `/etc/ssl/server-manager/${n}/privkey.pem`, name: n })),
      ...(gc.leCerts ?? []).map((n) => ({ key: "l:" + n, label: `${n} (Let's Encrypt)`, cert: `/etc/letsencrypt/live/${n}/fullchain.pem`, key2: `/etc/letsencrypt/live/${n}/privkey.pem`, name: n })),
    ],
    [gc],
  );
  const selected = installed.find((c) => c.cert === spec.certPath)?.key ?? (spec.certPath ? "manual" : "");
  const tls = spec.ssl !== "none";
  return (
    <FormSection title={t("web.f.ssl")}>
      <div className="segmented" style={{ marginBottom: 12 }}>
        {modes.map(([k, l]) => (
          <button key={k} className={spec.ssl === k ? "on" : ""} onClick={() => setSpec({ ssl: k })}>
            {t(l)}
          </button>
        ))}
      </div>
      {spec.ssl === "letsencrypt" && (
        <>
          {!caddy && !gc.certbot && (
            <div className="web-alert warn">
              <AlertTriangle size={14} />
              <span>{t("web.certbotMissing")}</span>
              <button className="btn sm" onClick={onInstallCertbot}>
                {t("web.installCertbot")}
              </button>
            </div>
          )}
          <div className="form-grid">
            <Field label={t("web.f.email")} hint={t(caddy ? "web.f.emailHintCaddy" : "web.f.emailHint")}>
              <input className="input" type="email" value={spec.email} placeholder="ops@example.com" onChange={(e) => setSpec({ email: e.target.value })} />
            </Field>
            {!caddy && (
              <Field label={t("web.f.keyType")}>
                <Select
                  value={spec.keyType}
                  onChange={(v) => setSpec({ keyType: v })}
                  options={[
                    { value: "ecdsa", label: "ECDSA (P-256)" },
                    { value: "rsa", label: "RSA 2048" },
                  ]}
                />
              </Field>
            )}
          </div>
          <Check checked={spec.staging} onChange={(v) => setSpec({ staging: v })}>
            {t("web.f.staging")}
          </Check>
          <div className="hint" style={{ marginTop: 6 }}>
            {t(caddy ? "web.f.leHintCaddy" : "web.f.leHint")}
          </div>
        </>
      )}
      {spec.ssl === "custom" && (
        <>
          <Field label={t("web.f.installedCert")} hint={installed.length === 0 ? t("web.f.noInstalledCert") : undefined}>
            <Select
              value={selected}
              onChange={(v) => {
                const c = installed.find((x) => x.key === v);
                if (c) setSpec({ certPath: c.cert, keyPath: c.key2, certName: c.name });
                else if (v === "manual") setSpec({ certName: "" });
              }}
              options={[{ value: "", label: "—" }, ...installed.map((c) => ({ value: c.key, label: c.label })), { value: "manual", label: t("web.f.manualPaths") }]}
            />
          </Field>
          {selected === "manual" || (!selected && spec.certPath) ? (
            <div className="form-grid">
              <Field label={t("web.f.certPath")}>
                <input className="input mono" value={spec.certPath} onChange={(e) => setSpec({ certPath: e.target.value })} />
              </Field>
              <Field label={t("web.f.keyPath")}>
                <input className="input mono" value={spec.keyPath} onChange={(e) => setSpec({ keyPath: e.target.value })} />
              </Field>
            </div>
          ) : null}
        </>
      )}
      {spec.ssl === "internal" && <div className="hint">{t("web.f.internalHint")}</div>}
      {tls && (
        <div className="web-checks">
          {!caddy ? (
            <>
              <Check checked={spec.forceHttps} onChange={(v) => setSpec({ forceHttps: v })}>
                {t("web.f.forceHttps")}
              </Check>
              <Check checked={spec.http2} onChange={(v) => setSpec({ http2: v })}>
                {t("web.f.http2")}
              </Check>
            </>
          ) : (
            <div className="hint">{t("web.f.caddyAutoHttps")}</div>
          )}
          <Check checked={spec.hsts} onChange={(v) => setSpec({ hsts: v })}>
            {t("web.f.hsts")}
          </Check>
          {spec.hsts && (
            <div className="row web-indent">
              <Field label={t("web.f.hstsMaxAge")} hint={t("web.f.hstsMaxAgeHint", { d: Math.round((spec.hstsMaxAge || 0) / 86400) })}>
                <input className="input" type="number" min={60} value={spec.hstsMaxAge || ""} onChange={(e) => setSpec({ hstsMaxAge: num(e.target.value) })} />
              </Field>
              <div className="field" style={{ justifyContent: "flex-end" }}>
                <Check checked={spec.hstsSubdomains} onChange={(v) => setSpec({ hstsSubdomains: v })}>
                  includeSubDomains
                </Check>
              </div>
            </div>
          )}
        </div>
      )}
    </FormSection>
  );
}

function SecuritySection({ spec, setSpec, caddy, editing }: { spec: SiteSpec; setSpec: SetSpec; caddy: boolean; editing: boolean }) {
  const t = useT();
  const users = spec.authUsers ?? [];
  const setUsers = (l: AuthUser[]) => setSpec({ authUsers: l });
  return (
    <FormSection title={t("web.f.security")}>
      <div className="form-grid">
        <Field label={t("web.f.maxBody")} hint={t("web.f.maxBodyHint")}>
          <input className="input" type="number" min={0} value={spec.maxBodyMb || ""} placeholder={caddy ? "∞" : "1"} onChange={(e) => setSpec({ maxBodyMb: num(e.target.value) })} />
        </Field>
      </div>
      <Check checked={spec.gzip} onChange={(v) => setSpec({ gzip: v })}>
        {t(caddy ? "web.f.gzipCaddy" : "web.f.gzip")}
      </Check>
      {!caddy && (
        <>
          <Check checked={spec.rateLimit} onChange={(v) => setSpec({ rateLimit: v })}>
            {t("web.f.rateLimit")}
          </Check>
          {spec.rateLimit && (
            <div className="form-grid web-indent">
              <Field label={t("web.f.rps")}>
                <input className="input" type="number" min={1} value={spec.rateRps || ""} onChange={(e) => setSpec({ rateRps: num(e.target.value) })} />
              </Field>
              <Field label={t("web.f.burst")}>
                <input className="input" type="number" min={0} value={spec.rateBurst || ""} onChange={(e) => setSpec({ rateBurst: num(e.target.value) })} />
              </Field>
              <div className="field" style={{ justifyContent: "flex-end" }}>
                <Check checked={spec.rateNoDelay} onChange={(v) => setSpec({ rateNoDelay: v })}>
                  nodelay
                </Check>
              </div>
            </div>
          )}
        </>
      )}
      <Check checked={spec.basicAuth} onChange={(v) => setSpec({ basicAuth: v, authUsers: v && users.length === 0 ? [{ user: "", password: "" }] : users })}>
        {t("web.f.basicAuth")}
      </Check>
      {spec.basicAuth && (
        <div className="web-indent">
          <Field label={t("web.f.realm")}>
            <input className="input" value={spec.authRealm} placeholder="Restricted" onChange={(e) => setSpec({ authRealm: e.target.value })} />
          </Field>
          <label className="web-sublabel">{t("web.f.users")}</label>
          {users.map((u, i) => (
            <div className="row web-listrow" key={i}>
              <input
                className="input"
                value={u.user}
                placeholder={t("web.f.user")}
                autoComplete="off"
                onChange={(e) => setUsers(users.map((x, j) => (j === i ? { ...x, user: e.target.value } : x)))}
              />
              <input
                className="input"
                type="password"
                value={u.password ?? ""}
                autoComplete="new-password"
                placeholder={editing ? t("web.f.passwordKeep") : t("web.f.password")}
                onChange={(e) => setUsers(users.map((x, j) => (j === i ? { ...x, password: e.target.value } : x)))}
              />
              <button className="icon-btn" style={{ flex: "none" }} onClick={() => setUsers(users.filter((_, j) => j !== i))}>
                <X size={13} />
              </button>
            </div>
          ))}
          <button className="btn sm ghost" onClick={() => setUsers([...users, { user: "", password: "" }])}>
            <Plus size={12} /> {t("web.f.addUser")}
          </button>
          <div className="hint">{t("web.f.bcryptHint")}</div>
        </div>
      )}
    </FormSection>
  );
}

function HeaderList({ list, onChange }: { list: Header[]; onChange: (l: Header[]) => void }) {
  const t = useT();
  return (
    <div className="web-headers">
      {list.map((h, i) => (
        <div className="row web-listrow" key={i}>
          <input className="input mono" style={{ flex: "0 0 38%" }} value={h.name} placeholder="X-Header" onChange={(e) => onChange(list.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))} />
          <input className="input mono" value={h.value} placeholder={t("web.f.value")} onChange={(e) => onChange(list.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)))} />
          <button className="icon-btn" style={{ flex: "none" }} onClick={() => onChange(list.filter((_, j) => j !== i))}>
            <X size={13} />
          </button>
        </div>
      ))}
      <button className="btn sm ghost" onClick={() => onChange([...list, { name: "", value: "" }])}>
        <Plus size={12} /> {t("web.f.addHeader")}
      </button>
    </div>
  );
}
