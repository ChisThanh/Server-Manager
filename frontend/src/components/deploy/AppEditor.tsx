import { useMemo, useState, type ReactNode } from "react";
import { GitBranch, HeartPulse, KeyRound, Plus, Settings2, Trash2, Undo2, Wrench, Container } from "lucide-react";
import { DeployService } from "../../../bindings/server-manager/services/deploy";
import type { App, AppView, SaveAppInput, SecretChange } from "../../../bindings/server-manager/services/deploy/models";
import { Modal } from "../Overlays";
import { SubTabs } from "../../ui/Tabs";
import { confirmDanger } from "../../ui/confirm";
import { errMsg } from "../../lib/api";
import { toast } from "../../store/ui";
import { useT, type Key } from "../../i18n";
import {
  envPath,
  safeEnvPath,
  validContainer,
  validDir,
  validEnvName,
  validEnvValue,
  validImage,
  validPort,
  validProject,
  validRef,
  validRelOrAbs,
  validRepo,
  validTag,
  validUrl,
  validVolume,
} from "./util";
import { Select } from "../../ui/Select";

type Tab = "source" | "build" | "env" | "health" | "advanced";

interface SecretRow {
  name: string;
  /** set = stored, unchanged; new/replace = value pending; remove = will be deleted */
  state: "set" | "new" | "replace" | "remove";
  value: string;
}

function defaults(connId: string): App {
  return {
    id: "",
    server: connId,
    name: "",
    stage: "production",
    type: "git",
    dir: "",
    envFile: "",
    loadEnv: true,
    sudo: false,
    restartSudo: false,
    repo: "",
    branch: "main",
    tokenUser: "x-access-token",
    hasToken: false,
    submodules: false,
    buildCmd: "",
    testCmd: "",
    restartCmd: "",
    composeFile: "compose.yml",
    project: "",
    tagVar: "APP_TAG",
    defaultTag: "latest",
    skipPull: false,
    image: { image: "", containerName: "", ports: [], volumes: [], restart: "unless-stopped", command: "", healthCmd: "", healthInterval: 0, healthTimeout: 0, healthRetries: 0 },
    vars: [],
    secrets: [],
    health: { type: "none", url: "", expectStatus: 200, bodyContains: "", insecure: false, command: "", timeout: 5, retries: 5, interval: 3, delay: 0 },
    autoRollback: true,
    minFreeMB: 100,
    commandTimeout: 30,
    created: 0,
    updated: 0,
  };
}

const TAB_OF: Record<string, Tab> = {
  name: "source",
  dir: "source",
  repo: "source",
  branch: "source",
  tokenUser: "source",
  token: "source",
  composeFile: "source",
  project: "source",
  tagVar: "source",
  defaultTag: "source",
  image: "source",
  containerName: "source",
  ports: "source",
  volumes: "source",
  imageHealth: "source",
  vars: "env",
  secrets: "env",
  newSecret: "env",
  envFile: "env",
  url: "health",
  healthCommand: "health",
  healthNums: "health",
  expectStatus: "health",
  minFreeMB: "advanced",
  commandTimeout: "advanced",
};

function Field({ label, hint, error, children, wide }: { label: ReactNode; hint?: ReactNode; error?: string; children: ReactNode; wide?: boolean }) {
  return (
    <div className={`field ${wide ? "deploy-wide" : ""}`}>
      <label>{label}</label>
      {children}
      {error ? <span className="error">{error}</span> : hint ? <span className="hint">{hint}</span> : null}
    </div>
  );
}

const lines = (s: string) =>
  s
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

export function AppEditor({ connId, initial, onClose, onSaved }: { connId: string; initial: AppView | null; onClose: () => void; onSaved: (v: AppView | null) => void }) {
  const t = useT();
  const [a, setA] = useState<App>(() => {
    const base = defaults(connId);
    if (!initial) return base;
    const src = initial.app;
    return { ...base, ...src, image: { ...base.image, ...src.image }, health: { ...base.health, ...src.health }, vars: [...(src.vars ?? [])] };
  });
  const [ports, setPorts] = useState((a.image.ports ?? []).join("\n"));
  const [volumes, setVolumes] = useState((a.image.volumes ?? []).join("\n"));
  const [secrets, setSecrets] = useState<SecretRow[]>(() => (initial?.app.secrets ?? []).map((name) => ({ name, state: "set", value: "" })));
  const [newSecret, setNewSecret] = useState({ name: "", value: "" });
  const [token, setToken] = useState<{ action: "keep" | "set" | "remove"; value: string }>({ action: "keep", value: "" });
  const [tab, setTab] = useState<Tab>("source");
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const up = (patch: Partial<App>) => setA((p) => ({ ...p, ...patch }));
  const upImage = (patch: Partial<App["image"]>) => setA((p) => ({ ...p, image: { ...p.image, ...patch } }));
  const upHealth = (patch: Partial<App["health"]>) => setA((p) => ({ ...p, health: { ...p.health, ...patch } }));
  const git = a.type === "git";

  const errors = useMemo(() => {
    const e: Record<string, string> = {};
    if (!a.name.trim()) e.name = t("deploy.v.name");
    if (!validDir(a.dir.trim())) e.dir = t("deploy.v.dir");
    if (a.envFile.trim() && !validRelOrAbs(a.envFile.trim())) e.envFile = t("deploy.v.path");
    else if (a.dir.trim() && !safeEnvPath(envPath({ ...a, envFile: a.envFile.trim() }))) e.envFile = t("deploy.v.envPath");
    if (git) {
      if (!validRepo(a.repo.trim())) e.repo = t("deploy.v.repo");
      if (a.branch.trim() && !validRef(a.branch.trim())) e.branch = t("deploy.v.ref");
      if (a.tokenUser.trim() && !/^[A-Za-z0-9._@+-]{1,100}$/.test(a.tokenUser.trim())) e.tokenUser = t("deploy.v.tokenUser");
      if (token.action === "set" && (token.value.length < 4 || /[\s]/.test(token.value))) e.token = t("deploy.v.token");
    } else {
      if (a.tagVar.trim() && !validEnvName(a.tagVar.trim())) e.tagVar = t("deploy.v.envName");
      if (a.defaultTag.trim() && !validTag(a.defaultTag.trim())) e.defaultTag = t("deploy.v.tag");
      if (a.project.trim() && !validProject(a.project.trim())) e.project = t("deploy.v.project");
      if (a.type === "compose" && a.composeFile.trim() && !validRelOrAbs(a.composeFile.trim())) e.composeFile = t("deploy.v.path");
      if (a.type === "image") {
        if (!validImage(a.image.image.trim())) e.image = t("deploy.v.image");
        if (a.image.containerName.trim() && !validContainer(a.image.containerName.trim())) e.containerName = t("deploy.v.container");
        const badPort = lines(ports).find((p) => !validPort(p));
        if (badPort) e.ports = t("deploy.v.port", { v: badPort });
        const badVol = lines(volumes).find((v) => !validVolume(v));
        if (badVol) e.volumes = t("deploy.v.volume", { v: badVol });
        if (a.image.command.includes("\n") || a.image.healthCmd.includes("\n")) e.imageHealth = t("deploy.v.oneLine");
      }
    }
    // Variables and secrets: valid, unique names; representable values.
    const names = new Map<string, number>();
    const tagVar = git ? "" : a.tagVar.trim() || "APP_TAG";
    if (tagVar) names.set(tagVar, 1);
    for (const v of a.vars ?? []) {
      const n = v.name.trim();
      if (!n && !v.value) continue;
      if (!validEnvName(n)) e.vars = t("deploy.v.envNameX", { name: n || "?" });
      else if (names.has(n)) e.vars = t("deploy.v.dupEnv", { name: n });
      else if (!validEnvValue(v.value)) e.vars = t("deploy.v.envValue", { name: n });
      names.set(n, 1);
    }
    for (const s of secrets) {
      if (s.state === "remove") continue;
      if (names.has(s.name)) e.secrets = t("deploy.v.dupEnv", { name: s.name });
      names.set(s.name, 1);
      if ((s.state === "new" || s.state === "replace") && (s.value.length < 4 || !validEnvValue(s.value))) e.secrets = t("deploy.v.secretValue", { name: s.name });
    }
    const h = a.health;
    if (h.type === "http") {
      if (!validUrl(h.url.trim())) e.url = t("deploy.v.url");
      if (h.expectStatus && (h.expectStatus < 100 || h.expectStatus > 599)) e.expectStatus = t("deploy.v.status");
    }
    if (h.type === "command" && !h.command.trim()) e.healthCommand = t("deploy.v.required");
    if (h.type !== "none" && (h.timeout < 1 || h.timeout > 300 || h.retries < 1 || h.retries > 100 || h.interval < 1 || h.interval > 600 || h.delay < 0 || h.delay > 600)) {
      e.healthNums = t("deploy.v.healthNums");
    }
    if (a.minFreeMB < 0) e.minFreeMB = t("deploy.v.number");
    if (a.commandTimeout < 1 || a.commandTimeout > 1440) e.commandTimeout = t("deploy.v.timeout");
    return e;
  }, [a, ports, volumes, secrets, token, git]);

  const err = (k: string) => (tried ? errors[k] : undefined);
  const tabErrors = (tb: Tab) => (tried ? Object.keys(errors).filter((k) => TAB_OF[k] === tb).length : 0);

  const addSecret = () => {
    const n = newSecret.name.trim();
    if (!validEnvName(n)) {
      toast(t("deploy.v.envNameX", { name: n || "?" }), "error");
      return;
    }
    if (newSecret.value.length < 4 || !validEnvValue(newSecret.value)) {
      toast(t("deploy.v.secretValue", { name: n }), "error");
      return;
    }
    setSecrets((list) => {
      const existing = list.find((s) => s.name === n);
      if (existing) return list.map((s) => (s.name === n ? { ...s, state: existing.state === "new" ? "new" : "replace", value: newSecret.value } : s));
      return [...list, { name: n, state: "new", value: newSecret.value }];
    });
    setNewSecret({ name: "", value: "" });
  };

  const save = async () => {
    setTried(true);
    const keys = Object.keys(errors);
    if (keys.length) {
      setTab(TAB_OF[keys[0]] ?? "source");
      return;
    }
    const changes: SecretChange[] = secrets
      .filter((s) => s.state !== "set")
      .map((s) => (s.state === "remove" ? { name: s.name, value: "", remove: true } : { name: s.name, value: s.value, remove: false }));
    const app: App = {
      ...a,
      name: a.name.trim(),
      dir: a.dir.trim(),
      repo: a.repo.trim(),
      branch: a.branch.trim(),
      image: { ...a.image, ports: lines(ports), volumes: lines(volumes) },
      vars: (a.vars ?? []).filter((v) => v.name.trim() || v.value).map((v) => ({ name: v.name.trim(), value: v.value })),
      secrets: [],
    };
    const input: SaveAppInput = { app, secrets: changes, tokenAction: token.action, token: token.action === "set" ? token.value : "" };
    setBusy(true);
    setError("");
    try {
      const v = await DeployService.SaveApp(connId, input);
      toast(t("deploy.saved", { name: v.app.name }), "success");
      onSaved(v);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!initial) return;
    const ok = await confirmDanger({
      serverId: connId,
      title: t("deploy.deleteQ", { name: initial.app.name }),
      message: t("deploy.deleteMsg"),
      confirmText: t("common.delete"),
    });
    if (!ok) return;
    try {
      await DeployService.DeleteApp(connId, initial.app.id);
      toast(t("deploy.deleted", { name: initial.app.name }), "success");
      onSaved(null);
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const typeBtn = (ty: string, label: Key) => (
    <button key={ty} className={a.type === ty ? "on" : ""} onClick={() => up({ type: ty })}>
      {t(label)}
    </button>
  );

  return (
    <Modal
      title={
        <>
          <Settings2 size={16} />
          {initial ? t("deploy.editTitle", { name: initial.app.name }) : t("deploy.newTitle")}
        </>
      }
      size="xwide"
      onClose={onClose}
      footer={
        <>
          {initial && (
            <button className="btn danger left" onClick={remove}>
              <Trash2 size={14} /> {t("common.delete")}
            </button>
          )}
          {error && <span className="err deploy-foot-err">{error}</span>}
          {tried && Object.keys(errors).length > 0 && !error && <span className="err deploy-foot-err">{t("deploy.v.fix")}</span>}
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" onClick={save} disabled={busy}>
            {busy && <span className="spinner" />}
            {t("common.save")}
          </button>
        </>
      }
    >
      <SubTabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          { id: "source", label: t("deploy.tab.source"), icon: <GitBranch size={13} />, badge: tabErrors("source") || undefined },
          { id: "build", label: t("deploy.tab.build"), icon: <Wrench size={13} /> },
          { id: "env", label: t("deploy.tab.env"), icon: <KeyRound size={13} />, badge: tabErrors("env") || undefined },
          { id: "health", label: t("deploy.tab.health"), icon: <HeartPulse size={13} />, badge: tabErrors("health") || undefined },
          { id: "advanced", label: t("deploy.tab.advanced"), icon: <Settings2 size={13} />, badge: tabErrors("advanced") || undefined },
        ]}
      />
      <div className="deploy-editor">
        {tab === "source" && (
          <>
            <div className="form-grid">
              <Field label={t("deploy.f.name")} error={err("name")}>
                <input className="input" value={a.name} onChange={(e) => up({ name: e.target.value })} autoFocus maxLength={64} />
              </Field>
              <Field label={t("deploy.f.stage")}>
                <div className="segmented">
                  {(["production", "staging", "development"] as const).map((s) => (
                    <button key={s} className={a.stage === s ? "on" : ""} onClick={() => up({ stage: s })}>
                      {t(`deploy.stage.${s}`)}
                    </button>
                  ))}
                </div>
              </Field>
            </div>
            <Field label={t("deploy.f.type")} hint={t(`deploy.typeHint.${git ? "git" : a.type === "compose" ? "compose" : "image"}`)}>
              <div className="segmented">
                {typeBtn("git", "deploy.type.git")}
                {typeBtn("compose", "deploy.type.compose")}
                {typeBtn("image", "deploy.type.image")}
              </div>
            </Field>
            <Field label={git ? t("deploy.f.dirGit") : t("deploy.f.dirCompose")} error={err("dir")} hint={t("deploy.f.dirHint")}>
              <input className="input mono" value={a.dir} onChange={(e) => up({ dir: e.target.value })} placeholder="/srv/myapp" spellCheck={false} />
            </Field>
            {git && (
              <>
                <div className="form-grid">
                  <Field label={t("deploy.f.repo")} error={err("repo")} hint={t("deploy.f.repoHint")}>
                    <input className="input mono" value={a.repo} onChange={(e) => up({ repo: e.target.value })} placeholder="https://github.com/org/app.git" spellCheck={false} />
                  </Field>
                  <Field label={t("deploy.f.branch")} error={err("branch")} hint={t("deploy.f.branchHint")}>
                    <input className="input mono" value={a.branch} onChange={(e) => up({ branch: e.target.value })} placeholder="main" spellCheck={false} />
                  </Field>
                </div>
                <div className="form-grid">
                  <Field label={t("deploy.f.token")} error={err("token")} hint={t("deploy.f.tokenHint")}>
                    {token.action === "set" ? (
                      <div className="row deploy-inline">
                        <input className="input mono" type="password" autoComplete="new-password" value={token.value} onChange={(e) => setToken({ action: "set", value: e.target.value })} autoFocus />
                        <button className="btn sm" onClick={() => setToken({ action: "keep", value: "" })}>
                          {t("common.cancel")}
                        </button>
                      </div>
                    ) : (
                      <div className="row deploy-inline">
                        <span className="deploy-secret-val">
                          {token.action === "remove" ? t("deploy.secret.willRemove") : a.hasToken ? "••••••" : t("deploy.secret.notSet")}
                        </span>
                        <button className="btn sm" onClick={() => setToken({ action: "set", value: "" })}>
                          {a.hasToken ? t("deploy.secret.replace") : t("deploy.secret.set")}
                        </button>
                        {a.hasToken && token.action !== "remove" && (
                          <button className="btn sm" onClick={() => setToken({ action: "remove", value: "" })}>
                            {t("deploy.secret.remove")}
                          </button>
                        )}
                        {token.action === "remove" && (
                          <button className="btn sm" onClick={() => setToken({ action: "keep", value: "" })}>
                            <Undo2 size={12} />
                          </button>
                        )}
                      </div>
                    )}
                  </Field>
                  <Field label={t("deploy.f.tokenUser")} error={err("tokenUser")} hint={t("deploy.f.tokenUserHint")}>
                    <input className="input mono" value={a.tokenUser} onChange={(e) => up({ tokenUser: e.target.value })} spellCheck={false} />
                  </Field>
                </div>
                <label className="check">
                  <input type="checkbox" checked={a.submodules} onChange={(e) => up({ submodules: e.target.checked })} /> {t("deploy.f.submodules")}
                </label>
              </>
            )}
            {!git && (
              <div className="form-grid">
                {a.type === "compose" && (
                  <Field label={t("deploy.f.composeFile")} error={err("composeFile")} hint={t("deploy.f.composeFileHint")}>
                    <input className="input mono" value={a.composeFile} onChange={(e) => up({ composeFile: e.target.value })} placeholder="compose.yml" spellCheck={false} />
                  </Field>
                )}
                <Field label={t("deploy.f.project")} error={err("project")} hint={t("deploy.f.projectHint")}>
                  <input className="input mono" value={a.project} onChange={(e) => up({ project: e.target.value })} spellCheck={false} />
                </Field>
                <Field label={t("deploy.f.tagVar")} error={err("tagVar")} hint={a.type === "compose" ? t("deploy.f.tagVarHint", { v: a.tagVar || "APP_TAG" }) : undefined}>
                  <input className="input mono" value={a.tagVar} onChange={(e) => up({ tagVar: e.target.value })} placeholder="APP_TAG" spellCheck={false} />
                </Field>
                <Field label={t("deploy.f.defaultTag")} error={err("defaultTag")}>
                  <input className="input mono" value={a.defaultTag} onChange={(e) => up({ defaultTag: e.target.value })} placeholder="latest" spellCheck={false} />
                </Field>
              </div>
            )}
            {a.type === "image" && (
              <div className="panel-box deploy-imagebox">
                <div className="box-title">
                  <Container size={14} /> {t("deploy.f.imageBox")}
                </div>
                <div className="form-grid">
                  <Field label={t("deploy.f.image")} error={err("image")} hint={t("deploy.f.imageHint")}>
                    <input className="input mono" value={a.image.image} onChange={(e) => upImage({ image: e.target.value })} placeholder="ghcr.io/org/app" spellCheck={false} />
                  </Field>
                  <Field label={t("deploy.f.containerName")} error={err("containerName")}>
                    <input className="input mono" value={a.image.containerName} onChange={(e) => upImage({ containerName: e.target.value })} spellCheck={false} />
                  </Field>
                  <Field label={t("deploy.f.restart")}>
                    <Select mono value={a.image.restart} onChange={(v) => upImage({ restart: v })} options={["unless-stopped", "always", "on-failure", "no"]} />
                  </Field>
                </div>
                <div className="form-grid">
                  <Field label={t("deploy.f.ports")} error={err("ports")} hint={t("deploy.f.portsHint")}>
                    <textarea className="input" rows={3} value={ports} onChange={(e) => setPorts(e.target.value)} placeholder={"8080:80\n127.0.0.1:9000:9000/tcp"} spellCheck={false} />
                  </Field>
                  <Field label={t("deploy.f.volumes")} error={err("volumes")} hint={t("deploy.f.volumesHint")}>
                    <textarea className="input" rows={3} value={volumes} onChange={(e) => setVolumes(e.target.value)} placeholder={"./data:/data\nappdata:/var/lib/app"} spellCheck={false} />
                  </Field>
                </div>
                <Field label={t("deploy.f.command")} error={err("imageHealth")} hint={t("deploy.f.commandHint")}>
                  <input className="input mono" value={a.image.command} onChange={(e) => upImage({ command: e.target.value })} spellCheck={false} />
                </Field>
                <div className="form-grid">
                  <Field label={t("deploy.f.dockerHealth")} hint={t("deploy.f.dockerHealthHint")}>
                    <input className="input mono" value={a.image.healthCmd} onChange={(e) => upImage({ healthCmd: e.target.value })} placeholder="wget -qO- http://localhost:80/ || exit 1" spellCheck={false} />
                  </Field>
                  <Field label={t("deploy.f.dockerHealthTiming")}>
                    <div className="row deploy-inline">
                      <input className="input" type="number" min={0} title={t("deploy.f.interval")} placeholder={t("deploy.f.interval")} value={a.image.healthInterval || ""} onChange={(e) => upImage({ healthInterval: Number(e.target.value) || 0 })} />
                      <input className="input" type="number" min={0} title={t("deploy.f.timeout")} placeholder={t("deploy.f.timeout")} value={a.image.healthTimeout || ""} onChange={(e) => upImage({ healthTimeout: Number(e.target.value) || 0 })} />
                      <input className="input" type="number" min={0} title={t("deploy.f.retries")} placeholder={t("deploy.f.retries")} value={a.image.healthRetries || ""} onChange={(e) => upImage({ healthRetries: Number(e.target.value) || 0 })} />
                    </div>
                  </Field>
                </div>
                <div className="hint">{t("deploy.f.generatedHint", { path: `${a.dir.replace(/\/+$/, "") || "<dir>"}/compose.yml` })}</div>
              </div>
            )}
          </>
        )}

        {tab === "build" && (
          <>
            {!git && <div className="hint-box">{t("deploy.f.composeBuildHint")}</div>}
            <Field label={git ? t("deploy.f.build") : t("deploy.f.preDeploy")} hint={t("deploy.f.scriptHint")} wide>
              <textarea className="input deploy-script" rows={5} value={a.buildCmd} onChange={(e) => up({ buildCmd: e.target.value })} placeholder={git ? "npm ci\nnpm run build" : ""} spellCheck={false} />
            </Field>
            <Field label={t("deploy.f.test")} hint={t("deploy.f.testHint")} wide>
              <textarea className="input deploy-script" rows={3} value={a.testCmd} onChange={(e) => up({ testCmd: e.target.value })} placeholder={git ? "npm test" : ""} spellCheck={false} />
            </Field>
            <Field label={git ? t("deploy.f.restartCmd") : t("deploy.f.postDeploy")} hint={git ? t("deploy.f.restartHint") : t("deploy.f.postDeployHint")} wide>
              <textarea className="input deploy-script" rows={3} value={a.restartCmd} onChange={(e) => up({ restartCmd: e.target.value })} placeholder={git ? "systemctl restart myapp" : ""} spellCheck={false} />
            </Field>
            <label className="check">
              <input type="checkbox" checked={a.restartSudo} disabled={a.sudo} onChange={(e) => up({ restartSudo: e.target.checked })} /> {t("deploy.f.restartSudo")}
            </label>
            <label className="check">
              <input type="checkbox" checked={a.loadEnv} onChange={(e) => up({ loadEnv: e.target.checked })} /> {t("deploy.f.loadEnv")}
            </label>
          </>
        )}

        {tab === "env" && (
          <>
            <Field label={t("deploy.f.envFile")} error={err("envFile")} hint={t("deploy.f.envFileHint", { path: envPath({ ...a, envFile: a.envFile.trim() }) })}>
              <input className="input mono" value={a.envFile} onChange={(e) => up({ envFile: e.target.value })} placeholder=".env" spellCheck={false} />
            </Field>
            <div className="deploy-subhead">{t("deploy.env.vars")}</div>
            {err("vars") && <div className="err deploy-inline-err">{err("vars")}</div>}
            <div className="deploy-kv-list">
              {(a.vars ?? []).map((v, i) => (
                <div className="deploy-kv-row" key={i}>
                  <input
                    className={`input input-sm mono ${v.name && !validEnvName(v.name.trim()) ? "invalid" : ""}`}
                    placeholder="NAME"
                    value={v.name}
                    onChange={(e) => up({ vars: (a.vars ?? []).map((x, j) => (j === i ? { ...x, name: e.target.value } : x)) })}
                    spellCheck={false}
                  />
                  <input
                    className={`input input-sm mono ${!validEnvValue(v.value) ? "invalid" : ""}`}
                    placeholder={t("deploy.env.value")}
                    value={v.value}
                    onChange={(e) => up({ vars: (a.vars ?? []).map((x, j) => (j === i ? { ...x, value: e.target.value } : x)) })}
                    spellCheck={false}
                  />
                  <button className="icon-btn" title={t("common.delete")} onClick={() => up({ vars: (a.vars ?? []).filter((_, j) => j !== i) })}>
                    <Trash2 size={13} />
                  </button>
                </div>
              ))}
              <button className="btn sm ghost deploy-add" onClick={() => up({ vars: [...(a.vars ?? []), { name: "", value: "" }] })}>
                <Plus size={13} /> {t("deploy.env.addVar")}
              </button>
            </div>
            <div className="deploy-subhead">{t("deploy.env.secrets")}</div>
            <div className="hint">{t("deploy.env.secretsHint")}</div>
            {err("secrets") && <div className="err deploy-inline-err">{err("secrets")}</div>}
            <div className="deploy-kv-list">
              {secrets.map((s) => (
                <div className={`deploy-kv-row ${s.state}`} key={s.name}>
                  <span className="mono deploy-secret-name">{s.name}</span>
                  {s.state === "replace" || s.state === "new" ? (
                    <input
                      className="input input-sm mono"
                      type="password"
                      autoComplete="new-password"
                      placeholder={t("deploy.secret.newValue")}
                      value={s.value}
                      onChange={(e) => setSecrets((l) => l.map((x) => (x.name === s.name ? { ...x, value: e.target.value } : x)))}
                    />
                  ) : (
                    <span className="deploy-secret-val">
                      ••••••{" "}
                      <span className={`chip ${s.state === "remove" ? "deploy-chip-rm" : ""}`}>{s.state === "remove" ? t("deploy.secret.willRemove") : t("deploy.secret.isSet")}</span>
                    </span>
                  )}
                  <div className="deploy-kv-actions">
                    {s.state === "set" && (
                      <>
                        <button className="btn sm" onClick={() => setSecrets((l) => l.map((x) => (x.name === s.name ? { ...x, state: "replace", value: "" } : x)))}>
                          {t("deploy.secret.replace")}
                        </button>
                        <button className="btn sm" onClick={() => setSecrets((l) => l.map((x) => (x.name === s.name ? { ...x, state: "remove" } : x)))}>
                          {t("deploy.secret.remove")}
                        </button>
                      </>
                    )}
                    {(s.state === "replace" || s.state === "remove") && (
                      <button className="btn sm" title={t("deploy.secret.undo")} onClick={() => setSecrets((l) => l.map((x) => (x.name === s.name ? { ...x, state: "set", value: "" } : x)))}>
                        <Undo2 size={12} />
                      </button>
                    )}
                    {s.state === "new" && (
                      <button className="icon-btn" title={t("common.delete")} onClick={() => setSecrets((l) => l.filter((x) => x.name !== s.name))}>
                        <Trash2 size={13} />
                      </button>
                    )}
                  </div>
                </div>
              ))}
              <div className="deploy-kv-row">
                <input className="input input-sm mono" placeholder="SECRET_NAME" value={newSecret.name} onChange={(e) => setNewSecret((p) => ({ ...p, name: e.target.value }))} spellCheck={false} />
                <input
                  className="input input-sm mono"
                  type="password"
                  autoComplete="new-password"
                  placeholder={t("deploy.secret.value")}
                  value={newSecret.value}
                  onChange={(e) => setNewSecret((p) => ({ ...p, value: e.target.value }))}
                  onKeyDown={(e) => e.key === "Enter" && addSecret()}
                />
                <button className="btn sm" onClick={addSecret} disabled={!newSecret.name.trim() || !newSecret.value}>
                  <Plus size={13} /> {t("deploy.env.addSecret")}
                </button>
              </div>
            </div>
            <div className="hint-box deploy-format">{t("deploy.env.format")}</div>
          </>
        )}

        {tab === "health" && (
          <>
            <Field label={t("deploy.f.healthType")}>
              <div className="segmented">
                {(["none", "http", "command"] as const).map((ty) => (
                  <button key={ty} className={a.health.type === ty ? "on" : ""} onClick={() => upHealth({ type: ty })}>
                    {t(`deploy.health.${ty}`)}
                  </button>
                ))}
              </div>
            </Field>
            {a.health.type === "http" && (
              <>
                <Field label={t("deploy.f.url")} error={err("url")} hint={t("deploy.f.urlHint")}>
                  <input className="input mono" value={a.health.url} onChange={(e) => upHealth({ url: e.target.value })} placeholder="http://127.0.0.1:3000/health" spellCheck={false} />
                </Field>
                <div className="form-grid">
                  <Field label={t("deploy.f.expectStatus")} error={err("expectStatus")} hint={t("deploy.f.expectStatusHint")}>
                    <input className="input" type="number" value={a.health.expectStatus || ""} onChange={(e) => upHealth({ expectStatus: Number(e.target.value) || 0 })} placeholder="2xx/3xx" />
                  </Field>
                  <Field label={t("deploy.f.bodyContains")}>
                    <input className="input mono" value={a.health.bodyContains} onChange={(e) => upHealth({ bodyContains: e.target.value })} spellCheck={false} />
                  </Field>
                </div>
                <label className="check">
                  <input type="checkbox" checked={a.health.insecure} onChange={(e) => upHealth({ insecure: e.target.checked })} /> {t("deploy.f.insecure")}
                </label>
              </>
            )}
            {a.health.type === "command" && (
              <Field label={t("deploy.f.healthCommand")} error={err("healthCommand")} hint={t("deploy.f.healthCommandHint")} wide>
                <textarea className="input deploy-script" rows={3} value={a.health.command} onChange={(e) => upHealth({ command: e.target.value })} placeholder="systemctl is-active --quiet myapp" spellCheck={false} />
              </Field>
            )}
            {a.health.type !== "none" && (
              <>
                <div className="form-grid">
                  <Field label={t("deploy.f.timeoutS")} error={err("healthNums")}>
                    <input className="input" type="number" min={1} value={a.health.timeout} onChange={(e) => upHealth({ timeout: Number(e.target.value) })} />
                  </Field>
                  <Field label={t("deploy.f.retries")}>
                    <input className="input" type="number" min={1} value={a.health.retries} onChange={(e) => upHealth({ retries: Number(e.target.value) })} />
                  </Field>
                  <Field label={t("deploy.f.intervalS")}>
                    <input className="input" type="number" min={1} value={a.health.interval} onChange={(e) => upHealth({ interval: Number(e.target.value) })} />
                  </Field>
                  <Field label={t("deploy.f.delayS")}>
                    <input className="input" type="number" min={0} value={a.health.delay} onChange={(e) => upHealth({ delay: Number(e.target.value) })} />
                  </Field>
                </div>
                <label className="check">
                  <input type="checkbox" checked={a.autoRollback} onChange={(e) => up({ autoRollback: e.target.checked })} /> {t("deploy.f.autoRollback")}
                </label>
                <div className="hint">{t("deploy.f.autoRollbackHint")}</div>
              </>
            )}
          </>
        )}

        {tab === "advanced" && (
          <>
            <label className="check">
              <input type="checkbox" checked={a.sudo} onChange={(e) => up({ sudo: e.target.checked })} /> {t("deploy.f.sudo")}
            </label>
            <div className="hint deploy-gap">{t("deploy.f.sudoHint")}</div>
            <div className="form-grid">
              <Field label={t("deploy.f.minFree")} error={err("minFreeMB")}>
                <input className="input" type="number" min={0} value={a.minFreeMB} onChange={(e) => up({ minFreeMB: Number(e.target.value) })} />
              </Field>
              <Field label={t("deploy.f.cmdTimeout")} error={err("commandTimeout")}>
                <input className="input" type="number" min={1} value={a.commandTimeout} onChange={(e) => up({ commandTimeout: Number(e.target.value) })} />
              </Field>
            </div>
            <div className="hint-box">{t("deploy.f.lockHint")}</div>
          </>
        )}
      </div>
    </Modal>
  );
}
