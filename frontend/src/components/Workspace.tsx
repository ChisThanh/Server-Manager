import { memo, Suspense, useCallback, useEffect, useMemo, useRef, useState, type ComponentType } from "react";
import { PanelLeftClose, PanelLeftOpen, RefreshCw, Unplug } from "lucide-react";
import { useApp } from "../store/app";
import { usePrefs } from "../store/prefs";
import { isMac } from "../lib/platform";
import { useT } from "../i18n";
import { MODULES } from "./modules";
import { WsContext } from "./builtinPanels";
import { EnvBadge } from "./EnvBadge";

export function Workspace({ id, visible }: { id: string; visible: boolean }) {
  const t = useT();
  const server = useApp((s) => s.servers.find((x) => x.id === id));
  const conn = useApp((s) => s.conns[id]);
  const navCollapsed = usePrefs((s) => s.wsNavCollapsed);
  const [panel, setPanel] = useState("overview");
  const [args, setArgs] = useState<Record<string, unknown>>({});
  // Panels are mounted on first visit and then kept alive (state, terminals).
  const [mounted, setMounted] = useState<Set<string>>(() => new Set(["overview"]));
  const home = conn?.home || "/";
  const [root, setRoot] = useState<string | null>(null);
  const connected = conn?.status === "connected";
  const everConnected = useRef(false);
  if (connected) everConnected.current = true;

  // Stable, so memoized panels don't re-render when the workspace does.
  const go = useCallback((p: string, arg?: unknown) => {
    setPanel(p);
    setMounted((m) => (m.has(p) ? m : new Set(m).add(p)));
    if (arg !== undefined) setArgs((a) => ({ ...a, [p]: arg }));
  }, []);

  // The "connected" status event can arrive before Connect() returns the home
  // directory, so wait for it rather than falling back to "/".
  useEffect(() => {
    if (connected && conn?.home && root === null) setRoot(server?.defaultPath || conn.home);
  }, [connected, conn?.home]);

  // ⌘/Ctrl + 1..9 switches panels.
  useEffect(() => {
    if (!visible) return;
    const onKey = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || e.shiftKey || e.altKey) return;
      const n = Number(e.key);
      if (n >= 1 && n <= 9 && MODULES[n - 1]) {
        e.preventDefault();
        go(MODULES[n - 1].id);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [visible]);

  const ws = useMemo(() => ({ home, root, setRoot }), [home, root]);

  if (!server) return null;

  return (
    <div className="workspace" style={{ display: visible ? "flex" : "none" }}>
      <div className="ws-header">
        <span className="ws-title">{server.name}</span>
        <EnvBadge env={server.environment} />
        {server.role && server.role !== "admin" && <span className="ui-badge muted">{t(`app.role.${server.role}` as never)}</span>}
        <span className="muted mono host">
          {server.user}@{server.host}
          {server.port !== 22 ? `:${server.port}` : ""}
        </span>
        <div className="spacer" />
        <button className="icon-btn" title={t("ws.disconnect")} onClick={() => useApp.getState().closeWorkspace(id)}>
          <Unplug size={15} />
        </button>
      </div>

      {conn?.status !== "connected" && (
        <div className={`conn-banner ${conn?.status === "connecting" ? "connecting" : ""}`}>
          {conn?.status === "connecting" ? <span className="spinner" /> : <Unplug size={15} />}
          <span className="grow">
            {conn?.status === "connecting" ? t("ws.connecting") : `${t("ws.lost")}${conn?.message ? `: ${conn.message}` : ""}`}
          </span>
          {conn?.status !== "connecting" && (
            <button className="btn sm" onClick={() => useApp.getState().reconnect(id)}>
              <RefreshCw size={13} /> {t("ws.reconnect")}
            </button>
          )}
        </div>
      )}

      {!everConnected.current ? (
        <div className="empty">
          {conn?.status === "connecting" ? <span className="spinner lg" /> : <Unplug size={32} />}
          <p>{conn?.status === "connecting" ? t("ws.connectingTo", { name: server.name }) : conn?.message || t("ws.notConnected")}</p>
        </div>
      ) : (
        <div className="ws-main">
          <nav className={`ws-side ${navCollapsed ? "collapsed" : ""}`}>
            {MODULES.map((m, i) => (
              <div key={m.id} style={{ display: "contents" }}>
                {i > 0 && MODULES[i - 1].group !== m.group && <div className="ws-side-sep" />}
                <button
                  className={`ws-side-btn ${panel === m.id ? "active" : ""}`}
                  onClick={() => go(m.id)}
                  title={`${t(m.label)}${i < 9 ? ` (${isMac ? "⌘" : "Ctrl+"}${i + 1})` : ""}`}
                >
                  {m.icon}
                  <span className="label">{t(m.label)}</span>
                </button>
              </div>
            ))}
            <div className="grow" />
            <button className="ws-side-btn" onClick={() => usePrefs.getState().toggleWsNav()} title={navCollapsed ? t("common.expand") : t("common.collapse")}>
              {navCollapsed ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}
              <span className="label">{t("common.collapse")}</span>
            </button>
          </nav>
          <WsContext.Provider value={ws}>
            <div className="ws-body">
              {MODULES.filter((m) => mounted.has(m.id)).map((m) => {
                return (
                  <div key={m.id} className={`panel ${panel === m.id ? "" : "hidden"}`}>
                    <Suspense fallback={<div className="center-msg"><span className="spinner lg" /></div>}>
                      <PanelHost C={m.component} connId={id} visible={visible && panel === m.id} connected={connected} navigate={go} arg={args[m.id]} />
                    </Suspense>
                  </div>
                );
              })}
            </div>
          </WsContext.Provider>
        </div>
      )}
    </div>
  );
}

type PanelProps = { connId: string; visible: boolean; connected: boolean; navigate: (panel: string, arg?: unknown) => void; arg?: unknown };

// A panel re-renders only when its own props change — not every time the
// workspace does (connection status, server list refreshes…). Hidden panels
// therefore stay idle.
const PanelHost = memo(function PanelHost({ C, ...props }: PanelProps & { C: ComponentType<PanelProps> }) {
  return <C {...props} />;
});
