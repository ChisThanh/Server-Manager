import type { ReactNode } from "react";

export interface TabItem<T extends string> {
  id: T;
  label: ReactNode;
  icon?: ReactNode;
  badge?: ReactNode;
  hidden?: boolean;
}

/** Horizontal sub-navigation inside a module panel. */
export function SubTabs<T extends string>({ items, value, onChange, right }: { items: TabItem<T>[]; value: T; onChange: (v: T) => void; right?: ReactNode }) {
  return (
    <div className="ui-subtabs">
      {items
        .filter((i) => !i.hidden)
        .map((i) => (
          <button key={i.id} className={`ui-subtab ${value === i.id ? "on" : ""}`} onClick={() => onChange(i.id)}>
            {i.icon}
            <span>{i.label}</span>
            {i.badge !== undefined && i.badge !== null && i.badge !== 0 && <span className="ui-subtab-badge">{i.badge}</span>}
          </button>
        ))}
      <div className="grow" />
      {right}
    </div>
  );
}
