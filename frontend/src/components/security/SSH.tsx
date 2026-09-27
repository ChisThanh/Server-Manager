import { useMemo, useState } from "react";
import { AlertTriangle, CheckCircle2, Plus, RefreshCw, RotateCcw, Save, X } from "lucide-react";
import { ErrorBox, KV, Loading, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { toast, useUI } from "../../store/ui";
import { useT } from "../../i18n";
import {
  SecurityService,
  confirmOpts,
  errCode,
  errMsg,
  errParams,
  isCancelled,
  sudo,
  tk,
  type SSHConfig,
  type SSHConfirm,
  type SSHUpdateResult,
  type SecViewProps,
} from "./common";
import { Select } from "../../ui/Select";

type Kind = "select" | "number" | "list";
interface Field {
  key: string;
  kind: Kind;
  options?: string[];
  min?: number;
  max?: number;
  cur: (c: SSHConfig) => string;
}

const FIELDS: Field[] = [
  { key: "PermitRootLogin", kind: "select", options: ["no", "prohibit-password", "forced-commands-only", "yes"], cur: (c) => c.permitRootLogin },
  { key: "PasswordAuthentication", kind: "select", options: ["no", "yes"], cur: (c) => c.passwordAuthentication },
  { key: "KbdInteractiveAuthentication", kind: "select", options: ["no", "yes"], cur: (c) => c.kbdInteractiveAuthentication },
  { key: "PubkeyAuthentication", kind: "select", options: ["yes", "no"], cur: (c) => c.pubkeyAuthentication },
  { key: "MaxAuthTries", kind: "number", min: 1, max: 100, cur: (c) => String(c.maxAuthTries) },
  { key: "X11Forwarding", kind: "select", options: ["no", "yes"], cur: (c) => c.x11Forwarding },
  { key: "ClientAliveInterval", kind: "number", min: 0, max: 86400, cur: (c) => String(c.clientAliveInterval) },
  { key: "ClientAliveCountMax", kind: "number", min: 0, max: 1000, cur: (c) => String(c.clientAliveCountMax) },
  { key: "AllowUsers", kind: "list", cur: (c) => (c.allowUsers ?? []).join(" ") },
  { key: "AllowGroups", kind: "list", cur: (c) => (c.allowGroups ?? []).join(" ") },
];

// Values considered risky (shown with a warning tone).
const RISKY: Record<string, string> = { PermitRootLogin: "yes", PasswordAuthentication: "yes", X11Forwarding: "yes", PubkeyAuthentication: "no" };

const GUARD_CODES = ["sec.ssh.noKeys", "sec.ssh.rootSelf", "sec.ssh.pubkeySelf", "sec.ssh.lockout", "sec.ssh.removeCurrentPort", "sec.ssh.confirmPorts"];

export function SSHView({ connId, active, canSec }: SecViewProps) {
  const t = useT();
  const cfg = useRemote(() => sudo(connId, (pw) => SecurityService.SSHConfig(connId, pw)), [connId], { enabled: active });
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [newPort, setNewPort] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<SSHUpdateResult | null>(null);
  const c = cfg.data;

  const changes = useMemo(() => {
    if (!c) return {};
    const out: Record<string, string[]> = {};
    for (const f of FIELDS) {
      if (!(f.key in draft)) continue;
      const v = draft[f.key].trim();
      if (v === f.cur(c)) continue;
      out[f.key] = v.split(/\s+/).filter(Boolean);
    }
    return out;
  }, [draft, c]);
  const invalid = FIELDS.some((f) => {
    if (f.kind !== "number" || !(f.key in draft)) return false;
    const n = Number(draft[f.key]);
    return !/^\d+$/.test(draft[f.key].trim()) || n < (f.min ?? 0) || n > (f.max ?? 1e9);
  });

  /** Asks the user about a guard error; returns true to retry with the flag set. */
  const askGuard = async (e: unknown, confirm: SSHConfirm): Promise<boolean> => {
    const code = errCode(e) ?? "";
    const p = errParams(e);
    const user = c?.loginUser ?? "";
    switch (code) {
      case "sec.ssh.noKeys":
        if (!(await confirmOpts({ serverId: connId, title: t("sec.ssh.noKeysTitle"), message: t("sec.ssh.noKeysMsg", { user }), confirmText: t("sec.ssh.disableAnyway"), typeWord: user }))) return false;
        confirm.noKeys = true;
        return true;
      case "sec.ssh.rootSelf":
        if (!(await confirmOpts({ serverId: connId, title: t("sec.ssh.rootSelfTitle"), message: t("sec.ssh.rootSelfMsg", { value: p.value ?? "" }), confirmText: t("sec.ssh.applyAnyway") }))) return false;
        confirm.rootLogin = true;
        return true;
      case "sec.ssh.pubkeySelf":
        if (!(await confirmOpts({ serverId: connId, title: t("sec.ssh.pubkeySelfTitle"), message: t("sec.ssh.pubkeySelfMsg"), confirmText: t("sec.ssh.applyAnyway"), typeWord: user }))) return false;
        confirm.pubkeySelf = true;
        return true;
      case "sec.ssh.lockout":
        if (!(await confirmOpts({ serverId: connId, title: t("sec.ssh.lockoutTitle"), message: t("sec.ssh.lockoutMsg", { user }), confirmText: t("sec.ssh.applyAnyway"), typeWord: user }))) return false;
        confirm.lockout = true;
        return true;
      case "sec.ssh.removeCurrentPort":
        if (!(await confirmOpts({ serverId: connId, title: t("sec.ssh.removePortTitle", { port: p.port ?? "" }), message: t("sec.ssh.removePortMsg", { port: p.port ?? "" }), confirmText: t("sec.ssh.removePortBtn") }))) return false;
        confirm.removeCurrentPort = true;
        return true;
      case "sec.ssh.confirmPorts": {
        const ports = (p.ports ?? "").split(/[,\s]+/).filter(Boolean).map(Number);
        const checks = await Promise.all(
          ports.map((port) =>
            sudo(connId, (pw) => SecurityService.CheckPort(connId, port, "tcp", pw))
              .then((r) => ({ port, r }))
              .catch(() => ({ port, r: null })),
          ),
        );
        const blocked = checks.filter((x) => x.r && x.r.active && x.r.allowed !== "yes");
        const r = await useUI.getState().openDialog({
          title: t("sec.ssh.newPortTitle"),
          message: (
            <div>
              <p>{t("sec.ssh.newPortMsg")}</p>
              <ul className="sec-list">
                {checks.map(({ port, r }) => (
                  <li key={port}>
                    <b className="mono">{port}/tcp</b> —{" "}
                    {!r
                      ? t("sec.ssh.fwUnknown")
                      : !r.active
                        ? t("sec.ssh.fwInactive", { backend: r.backend })
                        : r.allowed === "yes"
                          ? t("sec.ssh.fwAllowed", { backend: r.backend })
                          : r.allowed === "no"
                            ? t("sec.ssh.fwBlocked", { backend: r.backend })
                            : t("sec.ssh.fwMaybe", { backend: r.backend })}
                  </li>
                ))}
              </ul>
              <p className="muted">{t("sec.ssh.keepOldPort")}</p>
            </div>
          ),
          confirmText: blocked.length ? t("sec.ssh.continueWithout") : t("common.confirm"),
          danger: blocked.length > 0,
          extra: blocked.length ? [{ id: "allow", label: t("sec.ssh.allowFirst") }] : [],
        });
        if (!r || (r.action !== "ok" && r.action !== "allow")) return false;
        if (r.action === "allow") {
          for (const { port } of blocked) {
            try {
              const backend = await sudo(connId, (pw) => SecurityService.AllowPort(connId, port, "tcp", "SSH", pw));
              toast(t("sec.ssh.fwRuleAdded", { port, backend }), "success");
            } catch (err) {
              toast(errMsg(err), "error");
              return false;
            }
          }
        }
        confirm.newPorts = true;
        return true;
      }
    }
    return false;
  };

  const apply = async (set: Record<string, string[]>) => {
    const confirm: SSHConfirm = { noKeys: false, rootLogin: false, pubkeySelf: false, newPorts: false, removeCurrentPort: false, lockout: false };
    setBusy(true);
    try {
      for (let i = 0; i < 8; i++) {
        try {
          const r = await sudo(connId, (pw) => SecurityService.UpdateSSH(connId, { set, confirm }, pw));
          setResult(r);
          setDraft({});
          cfg.setData(r.config);
          toast(t("sec.ssh.applied"), "success");
          return;
        } catch (e) {
          if (isCancelled(e)) return;
          if (!GUARD_CODES.includes(errCode(e) ?? "")) {
            toast(errMsg(e), "error");
            return;
          }
          if (!(await askGuard(e, confirm))) return;
        }
      }
    } finally {
      setBusy(false);
    }
  };

  if (cfg.error && !c) return <ErrorBox error={cfg.error} onRetry={cfg.reload} />;
  if (!c) return <Loading label={t("sec.ssh.loading")} />;

  const managed = (c.managed ?? {}) as Record<string, string[] | null | undefined>;
  const ports = c.ports ?? [];
  const noKeyWarn = c.authType === "password" && c.loginKeyCount === 0;
  const portValid = /^\d{1,5}$/.test(newPort) && +newPort >= 1 && +newPort <= 65535 && !ports.includes(+newPort);

  return (
    <div className="sec-ssh">
      <div className="sec-grid2">
        <Section
          title={t("sec.ssh.effective")}
          actions={
            <button className="icon-btn" onClick={cfg.reload} title={t("common.refresh")} disabled={cfg.loading}>
              <RefreshCw size={14} className={cfg.loading ? "spin-icon" : ""} />
            </button>
          }
        >
          <div className="sec-form">
            {FIELDS.map((f) => {
              const cur = f.cur(c);
              const val = draft[f.key] ?? cur;
              const changed = f.key in changes;
              const isManaged = !!managed[f.key];
              return (
                <div key={f.key} className={`sec-frow ${changed ? "changed" : ""}`}>
                  <div className="sec-flabel">
                    <div className="mono">{f.key}</div>
                    <div className="muted sec-small">{tk(`sec.ssh.help.${f.key}`)}</div>
                  </div>
                  <div className="sec-fctl">
                    {f.kind === "select" ? (
                      <Select
                        size="sm"
                        value={val}
                        disabled={!canSec || busy}
                        onChange={(v) => setDraft({ ...draft, [f.key]: v })}
                        options={[...(f.options!.includes(val) ? [] : [{ value: val, label: val || "—" }]), ...f.options!]}
                      />
                    ) : (
                      <input
                        className={`input input-sm ${f.kind === "number" ? "sec-num" : ""}`}
                        value={val}
                        disabled={!canSec || busy}
                        placeholder={f.kind === "list" ? t("sec.ssh.listPlaceholder") : ""}
                        onChange={(e) => setDraft({ ...draft, [f.key]: e.target.value })}
                      />
                    )}
                    {RISKY[f.key] === cur && <StateBadge tone="warn">{t("sec.ssh.risky")}</StateBadge>}
                    {isManaged && (
                      <span className="badge" title={t("sec.ssh.managedHint")}>
                        {t("sec.ssh.managed")}
                      </span>
                    )}
                    {isManaged && canSec && (
                      <button className="icon-btn" title={t("sec.ssh.resetKey")} disabled={busy} onClick={() => apply({ [f.key]: [] })}>
                        <RotateCcw size={13} />
                      </button>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
          {canSec && (
            <div className="sec-actions">
              <button className="btn primary" disabled={busy || invalid || Object.keys(changes).length === 0} onClick={() => apply(changes)}>
                {busy ? <span className="spinner" /> : <Save size={13} />} {t("sec.ssh.apply", { n: Object.keys(changes).length })}
              </button>
              {Object.keys(draft).length > 0 && (
                <button className="btn ghost" disabled={busy} onClick={() => setDraft({})}>
                  {t("sec.discard")}
                </button>
              )}
              <span className="muted sec-small">{t("sec.ssh.applyHint")}</span>
            </div>
          )}
        </Section>

        <div>
          <Section title={t("sec.ssh.ports")}>
            <div className="chips">
              {ports.map((p) => (
                <span key={p} className="chip mono sec-port-chip">
                  {p}/tcp
                  {p === c.sessionPort && <span className="muted"> · {t("sec.ssh.thisSession")}</span>}
                  {canSec && ports.length > 1 && (
                    <button className="icon-btn sec-chip-x" title={t("sec.ssh.removePort")} disabled={busy} onClick={() => apply({ Port: ports.filter((x) => x !== p).map(String) })}>
                      <X size={11} />
                    </button>
                  )}
                </span>
              ))}
            </div>
            {canSec && (
              <div className="toolbar sec-mt">
                <input className="input input-sm sec-num" placeholder="2222" value={newPort} onChange={(e) => setNewPort(e.target.value.trim())} />
                <button className="btn sm" disabled={!portValid || busy} onClick={() => apply({ Port: [...ports.map(String), newPort] }).then(() => setNewPort(""))}>
                  <Plus size={13} /> {t("sec.ssh.addPort")}
                </button>
              </div>
            )}
            <div className="hint-box sec-mt">{t("sec.ssh.portHint")}</div>
          </Section>

          <Section title={t("sec.ssh.about")}>
            <KV
              items={[
                [t("sec.ssh.version"), <span className="mono">{c.version || "—"}</span>],
                [t("sec.ssh.mode"), c.mode === "dropin" ? t("sec.ssh.modeDropin") : t("sec.ssh.modeMain")],
                [t("sec.ssh.configPath"), <span className="mono">{c.configPath}</span>],
                [t("sec.ssh.files"), <span className="mono sec-small">{(c.files ?? []).join(", ")}</span>],
                [t("sec.ssh.listen"), <span className="mono">{(c.listenAddresses ?? []).join(", ") || "—"}</span>],
                [t("sec.ssh.session"), t("sec.ssh.sessionInfo", { user: c.loginUser, auth: tk(`sec.auth.${c.authType}`, undefined, c.authType), port: c.sessionPort })],
                [t("sec.ssh.loginKeys"), String(c.loginKeyCount)],
                ...(c.selinux ? [[t("sec.ssh.selinux"), c.selinux] as [string, string]] : []),
              ]}
            />
            {noKeyWarn && (
              <div className="hint-box sec-warnbox sec-mt">
                <AlertTriangle size={13} /> {t("sec.ssh.noKeyWarn", { user: c.loginUser })}
              </div>
            )}
          </Section>

          {result && (
            <Section title={t("sec.ssh.result")}>
              <div className="sec-result">
                <div>
                  <CheckCircle2 size={14} className="sec-ic ok" /> {result.reloaded ? t("sec.ssh.reloaded") : t("sec.ssh.written")}
                </div>
                {(result.listening ?? []).length > 0 && <div className="muted">{t("sec.ssh.listeningNow", { ports: (result.listening ?? []).join(", ") })}</div>}
                {result.backup && <div className="muted mono sec-small">{t("sec.ssh.backup", { path: result.backup })}</div>}
                {(result.overridden ?? []).length > 0 && (
                  <div className="hint-box sec-warnbox sec-mt">
                    <AlertTriangle size={13} /> {t("sec.ssh.overridden", { keys: (result.overridden ?? []).join(", ") })}
                  </div>
                )}
              </div>
            </Section>
          )}
        </div>
      </div>
    </div>
  );
}
