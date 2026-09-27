/** Props every per-server module panel receives. */
export interface PanelProps {
  connId: string;
  /** The panel is on screen (poll only while visible). */
  visible: boolean;
  /** The SSH connection is up. */
  connected: boolean;
  /** Switch the workspace to another panel (e.g. open logs of a service). */
  navigate: (panel: string, arg?: unknown) => void;
  /** Argument passed by the last navigate() to this panel. */
  arg?: unknown;
}
