import { useEffect, useRef, useState } from "react";
import { Archive, History as HistoryIcon, ListChecks, RefreshCw, Warehouse } from "lucide-react";
import { Events } from "@wailsio/runtime";
import type { PanelProps } from "../../ui/types";
import { Page, PageHeader } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { useRemote } from "../../ui/hooks";
import { useCan } from "../../ui/perm";
import { useT } from "../../i18n";
import { BackupService, type Job } from "./common";
import { JobsView } from "./Jobs";
import { HistoryView } from "./History";
import { DestinationsView } from "./Destinations";
import { JobEditor } from "./JobEditor";
import "./backup.css";

type Tab = "jobs" | "history" | "destinations";

interface NavArg {
  engine?: "postgres" | "mysql";
  database?: string;
  tab?: Tab;
}

export default function BackupPanel({ connId, visible, connected, arg }: PanelProps) {
  const t = useT();
  const canBackup = useCan(connId, "backup");
  const canRestore = useCan(connId, "restore");
  const [tab, setTab] = useState<Tab>("jobs");
  const [editing, setEditing] = useState<{ job: Partial<Job> | null } | null>(null);
  const [tick, setTick] = useState(0);
  const enabled = visible && connected;

  const jobs = useRemote(async () => (await BackupService.ListJobs(connId)) ?? [], [connId], { enabled });
  const status = useRemote(async () => (await BackupService.Status(connId)) ?? [], [connId, tick], { enabled, poll: 15000 });
  const dests = useRemote(async () => (await BackupService.ListDestinations()) ?? [], [tick], { enabled });

  // Refresh when a run of this server starts or ends.
  const reloadRef = useRef(() => {});
  reloadRef.current = () => {
    status.reload();
    setTick((x) => x + 1);
  };
  useEffect(() => {
    const off = Events.On("backup:run", (ev) => {
      if (ev.data?.server === connId) reloadRef.current();
    });
    return () => off();
  }, [connId]);

  // navigate("backup", { engine: "postgres", database: "app" }) from the
  // database module opens a prefilled new-job form.
  const lastArg = useRef<unknown>(undefined);
  useEffect(() => {
    if (!arg || arg === lastArg.current) return;
    lastArg.current = arg;
    const a = arg as NavArg;
    if (a.tab) setTab(a.tab);
    if (a.engine === "postgres" || a.engine === "mysql") {
      setTab("jobs");
      if (canBackup) {
        setEditing({
          job: {
            type: a.engine,
            database: a.database ?? "",
            name: a.database ? `${a.database} (${a.engine === "postgres" ? "PostgreSQL" : "MySQL"})` : "",
          },
        });
      }
    }
  }, [arg, canBackup]);

  const reloadAll = () => {
    jobs.reload();
    status.reload();
    dests.reload();
    setTick((x) => x + 1);
  };

  const runningCount = (status.data ?? []).filter((s) => s.running).length;

  return (
    <Page className="backup-page">
      <PageHeader
        icon={<Archive size={20} />}
        title={t("backup.title")}
        sub={t("backup.subtitle")}
        actions={
          <button className="icon-btn" onClick={reloadAll} title={t("backup.refresh")} disabled={jobs.loading}>
            <RefreshCw size={14} className={jobs.loading || status.loading ? "spin-icon" : ""} />
          </button>
        }
      />
      <SubTabs<Tab>
        value={tab}
        onChange={setTab}
        items={[
          {
            id: "jobs",
            label: t("backup.tab.jobs"),
            icon: <ListChecks size={14} />,
            badge: runningCount || undefined,
          },
          {
            id: "history",
            label: t("backup.tab.history"),
            icon: <HistoryIcon size={14} />,
          },
          {
            id: "destinations",
            label: t("backup.tab.destinations"),
            icon: <Warehouse size={14} />,
          },
        ]}
      />
      {tab === "jobs" && (
        <JobsView
          connId={connId}
          jobs={jobs}
          status={status.data ?? []}
          dests={dests.data ?? []}
          canBackup={canBackup}
          canRestore={canRestore}
          onEdit={(job) => setEditing({ job })}
          onChanged={reloadAll}
          goDestinations={() => setTab("destinations")}
        />
      )}
      {tab === "history" && <HistoryView connId={connId} jobs={jobs.data ?? []} enabled={enabled} tick={tick} />}
      {tab === "destinations" && <DestinationsView connId={connId} dests={dests} jobs={jobs.data ?? []} onChanged={reloadAll} />}
      {editing && (
        <JobEditor
          connId={connId}
          initial={editing.job}
          dests={dests.data ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reloadAll();
          }}
          goDestinations={() => {
            setEditing(null);
            setTab("destinations");
          }}
        />
      )}
    </Page>
  );
}
