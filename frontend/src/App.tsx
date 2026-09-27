import { Suspense, lazy, useEffect, useState } from "react";
import { Events } from "@wailsio/runtime";
import { Plus, X } from "lucide-react";
import logoUrl from "./assets/logo.svg";
import { useApp } from "./store/app";
import { toast } from "./store/ui";
import { FileService, errMsg } from "./lib/api";
import { Avatar, Sidebar } from "./components/Sidebar";
import { Workspace } from "./components/Workspace";
import { ContextMenu, DialogHost, Toasts, Transfers } from "./components/Overlays";
import { ServerDialog, blankServer } from "./components/ServerDialog";
import { usePrefs } from "./store/prefs";
import { isSidebarShortcut } from "./lib/platform";
import { useT } from "./i18n";
import { useSettings } from "./store/settings";
import { JobModals } from "./ui/jobs";
import { startMonitorFeed } from "./store/monitor";

export const PAGES: Record<string, React.LazyExoticComponent<() => React.JSX.Element>> = {
  fleet: lazy(() => import("./components/pages/Fleet")),
  alerts: lazy(() => import("./components/pages/Alerts")),
  command: lazy(() => import("./components/pages/CommandCenter")),
  audit: lazy(() => import("./components/pages/Audit")),
  settings: lazy(() => import("./components/pages/Settings")),
};

export default function App() {
  const t = useT();
  const collapsed = usePrefs((s) => s.sidebarCollapsed);
  const servers = useApp((s) => s.servers);
  const open = useApp((s) => s.open);
  const active = useApp((s) => s.active);
  const conns = useApp((s) => s.conns);
  const page = useApp((s) => s.page);
  const [adding, setAdding] = useState(false);

  useEffect(() => {
    useApp.getState().loadServers();
    useSettings.getState().load();
    startMonitorFeed();
    // Files dragged in from the OS: upload into the folder under the cursor.
    const off = Events.On("files:dropped", async (ev) => {
      const { files, target } = ev.data;
      const connId = target?.["data-conn-id"];
      const dir = target?.["data-remote-dir"];
      if (!connId || !dir || !files?.length) return;
      try {
        await FileService.Upload(connId, files, dir);
      } catch (e) {
        toast(errMsg(e), "error");
      }
    });
    // Block the webview's default "navigate to dropped file" behaviour.
    const prevent = (e: DragEvent) => {
      if (e.dataTransfer?.types.includes("Files")) e.preventDefault();
    };
    window.addEventListener("dragover", prevent);
    window.addEventListener("drop", prevent);
    const noCtx = (e: MouseEvent) => {
      const t = e.target as HTMLElement;
      if (!t.closest("input, textarea, .monaco-editor, .xterm")) e.preventDefault();
    };
    window.addEventListener("contextmenu", noCtx);
    const onKey = (e: KeyboardEvent) => {
      if (isSidebarShortcut(e)) {
        e.preventDefault();
        usePrefs.getState().toggleSidebar();
      }
    };
    // Capture phase so the editor/terminal can't swallow the shortcut.
    window.addEventListener("keydown", onKey, true);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      off();
      window.removeEventListener("dragover", prevent);
      window.removeEventListener("drop", prevent);
      window.removeEventListener("contextmenu", noCtx);
    };
  }, []);

  const recent = [...servers].sort((a, b) => b.lastUsed - a.lastUsed).slice(0, 8);

  return (
    <div className={`app ${collapsed ? "collapsed" : ""}`} style={{ "--sidebar-w": collapsed ? "56px" : "260px" } as React.CSSProperties}>
      <Sidebar />
      <main className="main">
        <div className="topbar">
          {open.map((id) => {
            const s = servers.find((x) => x.id === id);
            if (!s) return null;
            return (
              <div key={id} className={`ws-tab ${active === id && !page ? "active" : ""}`} onClick={() => useApp.getState().setActive(id)} title={`${s.user}@${s.host}`}>
                <span className={`dot ${conns[id]?.status ?? "disconnected"}`} />
                <span className="label">{s.name}</span>
                <button
                  className="icon-btn"
                  style={{ width: 18, height: 18 }}
                  onClick={(e) => {
                    e.stopPropagation();
                    useApp.getState().closeWorkspace(id);
                  }}
                  title={t("app.closeWorkspace")}
                >
                  <X size={12} />
                </button>
              </div>
            );
          })}
        </div>

        {open.map((id) => (
          <Workspace key={id} id={id} visible={active === id && !page} />
        ))}

        {page && PAGES[page] && (
          <div className="page-host">
            <Suspense fallback={<div className="center-msg"><span className="spinner lg" /></div>}>
              {(() => {
                const P = PAGES[page];
                return <P />;
              })()}
            </Suspense>
          </div>
        )}

        {!page && (!active || !open.includes(active)) && (
          <div className="empty">
            <img className="app-logo" src={logoUrl} alt="" width={88} height={88} draggable={false} />
            <h2>Server Manager</h2>
            <p>{t("app.tagline")}</p>
            {recent.length > 0 ? (
              <div className="welcome-grid">
                {recent.map((s) => (
                  <div key={s.id} className="welcome-card" onClick={() => useApp.getState().connect(s.id)}>
                    <Avatar server={s} />
                    <div style={{ minWidth: 0 }}>
                      <div style={{ fontWeight: 500, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{s.name}</div>
                      <div className="muted" style={{ fontSize: 11 }}>
                        {s.user}@{s.host}
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            ) : null}
            <button className="btn primary" style={{ marginTop: 12 }} onClick={() => setAdding(true)}>
              <Plus size={15} /> {t("app.addServer")}
            </button>
          </div>
        )}
      </main>

      {adding && <ServerDialog server={blankServer()} onClose={() => setAdding(false)} onSaved={(s) => useApp.getState().connect(s.id)} />}
      <Transfers />
      <JobModals />
      <ContextMenu />
      <DialogHost />
      <Toasts />
    </div>
  );
}
