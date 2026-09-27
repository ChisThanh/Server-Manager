import { useEffect, useMemo, useState } from "react";
import { Plus, Trash2 } from "lucide-react";
import { ServerService, SystemService, type Server } from "../lib/api";
import type { HTTPCheck } from "../../bindings/server-manager/internal/store";
import { useApp } from "../store/app";
import { useSettings } from "../store/settings";
import { toast } from "../store/ui";
import { errMsg } from "../lib/api";
import { useT, type Key } from "../i18n";

type Set = <K extends keyof Server>(k: K, v: Server[K]) => void;

function TagInput({ value, onChange, suggestions, placeholder }: { value: string[]; onChange: (v: string[]) => void; suggestions: string[]; placeholder: string }) {
  const [txt, setTxt] = useState("");
  const add = (v: string) => {
    const x = v.trim().replace(/^#/, "");
    if (x && !value.includes(x)) onChange([...value, x]);
    setTxt("");
  };
  return (
    <div className="tag-input">
      <div className="chips">
        {value.map((v) => (
          <span className="chip" key={v}>
            {v}
            <button type="button" onClick={() => onChange(value.filter((x) => x !== v))}>
              ×
            </button>
          </span>
        ))}
      </div>
      <input
        className="input"
        value={txt}
        list={`sugg-${placeholder}`}
        placeholder={placeholder}
        onChange={(e) => setTxt(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === ",") {
            e.preventDefault();
            add(txt);
          } else if (e.key === "Backspace" && !txt && value.length) {
            onChange(value.slice(0, -1));
          }
        }}
        onBlur={() => txt && add(txt)}
      />
      <datalist id={`sugg-${placeholder}`}>
        {suggestions
          .filter((s) => !value.includes(s))
          .map((s) => (
            <option key={s} value={s} />
          ))}
      </datalist>
    </div>
  );
}

export function OrganizationTab({ s, set }: { s: Server; set: Set }) {
  const t = useT();
  const servers = useApp((st) => st.servers);
  const tags = useMemo(() => Array.from(new Set(servers.flatMap((x) => x.tags ?? []))), [servers]);
  const regions = useMemo(() => Array.from(new Set(servers.map((x) => x.region).filter(Boolean))), [servers]);
  const providers = useMemo(() => Array.from(new Set(["AWS", "Google Cloud", "Azure", "DigitalOcean", "Hetzner", "Vultr", "Linode", "OVH", "Viettel IDC", "VNPT", "FPT Cloud", "BizFly", ...servers.map((x) => x.provider).filter(Boolean)])), [servers]);
  return (
    <>
      <div className="field">
        <label>{t("sd.environment")}</label>
        <div className="segmented">
          {["", "production", "staging", "development"].map((e) => (
            <button type="button" key={e || "none"} className={s.environment === e ? "on" : ""} onClick={() => set("environment", e)}>
              {e ? t(`app.env.${e}` as Key) : t("sd.envNone")}
            </button>
          ))}
        </div>
        {s.environment === "production" && <div className="hint">{t("sd.prodHint")}</div>}
      </div>
      <div className="field">
        <label>Tags</label>
        <TagInput value={s.tags ?? []} onChange={(v) => set("tags", v)} suggestions={tags} placeholder={t("sd.tagsPh")} />
      </div>
      <div className="row">
        <div className="field">
          <label>{t("sd.region")}</label>
          <input className="input" value={s.region} list="sm-regions" onChange={(e) => set("region", e.target.value)} placeholder="ap-southeast-1, HN, HCM…" />
          <datalist id="sm-regions">
            {regions.map((r) => (
              <option key={r} value={r} />
            ))}
          </datalist>
        </div>
        <div className="field">
          <label>{t("sd.provider")}</label>
          <input className="input" value={s.provider} list="sm-providers" onChange={(e) => set("provider", e.target.value)} />
          <datalist id="sm-providers">
            {providers.map((r) => (
              <option key={r} value={r} />
            ))}
          </datalist>
        </div>
      </div>
      <div className="field">
        <label>{t("sd.notes")}</label>
        <textarea className="input" rows={3} value={s.notes} onChange={(e) => set("notes", e.target.value)} placeholder={t("sd.notesPh")} />
      </div>
    </>
  );
}

export function MonitoringTab({ s, set }: { s: Server; set: Set }) {
  const t = useT();
  const connected = useApp((st) => st.conns[s.id]?.status === "connected");
  const [units, setUnits] = useState<string[]>([]);
  useEffect(() => {
    if (!s.id || !connected) return;
    SystemService.Services(s.id)
      .then((l) => setUnits((l ?? []).filter((u) => u.sub === "running").map((u) => u.name.replace(/\.service$/, ""))))
      .catch(() => {});
  }, [s.id, connected]);
  const checks = s.httpChecks ?? [];
  const setCheck = (i: number, patch: Partial<HTTPCheck>) => set("httpChecks", checks.map((c, j) => (j === i ? { ...c, ...patch } : c)));
  return (
    <>
      <label className="check" style={{ alignItems: "flex-start" }}>
        <input type="checkbox" checked={s.monitor} onChange={(e) => set("monitor", e.target.checked)} />
        <span>
          {t("sd.monitor")}
          <div className="hint">{t("sd.monitorHint")}</div>
        </span>
      </label>
      {s.monitor && s.authType === "password" && !s.hasSecret && <div className="hint-box" style={{ marginTop: 6 }}>{t("sd.monitorNeedsSecret")}</div>}
      <div className="field" style={{ marginTop: 12 }}>
        <label>{t("sd.watchServices")}</label>
        <TagInput value={s.watchServices ?? []} onChange={(v) => set("watchServices", v)} suggestions={units} placeholder={t("sd.watchPh")} />
        <div className="hint">{t("sd.watchHint")}</div>
      </div>
      <div className="field">
        <label>{t("sd.httpChecks")}</label>
        {checks.map((c, i) => (
          <div className="http-check" key={i}>
            <input className="input" style={{ flex: 1 }} value={c.name} placeholder={t("sd.checkName")} onChange={(e) => setCheck(i, { name: e.target.value })} />
            <input className="input mono" style={{ flex: 2.4 }} value={c.url} placeholder="https://api.example.com/health" onChange={(e) => setCheck(i, { url: e.target.value })} />
            <input className="input" style={{ width: 64 }} type="number" value={c.expect || ""} placeholder="2xx" title={t("sd.expectStatus")} onChange={(e) => setCheck(i, { expect: Number(e.target.value) || 0 })} />
            <input className="input" style={{ flex: 1 }} value={c.contains} placeholder={t("sd.contains")} onChange={(e) => setCheck(i, { contains: e.target.value })} />
            <label className="check" title={t("sd.viaServerHint")}>
              <input type="checkbox" checked={c.viaServer} onChange={(e) => setCheck(i, { viaServer: e.target.checked })} /> {t("sd.viaServer")}
            </label>
            <button type="button" className="icon-btn" onClick={() => set("httpChecks", checks.filter((_, j) => j !== i))}>
              <Trash2 size={13} />
            </button>
          </div>
        ))}
        <button type="button" className="btn sm" onClick={() => set("httpChecks", [...checks, { name: "", url: "", expect: 0, contains: "", viaServer: false }])}>
          <Plus size={12} /> {t("sd.addCheck")}
        </button>
        <div className="hint">{t("sd.httpHint")}</div>
      </div>
    </>
  );
}

export function AccessTab({ s, set, isNew }: { s: Server; set: Set; isNew: boolean }) {
  const t = useT();
  const rolePerms = useSettings((st) => st.rolePerms);
  const [sudoPw, setSudoPw] = useState("");
  const perms = ["files", "terminal", "exec", "services", "docker", "deploy", "database", "backup", "restore", "web", "security", "users"];
  const saveSudo = async (pw: string) => {
    try {
      await ServerService.SetSudoPassword(s.id, pw);
      await useApp.getState().loadServers();
      set("sudoSaved", pw !== "");
      setSudoPw("");
      toast(pw ? t("sd.sudoSavedOk") : t("sd.sudoForgotten"), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  return (
    <>
      <div className="field">
        <label>{t("sd.role")}</label>
        <div className="segmented">
          {["admin", "operator", "developer", "viewer"].map((r) => (
            <button type="button" key={r} className={(s.role || "admin") === r ? "on" : ""} onClick={() => set("role", r)}>
              {t(`app.role.${r}` as Key)}
            </button>
          ))}
        </div>
        <div className="hint">{t("sd.roleHint")}</div>
      </div>
      <div className="perm-matrix">
        {perms.map((p) => {
          const ok = (rolePerms[s.role || "admin"] ?? []).includes(p);
          return (
            <div key={p} className={`pm ${ok ? "ok" : ""}`}>
              <span>{ok ? "✓" : "✕"}</span> {t(`sd.perm.${p}` as Key)}
            </div>
          );
        })}
      </div>
      {!isNew && (
        <div className="field" style={{ marginTop: 14 }}>
          <label>{t("sd.sudoPassword")}</label>
          <div style={{ display: "flex", gap: 6 }}>
            <input className="input" type="password" value={sudoPw} autoComplete="new-password" placeholder={s.sudoSaved ? "••••••••" : ""} onChange={(e) => setSudoPw(e.target.value)} />
            <button type="button" className="btn" disabled={!sudoPw} onClick={() => saveSudo(sudoPw)}>
              {t("common.save")}
            </button>
            {s.sudoSaved && (
              <button type="button" className="btn" onClick={() => saveSudo("")}>
                {t("sd.forget")}
              </button>
            )}
          </div>
          <div className="hint">{t("sd.sudoHint")}</div>
        </div>
      )}
    </>
  );
}
