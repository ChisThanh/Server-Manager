import { useEffect, useMemo, useState } from "react";
import {
  ArrowDownToLine,
  FileCode2,
  Info,
  Layers,
  Plus,
  Power,
  RefreshCw,
  Repeat,
  RotateCcw,
  Rocket,
  ScrollText,
  Square,
  TerminalSquare,
  Play,
} from "lucide-react";
import type { TermRequest } from "../terminal/TerminalPanel";
import { useRemote } from "../../ui/hooks";
import { Empty, ErrorBox, KV, Loading } from "../../ui/Page";
import { StateBadge, type Tone } from "../../ui/Status";
import { runJob } from "../../ui/jobs";
import { useUI } from "../../store/ui";
import { useT } from "../../i18n";
import { DockerService, POLL_MS, PATH_RE, PROJECT_RE, StateCell, ago, confirmOpts, ds, fullDate, useFilter, type ComposeProject, type ViewProps } from "./common";
import { ComposeEditor, type EditorTarget } from "./ComposeEditor";
import { LogsModal } from "./Containers";
import { InspectModal } from "./Inspect";

type Action = "up" | "pull" | "restart" | "stop" | "start" | "down";

export function projectTone(p: ComposeProject): Tone {
  if (p.unhealthy > 0) return "err";
  if (p.total === 0 || p.running === 0) return "muted";
  if (p.running < p.total) return "warn";
  return "ok";
}

export function ComposeView(p: ViewProps & { composeMissing: boolean }) {
  const t = useT();
  const list = useRemote(() => ds(p.connId, (pw) => DockerService.ComposeProjects(p.connId, pw)), [p.connId], {
    enabled: p.active && !p.composeMissing,
    poll: p.auto ? POLL_MS : undefined,
  });
  const [q, setQ] = useState("");
  const [sel, setSel] = useState<string | null>(null);
  const [editor, setEditor] = useState<EditorTarget | null>(null);
  const projects = useFilter(list.data ?? undefined, q, (r) => [r.name, r.workingDir, ...(r.services ?? [])]);

  useEffect(() => {
    const all = list.data ?? [];
    if (all.length && (!sel || !all.some((x) => x.name === sel))) setSel(all[0].name);
  }, [list.data]);

  const newProject = async () => {
    const r = await useUI.getState().openDialog({
      title: t("docker.p.newTitle"),
      message: t("docker.p.newMsg"),
      confirmText: t("docker.p.newOpen"),
      wide: true,
      fields: [
        { name: "name", label: t("docker.p.name"), placeholder: "my-app", autoFocus: true, validate: (v) => (PROJECT_RE.test(v.trim()) ? null : t("docker.p.nameInvalid")) },
        {
          name: "dir",
          label: t("docker.p.dir"),
          placeholder: "/opt/stacks/my-app",
          validate: (v) => (PATH_RE.test(v.trim()) && v.trim() !== "/" && !v.includes("..") ? null : t("docker.p.dirInvalid")),
        },
        { name: "file", label: t("docker.p.file"), value: "compose.yaml", validate: (v) => (/^[A-Za-z0-9._-]+\.ya?ml$/.test(v.trim()) ? null : t("docker.p.fileInvalid")) },
      ],
    });
    if (r?.action !== "ok") return;
    const dir = String(r.values.dir).trim().replace(/\/+$/, "");
    const path = `${dir}/${String(r.values.file).trim()}`;
    setEditor({ mode: "new", spec: { project: String(r.values.name).trim(), workingDir: dir, files: [path], envFiles: [], path } });
  };

  if (p.composeMissing) {
    return <Empty icon={<Layers size={28} />} title={t("docker.p.missing")} text={t("docker.p.missingHint")} />;
  }
  if (list.error && !list.data) return <ErrorBox error={list.error} onRetry={list.reload} />;
  if (!list.data) return <Loading label={t("common.loading")} />;

  const current = (list.data ?? []).find((x) => x.name === sel) ?? null;

  return (
    <>
      <div className="toolbar">
        <input className="input" placeholder={t("docker.p.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="grow" />
        {list.error && <span className="badge err" title={list.error}>{t("docker.refreshFailed")}</span>}
        <span className="muted">{t("docker.p.count", { n: projects.length })}</span>
        {p.canEdit && (
          <button className="btn primary sm" onClick={newProject}>
            <Plus size={13} /> {t("docker.p.new")}
          </button>
        )}
        <button className="icon-btn" onClick={list.reload} title={t("common.refresh")} disabled={list.loading}>
          <RefreshCw size={14} className={list.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {(list.data ?? []).length === 0 ? (
        <Empty
          icon={<Layers size={28} />}
          title={t("docker.p.none")}
          text={t("docker.p.noneHint")}
          action={
            p.canEdit ? (
              <button className="btn primary" onClick={newProject}>
                <Plus size={13} /> {t("docker.p.new")}
              </button>
            ) : undefined
          }
        />
      ) : (
        <div className="docker-compose">
          <div className="panel-box docker-projects">
            <div className="list-rows">
              {projects.map((x) => (
                <div key={x.name} className={`list-row clickable ${sel === x.name ? "docker-sel" : ""}`} onClick={() => setSel(x.name)}>
                  <StateBadge tone={projectTone(x)}>{`${x.running}/${x.total}`}</StateBadge>
                  <div className="grow">
                    <div className="docker-name">{x.name}</div>
                    <div className="muted docker-sub mono">{x.workingDir || "—"}</div>
                  </div>
                </div>
              ))}
              {projects.length === 0 && <div className="muted docker-empty-small">{t("docker.noMatch")}</div>}
            </div>
          </div>
          <div className="docker-project-detail">
            {current ? (
              <ProjectDetail key={current.name} {...p} project={current} onEdit={setEditor} onChanged={list.reload} />
            ) : (
              <Empty title={t("docker.p.select")} />
            )}
          </div>
        </div>
      )}
      {editor && (
        <ComposeEditor
          connId={p.connId}
          target={editor}
          canEdit={p.canEdit}
          onClose={() => setEditor(null)}
          onDeployed={() => {
            setSel(editor.spec.project);
            list.reload();
          }}
        />
      )}
    </>
  );
}

function ProjectDetail(p: ViewProps & { project: ComposeProject; onEdit: (t: EditorTarget) => void; onChanged: () => void }) {
  const t = useT();
  const pr = p.project;
  const detail = useRemote(() => ds(p.connId, (pw) => DockerService.ComposeProjectDetail(p.connId, pr.name, pw)), [p.connId, pr.name], {
    enabled: p.active,
    poll: p.auto ? POLL_MS : undefined,
  });
  const [logs, setLogs] = useState<string | null>(null);
  const [inspect, setInspect] = useState<string | null>(null);
  const files = pr.configFiles ?? [];
  const containers = detail.data?.containers ?? [];
  const missingServices = useMemo(() => {
    const have = new Set(containers.map((c) => c.service));
    return (detail.data?.defined ?? []).filter((s) => !have.has(s));
  }, [detail.data]);

  const reload = () => {
    detail.reload();
    p.onChanged();
  };

  const action = async (a: Action) => {
    let opts = { pull: false, removeVolumes: false };
    const titles: Record<Action, string> = {
      up: t("docker.p.upTitle", { name: pr.name }),
      pull: t("docker.p.pullTitle", { name: pr.name }),
      restart: t("docker.p.restartTitle", { name: pr.name }),
      stop: t("docker.p.stopTitle", { name: pr.name }),
      start: t("docker.p.startTitle", { name: pr.name }),
      down: t("docker.p.downTitle", { name: pr.name }),
    };
    if (a === "up") {
      const r = await confirmOpts({
        serverId: p.connId,
        title: t("docker.p.upQ", { name: pr.name }),
        message: t("docker.p.upMsg"),
        confirmText: t("docker.p.up"),
        danger: false,
        options: [{ name: "pull", label: t("docker.p.optPull") }],
      });
      if (!r) return;
      opts.pull = r.pull;
    } else if (a === "down") {
      const r = await confirmOpts({
        serverId: p.connId,
        title: t("docker.p.downQ", { name: pr.name }),
        message: t("docker.p.downMsg"),
        confirmText: t("docker.p.down"),
        options: [{ name: "volumes", label: t("docker.p.optVolumes") }],
      });
      if (!r) return;
      opts.removeVolumes = r.volumes;
    } else if (a === "stop" || a === "restart") {
      const r = await confirmOpts({
        serverId: p.connId,
        title: t(a === "stop" ? "docker.p.stopQ" : "docker.p.restartQ", { name: pr.name }),
        message: t("docker.p.affects", { n: pr.total }),
        confirmText: t(a === "stop" ? "docker.p.stop" : "docker.p.restart"),
        danger: a === "stop",
      });
      if (!r) return;
    }
    await runJob(titles[a], () => ds(p.connId, (pw) => DockerService.ComposeAction(p.connId, pr.name, a, opts, pw)));
    reload();
  };

  const recreate = async (name: string) => {
    const r = await confirmOpts({
      serverId: p.connId,
      title: t("docker.c.recreateQ", { name }),
      message: t("docker.c.recreateMsg", { project: pr.name, service: containers.find((c) => c.name === name)?.service ?? "" }),
      confirmText: t("docker.c.recreate"),
      options: [{ name: "pull", label: t("docker.c.optPull") }],
    });
    if (!r) return;
    await runJob(t("docker.c.recreateTitle", { name }), () => ds(p.connId, (pw) => DockerService.RecreateContainer(p.connId, name, r.pull, pw)));
    reload();
  };

  const edit = (path: string) =>
    p.onEdit({ mode: "edit", spec: { project: pr.name, workingDir: pr.workingDir, files, envFiles: pr.envFiles ?? [], path } });

  return (
    <>
      <div className="docker-detail-head">
        <div>
          <h3 className="docker-title">
            <Layers size={16} /> {pr.name}
          </h3>
          <div className="muted docker-sub">{pr.status || "—"}</div>
        </div>
        <div className="grow" />
        {p.canEdit && (
          <div className="docker-btn-row">
            <button className="btn primary sm" onClick={() => action("up")} disabled={files.length === 0} title={t("docker.p.upHint")}>
              <Rocket size={13} /> {t("docker.p.up")}
            </button>
            <button className="btn sm" onClick={() => action("pull")} disabled={files.length === 0}>
              <ArrowDownToLine size={13} /> {t("docker.p.pull")}
            </button>
            <button className="btn sm" onClick={() => action("restart")}>
              <RotateCcw size={13} /> {t("docker.p.restart")}
            </button>
            {pr.running > 0 ? (
              <button className="btn sm" onClick={() => action("stop")}>
                <Square size={12} /> {t("docker.p.stop")}
              </button>
            ) : (
              <button className="btn sm" onClick={() => action("start")}>
                <Play size={13} /> {t("docker.p.start")}
              </button>
            )}
            <button className="btn danger sm" onClick={() => action("down")}>
              <Power size={13} /> {t("docker.p.down")}
            </button>
          </div>
        )}
      </div>

      <div className="panel-box docker-files">
        <KV
          items={[
            [t("docker.p.workdir"), <span className="mono">{pr.workingDir || "—"}</span>],
            [
              t("docker.p.files"),
              files.length === 0 ? (
                <span className="muted">{t("docker.p.noFiles")}</span>
              ) : (
                <div className="docker-file-list">
                  {files.map((f) => (
                    <div key={f} className="docker-file">
                      <span className="mono">{f}</span>
                      <button className="btn sm ghost" onClick={() => edit(f)}>
                        <FileCode2 size={13} /> {p.canEdit ? t("docker.p.editFile") : t("docker.p.viewFile")}
                      </button>
                    </div>
                  ))}
                </div>
              ),
            ],
            ...((pr.envFiles ?? []).length ? ([[t("docker.p.envFiles"), <span className="mono">{(pr.envFiles ?? []).join(", ")}</span>]] as [string, JSX.Element][]) : []),
          ]}
        />
        {detail.data?.configError && <div className="hint-box err docker-pre">{detail.data.configError}</div>}
      </div>

      <div className="section-title">{t("docker.p.containers")}</div>
      {detail.error && !detail.data ? (
        <ErrorBox error={detail.error} onRetry={detail.reload} />
      ) : !detail.data ? (
        <Loading />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("docker.p.service")}</th>
                <th>{t("docker.col.name")}</th>
                <th>{t("docker.col.state")}</th>
                <th>{t("docker.col.image")}</th>
                <th>{t("docker.col.ports")}</th>
                <th>{t("docker.col.created")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {containers.map((c) => (
                <tr key={c.name}>
                  <td>
                    <span className="chip">{c.service}</span>
                  </td>
                  <td className="mono">{c.name}</td>
                  <td title={c.status}>
                    <StateCell state={c.state} health={c.health} />
                  </td>
                  <td className="mono docker-image" title={c.image}>
                    {c.image}
                  </td>
                  <td className="mono docker-ports" title={c.ports}>
                    {c.ports || <span className="muted">—</span>}
                  </td>
                  <td className="muted" title={fullDate(c.created)}>
                    {ago(c.created)}
                  </td>
                  <td className="docker-actions">
                    <div className="docker-btns">
                      <button className="icon-btn" title={t("docker.c.logs")} onClick={() => setLogs(c.name)}>
                        <ScrollText size={13} />
                      </button>
                      {p.canShell && (
                        <button
                          className="icon-btn"
                          title={t("docker.c.shell")}
                          disabled={c.state !== "running"}
                          onClick={() => p.navigate("terminal", { cwd: "", nonce: Date.now(), exec: { kind: "docker", target: c.name, title: c.name } } satisfies TermRequest)}
                        >
                          <TerminalSquare size={13} />
                        </button>
                      )}
                      {p.canEdit && files.length > 0 && (
                        <button className="icon-btn" title={t("docker.c.recreate")} onClick={() => recreate(c.name)}>
                          <Repeat size={13} />
                        </button>
                      )}
                      <button className="icon-btn" title={t("docker.c.inspect")} onClick={() => setInspect(c.name)}>
                        <Info size={13} />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
              {missingServices.map((s) => (
                <tr key={`missing-${s}`}>
                  <td>
                    <span className="chip">{s}</span>
                  </td>
                  <td className="muted" colSpan={6}>
                    {t("docker.p.notCreated")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {containers.length === 0 && missingServices.length === 0 && <div className="docker-nomatch muted">{t("docker.p.noContainers")}</div>}
        </div>
      )}
      {logs && <LogsModal connId={p.connId} name={logs} onClose={() => setLogs(null)} onOpenPanel={() => p.navigate("logs", { source: "docker", container: logs })} />}
      {inspect && <InspectModal connId={p.connId} name={inspect} onClose={() => setInspect(null)} />}
    </>
  );
}
