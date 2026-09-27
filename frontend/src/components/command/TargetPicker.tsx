import { useMemo } from "react";
import { CheckSquare, ChevronDown, Lock, Plug, Search, Square, Wand2, X } from "lucide-react";
import type { Target } from "../../../bindings/server-manager/services/command";
import { EnvBadge } from "../EnvBadge";
import { useUI } from "../../store/ui";
import { useT } from "../../i18n";
import { useCmd, type Filters } from "./store";
import { Select } from "../../ui/Select";

const ENVS = ["production", "staging", "development"] as const;

export function matches(tg: Target, f: Filters): boolean {
  const q = f.q.trim().toLowerCase();
  if (q && ![tg.name, tg.host, tg.user, tg.group, ...(tg.tags ?? [])].some((v) => v.toLowerCase().includes(q))) return false;
  if (f.group === "\u0000" ? tg.group !== "" : f.group && tg.group !== f.group) return false;
  if (f.env === "none" ? tg.environment !== "" : f.env && tg.environment !== f.env) return false;
  if (f.tag && !(tg.tags ?? []).includes(f.tag)) return false;
  return true;
}

/** Whether the target can run commands at all. */
export const runnable = (tg: Target) => tg.canExec && (tg.readiness === "connected" || tg.readiness === "auto");

export function ReadinessBadge({ tg }: { tg: Target }) {
  const t = useT();
  if (!tg.canExec) {
    return (
      <span className="badge err" title={t("cmd.ready.blockedHint", { role: tg.role })}>
        <Lock size={10} /> {t("cmd.ready.blocked")}
      </span>
    );
  }
  switch (tg.readiness) {
    case "connected":
      return (
        <span className="badge ok" title={t(tg.via === "ui" ? "cmd.via.ui" : "cmd.via.background")}>
          <Plug size={10} /> {t("cmd.ready.connected")}
        </span>
      );
    case "auto":
      return (
        <span className="badge" title={t(`cmd.via.${tg.via}` as "cmd.via.password")}>
          {t("cmd.ready.auto")}
        </span>
      );
    case "needSecret":
      return (
        <span className="badge warn" title={t("cmd.ready.needSecretHint")}>
          {t("cmd.ready.needSecret")}
        </span>
      );
    default:
      return (
        <span className="badge err" title={tg.reason}>
          {t("cmd.ready.unavailable")}
        </span>
      );
  }
}

export function TargetPicker({ targets }: { targets: Target[] }) {
  const t = useT();
  const selected = useCmd((s) => s.selected);
  const filters = useCmd((s) => s.filters);
  const set = useCmd((s) => s.set);
  const showMenu = useUI((s) => s.showMenu);
  const sel = useMemo(() => new Set(selected), [selected]);

  const groups = useMemo(() => Array.from(new Set(targets.map((x) => x.group).filter(Boolean))).sort((a, b) => a.localeCompare(b)), [targets]);
  const tags = useMemo(() => Array.from(new Set(targets.flatMap((x) => x.tags ?? []))).sort((a, b) => a.localeCompare(b)), [targets]);
  const filtered = useMemo(() => targets.filter((x) => matches(x, filters)), [targets, filters]);
  const grouped = useMemo(() => {
    const map = new Map<string, Target[]>();
    for (const x of filtered) map.set(x.group, [...(map.get(x.group) ?? []), x]);
    return Array.from(map.entries()).sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : a.localeCompare(b)));
  }, [filtered]);

  const setFilter = (patch: Partial<Filters>) => set({ filters: { ...filters, ...patch } });
  const toggle = (id: string) => set({ selected: sel.has(id) ? selected.filter((x) => x !== id) : [...selected, id] });
  const add = (list: Target[]) => {
    const next = new Set(selected);
    for (const x of list) if (x.canExec) next.add(x.id);
    set({ selected: Array.from(next) });
  };
  const allFilteredSelected = filtered.filter((x) => x.canExec).every((x) => sel.has(x.id)) && filtered.some((x) => x.canExec);

  const quickMenu = (e: React.MouseEvent) => {
    const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
    const items = [
      ...ENVS.filter((env) => targets.some((x) => x.environment === env)).map((env) => ({
        label: `${t("cmd.quick.env")}: ${t(`app.env.${env}`)}`,
        onClick: () => add(targets.filter((x) => x.environment === env)),
      })),
      ...(groups.length ? [{ separator: true }] : []),
      ...groups.map((g) => ({ label: `${t("cmd.quick.group")}: ${g}`, onClick: () => add(targets.filter((x) => x.group === g)) })),
      ...(tags.length ? [{ separator: true }] : []),
      ...tags.map((tag) => ({ label: `${t("cmd.quick.tag")}: #${tag}`, onClick: () => add(targets.filter((x) => (x.tags ?? []).includes(tag))) })),
      { separator: true },
      { label: t("cmd.quick.ready"), onClick: () => add(targets.filter(runnable)) },
      { label: t("cmd.quick.connected"), onClick: () => add(targets.filter((x) => x.readiness === "connected")) },
    ];
    showMenu(r.left, r.bottom + 4, items);
  };

  const chosen = targets.filter((x) => sel.has(x.id));
  const ready = chosen.filter(runnable).length;
  const needSecret = chosen.filter((x) => x.canExec && x.readiness === "needSecret").length;
  const unavailable = chosen.filter((x) => x.canExec && x.readiness === "unavailable").length;
  const blocked = chosen.filter((x) => !x.canExec).length;
  const prod = chosen.filter((x) => x.environment === "production").length;

  return (
    <div className="panel-box cmd-targets">
      <div className="box-title">
        <span>{t("cmd.targets")}</span>
        <span className="muted">{t("cmd.selectedCount", { n: chosen.length, total: targets.length })}</span>
        <div className="grow" />
        <button className="btn sm ghost" onClick={quickMenu}>
          <Wand2 size={13} /> {t("cmd.quick")} <ChevronDown size={12} />
        </button>
      </div>
      <div className="cmd-filter-row">
        <div className="cmd-search">
          <Search size={13} />
          <input className="input input-sm" placeholder={t("cmd.filter.search")} value={filters.q} onChange={(e) => setFilter({ q: e.target.value })} />
        </div>
        <Select
          size="sm"
          value={filters.group}
          onChange={(group) => setFilter({ group })}
          title={t("cmd.filter.group")}
          options={[{ value: "", label: t("cmd.filter.allGroups") }, ...groups, { value: "\u0000", label: t("cmd.filter.noGroup") }]}
        />
        <Select
          size="sm"
          value={filters.env}
          onChange={(env) => setFilter({ env })}
          title={t("cmd.filter.env")}
          options={[{ value: "", label: t("cmd.filter.allEnvs") }, ...ENVS.map((env) => ({ value: env, label: t(`app.env.${env}`) })), { value: "none", label: t("cmd.filter.noEnv") }]}
        />
        {tags.length > 0 && (
          <Select
            size="sm"
            value={filters.tag}
            onChange={(tag) => setFilter({ tag })}
            title={t("cmd.filter.tag")}
            options={[{ value: "", label: t("cmd.filter.allTags") }, ...tags.map((tag) => ({ value: tag, label: `#${tag}` }))]}
          />
        )}
      </div>
      <div className="cmd-select-row">
        <button className="link-btn" disabled={filtered.length === 0} onClick={() => (allFilteredSelected ? set({ selected: selected.filter((id) => !filtered.some((x) => x.id === id)) }) : add(filtered))}>
          {allFilteredSelected ? <CheckSquare size={13} /> : <Square size={13} />} {t("cmd.selectFiltered", { n: filtered.length })}
        </button>
        {selected.length > 0 && (
          <button className="link-btn" onClick={() => set({ selected: [] })}>
            <X size={13} /> {t("cmd.clearSelection")}
          </button>
        )}
      </div>
      <div className="cmd-target-list">
        {targets.length === 0 && <div className="muted cmd-empty">{t("cmd.noServers")}</div>}
        {targets.length > 0 && filtered.length === 0 && <div className="muted cmd-empty">{t("cmd.noMatch")}</div>}
        {grouped.map(([g, list]) => (
          <div key={g || "_"}>
            {(g || grouped.length > 1) && (
              <div className="cmd-group-label">
                <span>{g || t("cmd.filter.noGroup")}</span>
                <button className="link-btn" onClick={() => add(list)}>
                  {t("cmd.selectGroup")}
                </button>
              </div>
            )}
            {list.map((x) => (
              <label key={x.id} className={`cmd-target ${sel.has(x.id) ? "on" : ""} ${x.canExec ? "" : "blocked"}`}>
                <input type="checkbox" checked={sel.has(x.id)} disabled={!x.canExec && !sel.has(x.id)} onChange={() => toggle(x.id)} />
                <span className="cmd-color" style={{ background: x.color || "#4f8cff" }} />
                <div className="cmd-target-meta">
                  <div className="cmd-target-name">
                    {x.name} <EnvBadge env={x.environment} short />
                  </div>
                  <div className="cmd-target-sub">
                    {x.user}@{x.host}
                    {x.port !== 22 ? `:${x.port}` : ""}
                    {(x.tags ?? []).map((tag) => (
                      <span key={tag} className="cmd-tag">
                        #{tag}
                      </span>
                    ))}
                  </div>
                </div>
                <ReadinessBadge tg={x} />
              </label>
            ))}
          </div>
        ))}
      </div>
      {chosen.length > 0 && (
        <div className="cmd-sel-summary">
          <span className="badge ok">{t("cmd.sum.ready", { n: ready })}</span>
          {needSecret > 0 && <span className="badge warn">{t("cmd.sum.needSecret", { n: needSecret })}</span>}
          {unavailable > 0 && <span className="badge err">{t("cmd.sum.unavailable", { n: unavailable })}</span>}
          {blocked > 0 && <span className="badge err">{t("cmd.sum.blocked", { n: blocked })}</span>}
          {prod > 0 && <span className="env-badge production">{t("cmd.sum.production", { n: prod })}</span>}
        </div>
      )}
      {needSecret > 0 && <div className="cmd-hint warn">{t("cmd.needSecretWarn", { n: needSecret })}</div>}
    </div>
  );
}
