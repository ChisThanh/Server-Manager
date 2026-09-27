import { useMemo, useState } from "react";
import { Bell, Check, Copy, Globe, LayoutGrid, PanelLeftClose, PanelLeftOpen, Pencil, Plug, PlugZap, Plus, ScrollText, Search, Settings, TerminalSquare, Trash2, Unplug } from "lucide-react";
import { useAlertCount } from "../store/alerts";
import { EnvBadge } from "./EnvBadge";
import type { Key } from "../i18n";

const GLOBAL_PAGES: { id: string; label: Key; icon: React.ReactNode }[] = [
  { id: "fleet", label: "app.page.fleet", icon: <LayoutGrid size={15} /> },
  { id: "alerts", label: "app.page.alerts", icon: <Bell size={15} /> },
  { id: "command", label: "app.page.command", icon: <TerminalSquare size={15} /> },
  { id: "audit", label: "app.page.audit", icon: <ScrollText size={15} /> },
  { id: "settings", label: "app.page.settings", icon: <Settings size={15} /> },
];
import { useApp } from "../store/app";
import { usePrefs } from "../store/prefs";
import { confirmDialog, toast, useUI } from "../store/ui";
import { ServerService, errMsg, type Server } from "../lib/api";
import { sidebarShortcut } from "../lib/platform";
import { LANGUAGES, useLang, useT } from "../i18n";
import { ServerDialog, blankServer } from "./ServerDialog";
import { Select } from "../ui/Select";

export function Avatar({ server, status, size }: { server: Server; status?: string; size?: number }) {
  const letters = (server.name || server.host)
    .split(/[\s._-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]!.toUpperCase())
    .join("");
  return (
    <div className="avatar" style={{ background: server.color || "#4f8cff", ...(size ? { width: size, height: size } : {}) }}>
      {letters || "?"}
      {status && <span className={`dot ${status}`} />}
    </div>
  );
}

export function Sidebar() {
  const t = useT();
  const servers = useApp((s) => s.servers);
  const conns = useApp((s) => s.conns);
  const active = useApp((s) => s.active);
  const open = useApp((s) => s.open);
  const collapsed = usePrefs((s) => s.sidebarCollapsed);
  const toggle = usePrefs((s) => s.toggleSidebar);
  const lang = useLang((s) => s.lang);
  const setLang = useLang((s) => s.setLang);
  const [q, setQ] = useState("");
  const [editing, setEditing] = useState<Server | null>(null);
  const showMenu = useUI((s) => s.showMenu);
  const page = useApp((s) => s.page);
  const alertCount = useAlertCount();

  const groups = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = servers.filter((s) => !needle || [s.name, s.host, s.user, s.group].some((v) => v.toLowerCase().includes(needle)));
    const map = new Map<string, Server[]>();
    for (const s of list) {
      const g = s.group || "";
      map.set(g, [...(map.get(g) ?? []), s]);
    }
    return Array.from(map.entries()).sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : a.localeCompare(b)));
  }, [servers, q]);

  const statusOf = (id: string) => (open.includes(id) ? conns[id]?.status : undefined);

  const onOpen = (s: Server) => {
    const st = useApp.getState();
    if (st.open.includes(s.id) && st.conns[s.id]?.status !== "disconnected") st.setActive(s.id);
    else st.connect(s.id);
  };

  const duplicate = async (s: Server) => {
    try {
      await ServerService.Save({ ...s, id: "", name: t("sidebar.copyName", { name: s.name }), hasSecret: false }, "", false);
      await useApp.getState().loadServers();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  const remove = async (s: Server) => {
    if (await confirmDialog(t("sidebar.deleteTitle"), t("sidebar.deleteMsg", { name: s.name }), t("common.delete"), true)) {
      await useApp.getState().deleteServer(s.id);
    }
  };

  const menuFor = (e: React.MouseEvent, s: Server) => {
    e.preventDefault();
    showMenu(e.clientX, e.clientY, [
      statusOf(s.id) === "connected"
        ? { label: t("sidebar.disconnect"), icon: <Unplug />, onClick: () => useApp.getState().closeWorkspace(s.id) }
        : { label: t("sidebar.connect"), icon: <Plug />, onClick: () => useApp.getState().connect(s.id) },
      { label: t("common.edit"), icon: <Pencil />, onClick: () => setEditing(s) },
      { label: t("sidebar.duplicate"), icon: <Copy />, onClick: () => duplicate(s) },
      {
        label: t("sidebar.copySsh"),
        icon: <Copy />,
        onClick: () => {
          navigator.clipboard.writeText(`ssh ${s.port !== 22 ? `-p ${s.port} ` : ""}${s.user}@${s.host}`);
          toast(t("common.copied"));
        },
      },
      { separator: true },
      { label: t("common.delete"), icon: <Trash2 />, danger: true, onClick: () => remove(s) },
    ]);
  };

  const langMenu = (e: React.MouseEvent) => {
    const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
    showMenu(
      r.right + 6,
      r.top,
      LANGUAGES.map((l) => ({
        label: l.name,
        icon: l.code === lang ? <Check /> : <span style={{ width: 14 }} />,
        onClick: () => setLang(l.code),
      })),
    );
  };

  const collapseTip = `${collapsed ? t("sidebar.expand") : t("sidebar.collapse")} (${sidebarShortcut})`;

  if (collapsed) {
    return (
      <aside className="sidebar rail">
        <div className="sidebar-head" />
        <div className="rail-nav">
          {GLOBAL_PAGES.map((g) => (
            <button key={g.id} className={`icon-btn ${page === g.id ? "active" : ""}`} title={t(g.label)} onClick={() => useApp.getState().setPage(g.id)}>
              {g.icon}
              {g.id === "alerts" && alertCount > 0 && <span className="nav-count">{alertCount}</span>}
            </button>
          ))}
        </div>
        <div className="rail-list">
          {servers.map((s) => (
            <button
              key={s.id}
              className={`rail-item ${active === s.id && !page ? "active" : ""}`}
              onClick={() => onOpen(s)}
              onContextMenu={(e) => menuFor(e, s)}
              title={`${s.name}\n${s.user}@${s.host}:${s.port}`}
            >
              <Avatar server={s} status={statusOf(s.id)} size={30} />
            </button>
          ))}
          <button className="icon-btn rail-add" title={t("app.addServer")} onClick={() => setEditing(blankServer())}>
            <Plus size={16} />
          </button>
        </div>
        <div className="rail-foot">
          <button className="icon-btn" title={t("sidebar.language")} onClick={langMenu}>
            <Globe size={16} />
          </button>
          <button className="icon-btn" title={collapseTip} onClick={toggle}>
            <PanelLeftOpen size={16} />
          </button>
        </div>
        {editing && <ServerDialog server={editing} onClose={() => setEditing(null)} />}
      </aside>
    );
  }

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span className="title">{t("sidebar.title")}</span>
        <button className="icon-btn" title={t("app.addServer")} onClick={() => setEditing(blankServer())}>
          <Plus size={16} />
        </button>
      </div>
      <div className="nav-list">
        {GLOBAL_PAGES.map((g) => (
          <button key={g.id} className={`nav-item ${page === g.id ? "active" : ""}`} onClick={() => useApp.getState().setPage(g.id)}>
            {g.icon}
            <span className="grow">{t(g.label)}</span>
            {g.id === "alerts" && alertCount > 0 && <span className="nav-count">{alertCount}</span>}
          </button>
        ))}
      </div>
      <div className="sidebar-search">
        <div style={{ position: "relative" }}>
          <Search size={13} style={{ position: "absolute", left: 9, top: 8.5, color: "var(--fg-3)" }} />
          <input className="input input-sm" style={{ paddingLeft: 28 }} placeholder={t("sidebar.search")} value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
      </div>
      <div className="server-list">
        {servers.length === 0 && (
          <div className="empty" style={{ padding: "30px 10px" }}>
            <p>{t("sidebar.empty")}</p>
            <button className="btn primary sm" onClick={() => setEditing(blankServer())}>
              <Plus size={14} /> {t("app.addServer")}
            </button>
          </div>
        )}
        {groups.map(([g, list]) => (
          <div key={g || "_"}>
            {(g || groups.length > 1) && <div className="group-label">{g || t("sidebar.other")}</div>}
            {list.map((s) => {
              const st = statusOf(s.id);
              return (
                <div
                  key={s.id}
                  className={`server-item ${active === s.id && !page ? "active" : ""}`}
                  onClick={() => onOpen(s)}
                  onContextMenu={(e) => menuFor(e, s)}
                  title={`${s.user}@${s.host}:${s.port}`}
                >
                  <Avatar server={s} status={st} />
                  <div className="meta">
                    <div className="name">
                      {s.name} <EnvBadge env={s.environment} short />
                    </div>
                    <div className="sub">
                      {s.user}@{s.host}
                      {s.port !== 22 ? `:${s.port}` : ""}
                    </div>
                  </div>
                  <div className="actions">
                    {st === "connected" ? (
                      <button
                        className="icon-btn"
                        title={t("sidebar.disconnect")}
                        onClick={(e) => {
                          e.stopPropagation();
                          useApp.getState().closeWorkspace(s.id);
                        }}
                      >
                        <Unplug size={14} />
                      </button>
                    ) : (
                      <button
                        className="icon-btn"
                        title={t("sidebar.connect")}
                        onClick={(e) => {
                          e.stopPropagation();
                          useApp.getState().connect(s.id);
                        }}
                      >
                        <PlugZap size={14} />
                      </button>
                    )}
                    <button
                      className="icon-btn"
                      title={t("common.edit")}
                      onClick={(e) => {
                        e.stopPropagation();
                        setEditing(s);
                      }}
                    >
                      <Pencil size={14} />
                    </button>
                  </div>
                </div>
              );
            })}
          </div>
        ))}
      </div>
      <div className="sidebar-foot">
        <Globe size={14} className="muted" />
        <Select ghost className="lang-select" value={lang} onChange={setLang} title={t("sidebar.language")} options={LANGUAGES.map((l) => ({ value: l.code, label: l.name }))} />
        <button className="icon-btn" title={collapseTip} onClick={toggle}>
          <PanelLeftClose size={16} />
        </button>
      </div>
      {editing && <ServerDialog server={editing} onClose={() => setEditing(null)} />}
    </aside>
  );
}
