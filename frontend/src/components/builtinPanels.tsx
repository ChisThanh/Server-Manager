import { createContext, useContext } from "react";
import type { PanelProps } from "../ui/types";
import { TerminalPanel, type TermRequest } from "./terminal/TerminalPanel";
import { Processes } from "./system/Processes";
import { Services } from "./system/Services";

/** Workspace-wide state shared by the built-in panels. */
export interface WsState {
  home: string;
  root: string | null;
  setRoot: (r: string) => void;
}
export const WsContext = createContext<WsState>({ home: "/", root: null, setRoot: () => {} });

export function TerminalPanelWrap(p: PanelProps) {
  const ws = useContext(WsContext);
  return <TerminalPanel connId={p.connId} visible={p.visible} connected={p.connected} request={p.arg as TermRequest | undefined} defaultCwd={ws.root ?? ws.home} />;
}

export function ServicesPanel(p: PanelProps) {
  return <Services connId={p.connId} visible={p.visible} connected={p.connected} navigate={p.navigate} />;
}

export function ProcessesPanel(p: PanelProps) {
  return <Processes connId={p.connId} visible={p.visible} connected={p.connected} />;
}
