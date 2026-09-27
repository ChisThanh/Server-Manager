import { useMemo, useState } from "react";
import { FolderOpen, Loader2 } from "lucide-react";
import { Modal } from "./Overlays";
import { AuthType, ServerService, errMsg, type Server } from "../lib/api";
import { useApp } from "../store/app";
import { confirmDialog, toast } from "../store/ui";
import { useT } from "../i18n";
import { SubTabs } from "../ui/Tabs";
import { AccessTab, MonitoringTab, OrganizationTab } from "./ServerDialogTabs";

export const COLORS = ["#4f8cff", "#34c77b", "#f5a524", "#f0525d", "#a970ff", "#17c3b2", "#ff7ab6", "#8b8b94"];

export function blankServer(): Server {
  return {
    id: "",
    name: "",
    group: "",
    host: "",
    port: 22,
    user: "root",
    authType: AuthType.AuthPassword,
    keyPath: "",
    defaultPath: "",
    color: COLORS[Math.floor(Math.random() * COLORS.length)],
    hasSecret: false,
    createdAt: 0,
    updatedAt: 0,
    lastUsed: 0,
    environment: "",
    tags: [],
    region: "",
    provider: "",
    notes: "",
    role: "admin",
    monitor: false,
    watchServices: [],
    httpChecks: [],
    osInfo: "",
    sudoSaved: false,
  };
}

type DialogTab = "connection" | "organization" | "monitoring" | "access";

export function ServerDialog(props: { server: Server; onClose: () => void; onSaved?: (s: Server) => void; initialTab?: DialogTab }) {
  const t = useT();
  const [s, setS] = useState<Server>(props.server);
  const [tab, setTab] = useState<DialogTab>(props.initialTab ?? "connection");
  const [secret, setSecret] = useState("");
  const [secretTouched, setSecretTouched] = useState(false);
  const [busy, setBusy] = useState<"" | "save" | "test">("");
  const [test, setTest] = useState<{ ok: boolean; text: string } | null>(null);
  // Select the stable array; deriving inside the selector would return a new
  // reference every time and re-render forever under zustand v5.
  const servers = useApp((st) => st.servers);
  const groups = useMemo(() => Array.from(new Set(servers.map((x) => x.group).filter(Boolean))), [servers]);
  const isNew = !props.server.id;

  const set = <K extends keyof Server>(k: K, v: Server[K]) => setS((x) => ({ ...x, [k]: v }));

  // Accept "user@host:port" pasted into the host field.
  const onHost = (v: string) => {
    const m = v.trim().match(/^(?:([^@\s]+)@)?([^:\s]+)(?::(\d+))?$/);
    if (m && (m[1] || m[3])) {
      setS((x) => ({ ...x, host: m[2], user: m[1] ?? x.user, port: m[3] ? Number(m[3]) : x.port }));
    } else {
      set("host", v);
    }
  };

  const valid = s.host.trim() !== "" && s.user.trim() !== "" && (s.httpChecks ?? []).every((c) => !c.url || /^https?:\/\/\S+$/.test(c.url));

  const runTest = async () => {
    setBusy("test");
    setTest(null);
    try {
      for (let i = 0; i < 2; i++) {
        const r = await ServerService.Test(s, secretTouched ? secret : "");
        if (r.status === "ok") {
          setTest({ ok: true, text: t("sd.testOk", { home: r.home }) });
        } else if (r.status === "need-secret") {
          setTest({ ok: false, text: s.authType === AuthType.AuthKey ? t("sd.needPassphrase") : t("sd.needPassword") });
        } else if (r.status === "hostkey" && r.hostKey) {
          const hk = r.hostKey;
          const ok = await confirmDialog(
            hk.mismatch ? t("conn.hostChangedTitle") : t("conn.hostNewTitle"),
            `${hk.host}:${hk.port}\n${hk.keyType}\n${hk.fingerprint}`,
            t("sd.trust"),
            hk.mismatch,
          );
          if (ok) {
            await ServerService.TrustHostKey(hk.host, hk.port, hk.keyBase64);
            continue;
          }
          setTest({ ok: false, text: t("sd.notTrusted") });
        }
        break;
      }
    } catch (e) {
      setTest({ ok: false, text: errMsg(e) });
    } finally {
      setBusy("");
    }
  };

  const save = async () => {
    if (!valid) return;
    setBusy("save");
    try {
      const saved = await ServerService.Save(s, secret, secretTouched);
      await useApp.getState().loadServers();
      toast(isNew ? t("sd.added") : t("sd.saved"), "success");
      props.onSaved?.(saved);
      props.onClose();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy("");
    }
  };

  const pickKey = async () => {
    try {
      const p = await ServerService.PickKeyFile(t("sd.pickKeyTitle"));
      if (p) set("keyPath", p);
    } catch {
      /* dialog cancelled */
    }
  };

  const secretLabel = s.authType === AuthType.AuthKey ? t("sd.passphrase") : t("sd.password");
  const secretHint = !isNew && props.server.hasSecret && !secretTouched ? t("sd.secretKept") : t("sd.secretHint");

  return (
    <Modal
      title={isNew ? t("sd.addTitle") : t("sd.editTitle", { name: props.server.name })}
      size="wide"
      onClose={props.onClose}
      footer={
        <>
          <button className="btn left" onClick={runTest} disabled={!valid || busy !== ""}>
            {busy === "test" && <Loader2 size={14} className="spin-icon" />}
            {t("sd.test")}
          </button>
          <button className="btn" onClick={props.onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save} disabled={!valid || busy !== ""}>
            {isNew ? t("sd.add") : t("common.save")}
          </button>
        </>
      }
    >
      <SubTabs<DialogTab>
        value={tab}
        onChange={setTab}
        items={[
          { id: "connection", label: t("sd.tabConnection") },
          { id: "organization", label: t("sd.tabOrganization") },
          { id: "monitoring", label: t("sd.tabMonitoring") },
          { id: "access", label: t("sd.tabAccess") },
        ]}
      />
      {tab === "organization" && <OrganizationTab s={s} set={set} />}
      {tab === "monitoring" && <MonitoringTab s={s} set={set} />}
      {tab === "access" && <AccessTab s={s} set={set} isNew={isNew} />}
      <form
        style={{ display: tab === "connection" ? undefined : "none" }}
        onSubmit={(e) => {
          e.preventDefault();
          save();
        }}
      >
        <div className="row">
          <div className="field" style={{ flex: 2 }}>
            <label>{t("sd.host")}</label>
            <input className="input" value={s.host} onChange={(e) => onHost(e.target.value)} placeholder={t("sd.hostPh")} autoFocus spellCheck={false} />
          </div>
          <div className="field" style={{ flex: 0.7 }}>
            <label>{t("sd.port")}</label>
            <input className="input" type="number" value={s.port} min={1} max={65535} onChange={(e) => set("port", Number(e.target.value))} />
          </div>
        </div>
        <div className="row">
          <div className="field">
            <label>{t("sd.name")}</label>
            <input className="input" value={s.name} onChange={(e) => set("name", e.target.value)} placeholder={s.host ? `${s.user}@${s.host}` : t("sd.namePh")} />
          </div>
          <div className="field">
            <label>{t("sd.group")}</label>
            <input className="input" value={s.group} onChange={(e) => set("group", e.target.value)} list="sm-groups" placeholder={t("sd.groupPh")} />
            <datalist id="sm-groups">
              {groups.map((g) => (
                <option key={g} value={g} />
              ))}
            </datalist>
          </div>
        </div>

        <div className="field">
          <label>{t("sd.user")}</label>
          <input className="input" value={s.user} onChange={(e) => set("user", e.target.value)} spellCheck={false} autoCapitalize="off" />
        </div>

        <div className="field">
          <label>{t("sd.auth")}</label>
          <div className="segmented">
            {[
              [AuthType.AuthPassword, t("sd.authPassword")],
              [AuthType.AuthKey, t("sd.authKey")],
              [AuthType.AuthAgent, t("sd.authAgent")],
            ].map(([v, label]) => (
              <button type="button" key={v} className={s.authType === v ? "on" : ""} onClick={() => set("authType", v as AuthType)}>
                {label}
              </button>
            ))}
          </div>
        </div>

        {s.authType === AuthType.AuthKey && (
          <div className="field">
            <label>{t("sd.keyPath")}</label>
            <div style={{ display: "flex", gap: 6 }}>
              <input className="input mono" value={s.keyPath} onChange={(e) => set("keyPath", e.target.value)} placeholder={t("sd.keyPh")} spellCheck={false} />
              <button type="button" className="btn" onClick={pickKey}>
                <FolderOpen size={14} /> {t("sd.choose")}
              </button>
            </div>
          </div>
        )}

        {s.authType !== AuthType.AuthAgent && (
          <div className="field">
            <label>{secretLabel}</label>
            <input
              className="input"
              type="password"
              value={secret}
              onChange={(e) => {
                setSecret(e.target.value);
                setSecretTouched(true);
              }}
              placeholder={!isNew && props.server.hasSecret ? "••••••••" : ""}
              autoComplete="new-password"
            />
            <div className="hint">
              {secretHint}
              {!isNew && props.server.hasSecret && (
                <>
                  {" · "}
                  <a
                    href="#"
                    onClick={(e) => {
                      e.preventDefault();
                      setSecret("");
                      setSecretTouched(true);
                    }}
                  >
                    {t("sd.clearSecret")}
                  </a>
                </>
              )}
            </div>
          </div>
        )}
        {s.authType === AuthType.AuthAgent && (
          <div className="hint-box">{t("sd.agentHint")}</div>
        )}

        <div className="row">
          <div className="field">
            <label>{t("sd.defaultPath")}</label>
            <input className="input mono" value={s.defaultPath} onChange={(e) => set("defaultPath", e.target.value)} placeholder={t("sd.defaultPathPh")} spellCheck={false} />
          </div>
          <div className="field" style={{ flex: 0.8 }}>
            <label>{t("sd.color")}</label>
            <div className="color-row" style={{ height: 30, alignItems: "center" }}>
              {COLORS.map((c) => (
                <div key={c} className={`color-swatch ${s.color === c ? "on" : ""}`} style={{ background: c }} onClick={() => set("color", c)} />
              ))}
            </div>
          </div>
        </div>
        {test && <div className={`test-result ${test.ok ? "ok" : "err"}`}>{test.text}</div>}
        <button type="submit" hidden />
      </form>
    </Modal>
  );
}
