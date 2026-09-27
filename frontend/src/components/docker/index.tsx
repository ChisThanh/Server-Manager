import { useEffect, useState } from "react";
import { Box, Container as ContainerIcon, Disc, HardDrive, Layers, Network, PieChart, RefreshCw, ServerCrash, ShieldAlert } from "lucide-react";
import type { PanelProps } from "../../ui/types";
import { Empty, ErrorBox, Loading, Page, PageHeader } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { useRemote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { Modal } from "../Overlays";
import { formatBytes } from "../../lib/format";
import { useT } from "../../i18n";
import { DockerService, ds, type Status } from "./common";
import { ContainersView } from "./Containers";
import { ComposeView } from "./Compose";
import { ImagesView, NetworksView, VolumesView } from "./Resources";
import "./docker.css";

type Tab = "containers" | "compose" | "images" | "volumes" | "networks";
const TABS: Tab[] = ["containers", "compose", "images", "volumes", "networks"];

const AUTO_KEY = "sm.docker.auto";

function initialAuto(): boolean {
  try {
    return localStorage.getItem(AUTO_KEY) !== "0";
  } catch {
    return true;
  }
}

export default function DockerPanel({ connId, visible, connected, navigate, arg }: PanelProps) {
  const t = useT();
  const canEdit = useCan(connId, "docker");
  const canShell = useCan(connId, "terminal");
  const [tab, setTab] = useState<Tab>("containers");
  const [auto, setAuto] = useState(initialAuto);
  const [df, setDf] = useState(false);
  const status = useRemote(() => ds(connId, (pw) => DockerService.Status(connId, pw)), [connId], { enabled: visible && connected });

  // navigate("docker", { tab: "compose" }) from other modules.
  useEffect(() => {
    const want = (arg as { tab?: string } | undefined)?.tab;
    if (want && (TABS as string[]).includes(want)) setTab(want as Tab);
  }, [arg]);

  const toggleAuto = (v: boolean) => {
    setAuto(v);
    try {
      localStorage.setItem(AUTO_KEY, v ? "1" : "0");
    } catch {
      /* ignore */
    }
  };

  const st = status.data;
  const ready = !!st && st.installed && st.daemonRunning;
  const view = { connId, auto, canEdit, canShell, navigate };

  return (
    <Page className="docker-page">
      <PageHeader
        icon={<ContainerIcon size={20} />}
        title="Docker"
        sub={st ? <StatusLine st={st} /> : t("docker.detecting")}
        actions={
          <div className="docker-btn-row">
            {ready && (
              <button className="btn sm" onClick={() => setDf(true)}>
                <PieChart size={13} /> {t("docker.df.button")}
              </button>
            )}
            <button className="icon-btn" onClick={status.reload} title={t("docker.recheck")} disabled={status.loading}>
              <RefreshCw size={14} className={status.loading ? "spin-icon" : ""} />
            </button>
          </div>
        }
      />
      {!connected && !st ? (
        <Empty icon={<ServerCrash size={32} />} title={t("docker.notConnected")} />
      ) : status.error && !st ? (
        <ErrorBox error={status.error} onRetry={status.reload} />
      ) : !st ? (
        <Loading label={t("docker.detecting")} />
      ) : !st.installed ? (
        <Empty
          icon={<Box size={32} />}
          title={t("docker.notInstalled")}
          text={
            <div className="docker-explain">
              <p>{t("docker.notInstalledHint")}</p>
              <pre className="docker-pre mono">curl -fsSL https://get.docker.com | sudo sh</pre>
              <p className="muted">{t("docker.notInstalledHint2")}</p>
            </div>
          }
          action={
            <button className="btn" onClick={status.reload}>
              {t("docker.recheck")}
            </button>
          }
        />
      ) : !st.daemonRunning ? (
        <Empty
          icon={<ServerCrash size={32} />}
          title={t("docker.daemonDown")}
          text={
            <div className="docker-explain">
              <p>{t("docker.daemonDownHint")}</p>
              <pre className="docker-pre mono">sudo systemctl start docker{"\n"}sudo systemctl status docker</pre>
              {st.daemonError && <pre className="docker-pre mono muted">{st.daemonError}</pre>}
            </div>
          }
          action={
            <button className="btn" onClick={status.reload}>
              {t("docker.recheck")}
            </button>
          }
        />
      ) : (
        <>
          <SubTabs<Tab>
            value={tab}
            onChange={setTab}
            items={[
              { id: "containers", label: t("docker.tab.containers"), icon: <Box size={14} /> },
              { id: "compose", label: t("docker.tab.compose"), icon: <Layers size={14} /> },
              { id: "images", label: t("docker.tab.images"), icon: <Disc size={14} /> },
              { id: "volumes", label: t("docker.tab.volumes"), icon: <HardDrive size={14} /> },
              { id: "networks", label: t("docker.tab.networks"), icon: <Network size={14} /> },
            ]}
            right={
              <label className="check docker-auto" title={t("docker.autoHint")}>
                <input type="checkbox" checked={auto} onChange={(e) => toggleAuto(e.target.checked)} /> {t("docker.auto")}
              </label>
            }
          />
          {!canEdit && (
            <div className="hint-box docker-readonly">
              <ShieldAlert size={14} /> {t("docker.readOnly")}
            </div>
          )}
          <div className="docker-view">
            {tab === "containers" && <ContainersView {...view} active={visible && connected} />}
            {tab === "compose" && <ComposeView {...view} active={visible && connected} composeMissing={!st.compose} />}
            {tab === "images" && <ImagesView {...view} active={visible && connected} />}
            {tab === "volumes" && <VolumesView {...view} active={visible && connected} />}
            {tab === "networks" && <NetworksView {...view} active={visible && connected} />}
          </div>
        </>
      )}
      {df && <DiskUsageModal connId={connId} onClose={() => setDf(false)} />}
    </Page>
  );
}

function StatusLine({ st }: { st: Status }) {
  const t = useT();
  if (!st.installed) return <>{t("docker.notInstalled")}</>;
  return (
    <span className="docker-status-line">
      <span>Docker {st.serverVersion || st.clientVersion}</span>
      {st.compose ? (
        <span>
          · Compose {st.composeVersion} <span className="muted">({st.compose === "plugin" ? "docker compose" : "docker-compose"})</span>
        </span>
      ) : (
        <span className="muted">· {t("docker.p.missing")}</span>
      )}
      {st.needSudo && (
        <span className="badge warn" title={t("docker.sudoHint")}>
          sudo
        </span>
      )}
      {!st.daemonRunning && <span className="badge err">{t("docker.daemonDown")}</span>}
    </span>
  );
}

function DiskUsageModal({ connId, onClose }: { connId: string; onClose: () => void }) {
  const t = useT();
  const df = useRemote(() => ds(connId, (pw) => DockerService.DiskUsage(connId, pw)), [connId]);
  const rows = df.data ?? [];
  const total = rows.reduce((s, r) => s + r.size, 0);
  const reclaim = rows.reduce((s, r) => s + r.reclaimable, 0);
  const label = (type: string) => {
    switch (type) {
      case "Images":
        return t("docker.tab.images");
      case "Containers":
        return t("docker.tab.containers");
      case "Local Volumes":
        return t("docker.tab.volumes");
      case "Build Cache":
        return t("docker.df.buildCache");
    }
    return type;
  };
  return (
    <Modal
      title={
        <>
          <PieChart size={16} /> {t("docker.df.title")}
        </>
      }
      size="wide"
      onClose={onClose}
      footer={
        <button className="btn primary" onClick={onClose}>
          {t("common.close")}
        </button>
      }
    >
      {df.error ? (
        <ErrorBox error={df.error} onRetry={df.reload} />
      ) : !df.data ? (
        <Loading />
      ) : (
        <>
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>{t("docker.df.type")}</th>
                  <th className="num">{t("docker.df.total")}</th>
                  <th className="num">{t("docker.df.active")}</th>
                  <th className="num">{t("docker.col.size")}</th>
                  <th className="num">{t("docker.df.reclaimable")}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.type}>
                    <td>{label(r.type)}</td>
                    <td className="num">{r.total}</td>
                    <td className="num">{r.active}</td>
                    <td className="num">{formatBytes(r.size)}</td>
                    <td className="num">
                      {formatBytes(r.reclaimable)}
                      {r.size > 0 && <span className="muted"> ({Math.round((r.reclaimable / r.size) * 100)}%)</span>}
                    </td>
                  </tr>
                ))}
                <tr className="docker-total-row">
                  <td>{t("docker.df.sum")}</td>
                  <td />
                  <td />
                  <td className="num">{formatBytes(total)}</td>
                  <td className="num">{formatBytes(reclaim)}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p className="muted docker-df-hint">{t("docker.df.hint")}</p>
        </>
      )}
    </Modal>
  );
}
