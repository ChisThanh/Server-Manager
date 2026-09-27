import { lazy, type ComponentType, type ReactNode } from "react";
import {
  Activity,
  Archive,
  Boxes,
  Database,
  FolderTree,
  Gauge,
  Globe,
  ListTree,
  Rocket,
  ScrollText,
  Server as ServerIcon,
  Shield,
  SquareTerminal,
} from "lucide-react";
import type { Key } from "../i18n";
import type { PanelProps } from "../ui/types";

export interface ModuleDef {
  id: string;
  label: Key;
  icon: ReactNode;
  group: number;
  component: ComponentType<PanelProps>;
}

// Heavy panels load on first use.
const OverviewPanel = lazy(() => import("./overview"));
const MonitoringPanel = lazy(() => import("./monitoring"));
const LogsPanel = lazy(() => import("./logs"));
const DockerPanel = lazy(() => import("./docker"));
const WebPanel = lazy(() => import("./web"));
const DatabasePanel = lazy(() => import("./database"));
const DeployPanel = lazy(() => import("./deploy"));
const BackupPanel = lazy(() => import("./backup"));
const SecurityPanel = lazy(() => import("./security"));

import { ProcessesPanel, ServicesPanel, TerminalPanelWrap } from "./builtinPanels";
const FilesPanel = lazy(() => import("./files/FilesPanel"));

/** Per-server panels, in nav order. Groups are separated in the nav. */
export const MODULES: ModuleDef[] = [
  { id: "overview", label: "app.nav.overview", icon: <Gauge size={15} />, group: 0, component: OverviewPanel },
  { id: "monitor", label: "nav.monitor", icon: <Activity size={15} />, group: 0, component: MonitoringPanel },
  { id: "logs", label: "app.nav.logs", icon: <ScrollText size={15} />, group: 0, component: LogsPanel },
  { id: "files", label: "nav.files", icon: <FolderTree size={15} />, group: 1, component: FilesPanel },
  { id: "terminal", label: "nav.terminal", icon: <SquareTerminal size={15} />, group: 1, component: TerminalPanelWrap },
  { id: "services", label: "nav.services", icon: <ServerIcon size={15} />, group: 2, component: ServicesPanel },
  { id: "processes", label: "nav.processes", icon: <ListTree size={15} />, group: 2, component: ProcessesPanel },
  { id: "docker", label: "app.nav.docker", icon: <Boxes size={15} />, group: 2, component: DockerPanel },
  { id: "web", label: "app.nav.web", icon: <Globe size={15} />, group: 3, component: WebPanel },
  { id: "database", label: "app.nav.database", icon: <Database size={15} />, group: 3, component: DatabasePanel },
  { id: "deploy", label: "app.nav.deploy", icon: <Rocket size={15} />, group: 3, component: DeployPanel },
  { id: "backup", label: "app.nav.backup", icon: <Archive size={15} />, group: 3, component: BackupPanel },
  { id: "security", label: "app.nav.security", icon: <Shield size={15} />, group: 4, component: SecurityPanel },
];
