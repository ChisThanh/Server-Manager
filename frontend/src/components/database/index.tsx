import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Activity as ActivityIcon,
  Archive,
  Code2,
  Database,
  Gauge,
  HardDrive,
  Pencil,
  Plus,
  RefreshCw,
  ScanSearch,
  ServerCrash,
  SquareTerminal,
  Timer,
  Trash2,
} from "lucide-react";
import type { PanelProps } from "../../ui/types";
import { Empty, ErrorBox, Loading, Page, PageHeader } from "../../ui/Page";
import { SubTabs, type TabItem } from "../../ui/Tabs";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { confirmDanger } from "../../ui/confirm";
import { errMsg } from "../../lib/api";
import { toast } from "../../store/ui";
import { useT, type Key } from "../../i18n";
import type { TermRequest } from "../terminal/TerminalPanel";
import {
  DatabaseService,
  EngineIcon,
  POLL_MS,
  consoleExec,
  dbs,
  engineName,
  type Detection,
  type Target,
  type TargetViewProps,
} from "./common";
import { OverviewView } from "./Overview";
import { QueryRunner } from "./QueryRunner";
import { ActivityView } from "./Activity";
import { RedisSlowlog, SlowView } from "./Slow";
import { DatabasesView } from "./Databases";
import { RedisConsole } from "./RedisConsole";
import { TargetDialog } from "./TargetDialog";
import "./database.css";

type Tab =
  | "overview"
  | "query"
  | "activity"
  | "slow"
  | "databases"
  | "console"
  | "slowlog";
const SQL_TABS: Tab[] = ["overview", "query", "activity", "slow", "databases"];
const REDIS_TABS: Tab[] = ["overview", "console", "slowlog"];

const LAST_KEY = "sm.db.target.";

function lastTarget(connId: string): string {
  try {
    return localStorage.getItem(LAST_KEY + connId) ?? "";
  } catch {
    return "";
  }
}

export default function DatabasePanel({
  connId,
  visible,
  connected,
  navigate,
  arg,
}: PanelProps) {
  const t = useT();
  const canEdit = useCan(connId, "database");
  const canShell = useCan(connId, "terminal");
  const [targets, setTargets] = useState<Target[] | null>(null);
  const [selId, setSelId] = useState(() => lastTarget(connId));
  const [dialog, setDialog] = useState<{ target?: Target } | null>(null);
  const [wantTab, setWantTab] = useState<{
    tab: Tab;
    db?: string;
    n: number;
  } | null>(null);
  const det = useRemote(() => DatabaseService.Detect(connId, ""), [connId], {
    enabled: visible && connected,
  });

  useEffect(() => {
    if (det.data) setTargets(det.data.targets ?? []);
  }, [det.data]);

  // navigate("database", { target, tab }) from other modules.
  useEffect(() => {
    const a = arg as { target?: string; tab?: string } | undefined;
    if (a?.target) setSelId(a.target);
    if (a?.tab) setWantTab({ tab: a.tab as Tab, n: Date.now() });
  }, [arg]);

  const list = targets ?? [];
  const sel = list.find((x) => x.id === selId) ?? list[0];

  useEffect(() => {
    if (!sel) return;
    try {
      localStorage.setItem(LAST_KEY + connId, sel.id);
    } catch {
      /* ignore */
    }
  }, [sel?.id]);

  const reloadTargets = async () => {
    try {
      setTargets((await DatabaseService.Targets(connId)) ?? []);
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const scanDockerSudo = async () => {
    try {
      const d = await dbs(connId, (pw) =>
        DatabaseService.DetectSudo(connId, pw),
      );
      det.setData(d);
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const remove = async (tg: Target) => {
    const ok = await confirmDanger({
      serverId: connId,
      title: tg.detected
        ? t("db.tg.resetQ", { name: tg.name })
        : t("db.tg.deleteQ", { name: tg.name }),
      message: tg.detected ? t("db.tg.resetMsg") : t("db.tg.deleteMsg"),
      confirmText: tg.detected ? t("db.tg.reset") : t("common.delete"),
    });
    if (!ok) return;
    try {
      await DatabaseService.DeleteTarget(connId, tg.id);
      await reloadTargets();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const d = det.data;
  const containers = useMemo(
    () =>
      (d?.targets ?? [])
        .filter((x) => x.mode === "docker")
        .map((x) => x.container),
    [d],
  );

  const header = (
    <PageHeader
      icon={<Database size={20} />}
      title={t("db.title")}
      sub={d ? <DetectSummary d={d} /> : t("db.detecting")}
      actions={
        <div className="row">
          {canEdit && (
            <button className="btn sm" onClick={() => setDialog({})}>
              <Plus size={13} /> {t("db.tg.add")}
            </button>
          )}
          <button
            className="icon-btn"
            onClick={det.reload}
            title={t("db.rescan")}
            disabled={det.loading}
          >
            <RefreshCw size={14} className={det.loading ? "spin-icon" : ""} />
          </button>
        </div>
      }
    />
  );

  let body;
  if (!connected && !targets)
    body = (
      <Empty icon={<ServerCrash size={32} />} title={t("db.notConnected")} />
    );
  else if (det.error && !targets)
    body = <ErrorBox error={det.error} onRetry={det.reload} />;
  else if (!targets) body = <Loading label={t("db.detecting")} />;
  else if (list.length === 0)
    body = (
      <Empty
        icon={<Database size={32} />}
        title={t("db.noneFound")}
        text={t("db.noneFoundHint")}
        action={
          <div className="row">
            {d?.docker === "denied" && (
              <button className="btn" onClick={scanDockerSudo}>
                <ScanSearch size={13} /> {t("db.scanDockerSudo")}
              </button>
            )}
            {canEdit && (
              <button className="btn primary" onClick={() => setDialog({})}>
                <Plus size={13} /> {t("db.tg.add")}
              </button>
            )}
          </div>
        }
      />
    );
  else
    body = (
      <div className="db-layout">
        <div className="db-targets">
          {list.map((tg) => (
            <button
              key={tg.id}
              className={`db-target ${sel?.id === tg.id ? "on" : ""}`}
              onClick={() => setSelId(tg.id)}
            >
              <EngineIcon engine={tg.engine} />
              <div className="db-target-text">
                <div className="db-target-name">{tg.name}</div>
                <div className="db-target-sub">
                  {engineName(tg)} {tg.version && <span>{tg.version}</span>}
                  {tg.mode === "docker" && (
                    <span className="badge">docker</span>
                  )}
                  {tg.auth === "password" && (
                    <span className="badge">
                      {tg.user || t("db.tg.password")}
                    </span>
                  )}
                </div>
              </div>
              <span
                className={`ui-dot standalone ${tg.running ? "ok" : tg.detected ? "err" : "muted"}`}
              />
            </button>
          ))}
          {d?.docker === "denied" && (
            <button
              className="btn ghost sm db-scan-sudo"
              onClick={scanDockerSudo}
              title={t("db.dockerDeniedHint")}
            >
              <ScanSearch size={13} /> {t("db.scanDockerSudo")}
            </button>
          )}
        </div>
        <div className="db-main">
          {sel && (
            <TargetPane
              key={sel.id}
              connId={connId}
              target={sel}
              active={visible && connected}
              canEdit={canEdit}
              canShell={canShell}
              navigate={navigate}
              wantTab={wantTab}
              onEdit={() => setDialog({ target: sel })}
              onDelete={() => remove(sel)}
            />
          )}
        </div>
      </div>
    );

  return (
    <Page className="db-page">
      {header}
      {body}
      {dialog && (
        <TargetDialog
          connId={connId}
          target={dialog.target}
          containers={containers}
          onClose={() => setDialog(null)}
          onSaved={async (tg) => {
            setDialog(null);
            await reloadTargets();
            setSelId(tg.id);
            toast(t("db.tg.saved"), "success");
          }}
        />
      )}
    </Page>
  );
}

function DetectSummary({ d }: { d: Detection }) {
  const t = useT();
  const running = (d.targets ?? []).filter((x) => x.running).length;
  return (
    <>
      {t("db.summary", { n: (d.targets ?? []).length, running })}
      {d.docker === "denied" && (
        <span className="muted"> · {t("db.dockerDenied")}</span>
      )}
    </>
  );
}

function TargetPane(
  props: Omit<TargetViewProps, "active"> & {
    active: boolean;
    wantTab: { tab: Tab; db?: string; n: number } | null;
    onEdit: () => void;
    onDelete: () => void;
  },
) {
  const { connId, target, active, canEdit, canShell, navigate } = props;
  const t = useT();
  const redis = target.engine === "redis";
  const tabs = redis ? REDIS_TABS : SQL_TABS;
  const [tab, setTab] = useState<Tab>("overview");
  const [queryDb, setQueryDb] = useState<{ db: string; n: number } | null>(
    null,
  );

  useEffect(() => {
    if (props.wantTab && tabs.includes(props.wantTab.tab))
      setTab(props.wantTab.tab);
  }, [props.wantTab?.n]);

  const ovActive = active && (tab === "overview" || tab === "slowlog");
  const ov = useRemote(
    () => dbs(connId, (pw) => DatabaseService.Overview(connId, target.id, pw)),
    [connId, target.id],
    {
      enabled: ovActive,
      poll: POLL_MS,
    },
  );

  const viewProps: TargetViewProps = {
    connId,
    target,
    active,
    canEdit,
    canShell,
    navigate,
  };
  const exec = consoleExec(target, target.database);
  const labels: Record<Tab, Key> = {
    overview: "db.tab.overview",
    query: "db.tab.query",
    activity: "db.tab.activity",
    slow: "db.tab.slow",
    databases: "db.tab.databases",
    console: "db.tab.console",
    slowlog: "db.tab.slowlog",
  };
  const icons: Record<Tab, ReactNode> = {
    overview: <Gauge size={13} />,
    query: <Code2 size={13} />,
    activity: <ActivityIcon size={13} />,
    slow: <Timer size={13} />,
    databases: <HardDrive size={13} />,
    console: <SquareTerminal size={13} />,
    slowlog: <Timer size={13} />,
  };
  const items: TabItem<Tab>[] = tabs.map((id) => ({
    id,
    label: t(labels[id]),
    icon: icons[id],
    badge:
      id === "slowlog"
        ? (ov.data?.redis?.slowlog ?? []).length || undefined
        : undefined,
  }));

  return (
    <>
      <div className="db-target-head">
        <div className="page-icon db-head-ic">
          <EngineIcon engine={target.engine} size={17} />
        </div>
        <div className="db-target-title">
          <div>
            <b>{target.name}</b>{" "}
            <StateBadge
              state={
                target.running
                  ? "running"
                  : target.detected
                    ? "stopped"
                    : "unknown"
              }
            >
              {target.running
                ? t("db.running")
                : target.detected
                  ? t("db.stopped")
                  : t("db.custom")}
            </StateBadge>
          </div>
          <div className="muted db-target-meta">
            {engineName(target)}{" "}
            {ov.data?.version ? ov.data.version.split(" ")[0] : target.version}
            {" · "}
            {target.mode === "docker"
              ? t("db.inContainer", { name: target.container })
              : t("db.onHost")}
            {" · "}
            {target.auth === "peer"
              ? t("db.tg.peerShort")
              : `${target.user || "default"}@${target.host || "local"}${target.port ? ":" + target.port : ""}`}
          </div>
        </div>
        <div className="grow" />
        {canShell && exec && (
          <button
            className="btn sm"
            onClick={() =>
              navigate("terminal", {
                cwd: "",
                nonce: Date.now(),
                exec,
              } satisfies TermRequest)
            }
          >
            <SquareTerminal size={13} /> {t("db.console")}
          </button>
        )}
        {!redis && (
          <button
            className="btn sm"
            onClick={() =>
              navigate("backup", {
                engine: target.engine,
                database: target.database,
                target: target.id,
              })
            }
          >
            <Archive size={13} /> {t("db.backupNow")}
          </button>
        )}
        {canEdit && (
          <button
            className="icon-btn"
            onClick={props.onEdit}
            title={t("db.tg.editBtn")}
          >
            <Pencil size={14} />
          </button>
        )}
        {canEdit && target.saved && (
          <button
            className="icon-btn"
            onClick={props.onDelete}
            title={target.detected ? t("db.tg.reset") : t("common.delete")}
          >
            <Trash2 size={14} />
          </button>
        )}
      </div>
      <SubTabs items={items} value={tab} onChange={setTab} />
      <div className="db-tab-body">
        {tab === "overview" &&
          (ov.error && !ov.data ? (
            <ErrorBox
              error={ov.error}
              onRetry={ov.reload}
              actions={
                canEdit && (
                  <button className="btn" onClick={props.onEdit}>
                    <Pencil size={13} /> {t("db.fixConnection")}
                  </button>
                )
              }
            />
          ) : !ov.data ? (
            <Loading />
          ) : (
            <OverviewView ov={ov.data} onGoSlow={() => setTab("slow")} />
          ))}
        {tab === "query" && (
          <QueryRunner
            {...viewProps}
            key={queryDb?.n ?? 0}
            initialDb={queryDb?.db}
          />
        )}
        {tab === "activity" && <ActivityView {...viewProps} />}
        {tab === "slow" && <SlowView {...viewProps} />}
        {tab === "databases" && (
          <DatabasesView
            {...viewProps}
            onQuery={(db) => {
              setQueryDb({ db, n: Date.now() });
              setTab("query");
            }}
          />
        )}
        {tab === "console" && (
          <RedisConsole
            {...viewProps}
            dbCount={(ov.data?.redis?.keyspace ?? []).reduce(
              (m, k) => Math.max(m, k.db + 1),
              16,
            )}
          />
        )}
        {tab === "slowlog" &&
          (ov.error && !ov.data ? (
            <ErrorBox error={ov.error} onRetry={ov.reload} />
          ) : !ov.data?.redis ? (
            <Loading />
          ) : (
            <RedisSlowlog
              entries={ov.data.redis.slowlog ?? []}
              error={ov.data.redis.slowlogError}
            />
          ))}
      </div>
    </>
  );
}
