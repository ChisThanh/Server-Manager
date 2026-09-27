import { useEffect, useMemo, useState } from "react";
import { Events } from "@wailsio/runtime";
import { Boxes, Container, Eye, GitBranch, History as HistoryIcon, Pencil, Plus, RefreshCw, Rocket, RotateCcw, Search } from "lucide-react";
import { DeployService } from "../../../bindings/server-manager/services/deploy";
import type { AppView, DeployStart, Run } from "../../../bindings/server-manager/services/deploy/models";
import type { PanelProps } from "../../ui/types";
import { Empty, ErrorBox, Loading, Page, PageHeader, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { errMsg } from "../../lib/api";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import { AppEditor } from "./AppEditor";
import { DeployDialog, LiveDeploy, type DeployMode } from "./DeployDialog";
import { History } from "./History";
import { fmtAgo, fmtTime, previousTarget, stageLabel, statusLabel, statusTone, typeLabel } from "./util";
import "./deploy.css";

function TypeIcon({ type }: { type: string }) {
  if (type === "git") return <GitBranch size={15} />;
  if (type === "image") return <Container size={15} />;
  return <Boxes size={15} />;
}

function sourceText(v: AppView): string {
  const a = v.app;
  if (a.type === "git") {
    const repo = a.repo.replace(/\.git$/, "").split(/[/:]/).slice(-2).join("/");
    return `${repo} @ ${a.branch}`;
  }
  if (a.type === "image") return a.image.image;
  return a.composeFile;
}

export default function DeployPanel({ connId, visible, connected }: PanelProps) {
  const t = useT();
  const canDeploy = useCan(connId, "deploy");
  const [editor, setEditor] = useState<AppView | "new" | null>(null);
  const [dialog, setDialog] = useState<{ view: AppView; mode: DeployMode } | null>(null);
  const [live, setLive] = useState<{ view: AppView; start: DeployStart } | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const [q, setQ] = useState("");
  const [anyActive, setAnyActive] = useState(false);

  const apps = useRemote(
    async () => {
      const list = (await DeployService.Apps(connId)) ?? [];
      setAnyActive(list.some((v) => v.active));
      return list;
    },
    [connId],
    { enabled: visible, poll: anyActive ? 3000 : undefined },
  );
  const refresh = () => {
    apps.reload();
    setReloadKey((k) => k + 1);
  };

  // Deployments started elsewhere (another panel instance, the job list) show up here too.
  useEffect(() => {
    if (!visible) return;
    const off = Events.On("deploy:step", (ev) => {
      const e = ev.data;
      if (e.server !== connId) return;
      if ((e.step === "preflight" && e.state === "running") || (e.step === "done" && (e.state === "ok" || e.state === "failed"))) refresh();
    });
    return () => off();
  }, [connId, visible]);

  const list = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return (apps.data ?? []).filter((v) => !needle || [v.app.name, v.app.repo, v.app.dir, v.app.image.image, v.current?.version].some((x) => (x ?? "").toLowerCase().includes(needle)));
  }, [apps.data, q]);

  const sel = (apps.data ?? []).find((v) => v.app.id === selected) ?? null;

  const openRollback = async (v: AppView) => {
    try {
      const runs = (await DeployService.Runs(connId, v.app.id, 200)) ?? [];
      const target = previousTarget(runs, v.app.type);
      if (!target) {
        toast(t("deploy.noPrevious"), "error");
        return;
      }
      setDialog({ view: v, mode: { kind: "rollback", target } });
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const openLive = async (v: AppView) => {
    if (v.active) setLive({ view: v, start: v.active });
  };

  const header = (
    <PageHeader
      icon={<Rocket size={18} />}
      title={t("deploy.title")}
      sub={t("deploy.sub")}
      actions={
        <>
          <button className="icon-btn" title={t("common.refresh")} onClick={refresh}>
            <RefreshCw size={15} className={apps.loading ? "spin-icon" : ""} />
          </button>
          {canDeploy && (
            <button className="btn primary" onClick={() => setEditor("new")}>
              <Plus size={14} /> {t("deploy.newApp")}
            </button>
          )}
        </>
      }
    />
  );

  let body;
  if (apps.error && !apps.data) body = <ErrorBox error={apps.error} onRetry={apps.reload} />;
  else if (!apps.data) body = <Loading />;
  else if (apps.data.length === 0)
    body = (
      <Empty
        icon={<Rocket size={28} />}
        title={t("deploy.empty")}
        text={t("deploy.emptyHint")}
        action={
          canDeploy && (
            <button className="btn primary" onClick={() => setEditor("new")}>
              <Plus size={14} /> {t("deploy.newApp")}
            </button>
          )
        }
      />
    );
  else
    body = (
      <>
        {apps.data.length > 6 && (
          <div className="toolbar">
            <div className="deploy-search">
              <Search size={13} />
              <input className="input input-sm" placeholder={t("deploy.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
            </div>
          </div>
        )}
        {!connected && <div className="hint-box deploy-gap">{t("deploy.offline")}</div>}
        <div className="deploy-cards">
          {list.map((v) => (
            <AppCard
              key={v.app.id}
              v={v}
              selected={v.app.id === selected}
              canDeploy={canDeploy}
              connected={connected}
              onSelect={() => setSelected(v.app.id === selected ? null : v.app.id)}
              onDeploy={() => setDialog({ view: v, mode: { kind: "deploy" } })}
              onRollback={() => openRollback(v)}
              onEdit={() => setEditor(v)}
              onView={() => openLive(v)}
            />
          ))}
        </div>
        {list.length === 0 && <Empty title={t("deploy.noMatch")} />}
        {sel && (
          <Section title={t("deploy.historyOf", { name: sel.app.name })} icon={<HistoryIcon size={14} />}>
            <History
              connId={connId}
              app={sel.app}
              currentTarget={sel.current?.data.target ?? ""}
              canDeploy={canDeploy && connected}
              reloadKey={reloadKey}
              onRedeploy={(r: Run) => setDialog({ view: sel, mode: { kind: "deploy", redeployOf: r } })}
              onRollback={(r: Run) => setDialog({ view: sel, mode: { kind: "rollback", target: r } })}
            />
          </Section>
        )}
      </>
    );

  return (
    <Page className="deploy-page">
      {header}
      {body}
      {editor && (
        <AppEditor
          connId={connId}
          initial={editor === "new" ? null : editor}
          onClose={() => setEditor(null)}
          onSaved={(v) => {
            setEditor(null);
            if (v) setSelected(v.app.id);
            refresh();
          }}
        />
      )}
      {dialog && <DeployDialog connId={connId} app={dialog.view.app} mode={dialog.mode} onClose={() => (setDialog(null), refresh())} onChanged={refresh} />}
      {live && <LiveDeploy connId={connId} app={live.view.app} start={live.start} onClose={() => (setLive(null), refresh())} onChanged={refresh} />}
    </Page>
  );
}

function AppCard(props: {
  v: AppView;
  selected: boolean;
  canDeploy: boolean;
  connected: boolean;
  onSelect: () => void;
  onDeploy: () => void;
  onRollback: () => void;
  onEdit: () => void;
  onView: () => void;
}) {
  const t = useT();
  const { v } = props;
  const a = v.app;
  const cur = v.current;
  const last = v.last;
  const busy = !!v.active;
  const lastDiffers = last && cur && last.id !== cur.id;
  return (
    <div className={`deploy-card ${props.selected ? "selected" : ""}`} onClick={props.onSelect}>
      <div className="deploy-card-head">
        <TypeIcon type={a.type} />
        <span className="deploy-card-name" title={a.name}>
          {a.name}
        </span>
        <span className={`deploy-stage ${a.stage}`}>{stageLabel(a.stage)}</span>
      </div>
      <div className="deploy-card-src muted" title={a.type === "git" ? a.repo : a.dir}>
        {typeLabel(a.type)} · <span className="mono">{sourceText(v)}</span>
      </div>
      <div className="deploy-card-ver">
        <div className="deploy-k">{t("deploy.card.current")}</div>
        <div className="deploy-v mono" title={cur?.data.target}>
          {cur?.version || "—"}
        </div>
        <div className="deploy-s muted">
          {cur ? (
            <span title={fmtTime(cur.finished || cur.started)}>{t("deploy.card.deployedBy", { ago: fmtAgo(cur.finished || cur.started), actor: cur.actor })}</span>
          ) : (
            t("deploy.card.never")
          )}
        </div>
        {cur?.data.commit?.subject && <div className="deploy-s deploy-subject">{cur.data.commit.subject}</div>}
      </div>
      <div className="deploy-card-last">
        {busy ? (
          <>
            <span className="spinner" /> <span>{t("deploy.card.running")}</span>
            <button
              className="btn sm ghost"
              onClick={(e) => {
                e.stopPropagation();
                props.onView();
              }}
            >
              <Eye size={13} /> {t("deploy.card.view")}
            </button>
          </>
        ) : last ? (
          <>
            <StateBadge tone={statusTone(last.status)}>{statusLabel(last.status)}</StateBadge>
            <span className="muted" title={fmtTime(last.started)}>
              {lastDiffers ? <span className="mono">{last.version} · </span> : null}
              {fmtAgo(last.started)}
            </span>
          </>
        ) : (
          <span className="muted">{t("deploy.card.noRuns")}</span>
        )}
      </div>
      <div className="deploy-card-actions" onClick={(e) => e.stopPropagation()}>
        {props.canDeploy && (
          <>
            <button className="btn sm primary" disabled={busy || !props.connected} onClick={props.onDeploy}>
              <Rocket size={13} /> {t("deploy.deploy")}
            </button>
            <button className="btn sm" disabled={busy || !props.connected || !cur} onClick={props.onRollback}>
              <RotateCcw size={13} /> {t("deploy.rollback")}
            </button>
          </>
        )}
        <div className="grow" />
        <button className={`icon-btn ${props.selected ? "active" : ""}`} title={t("deploy.history")} onClick={props.onSelect}>
          <HistoryIcon size={14} />
        </button>
        {props.canDeploy && (
          <button className="icon-btn" title={t("common.edit")} onClick={props.onEdit}>
            <Pencil size={14} />
          </button>
        )}
      </div>
    </div>
  );
}
