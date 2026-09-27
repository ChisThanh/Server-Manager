import type { ReactNode } from "react";
import { AlertTriangle, Inbox } from "lucide-react";
import { useT } from "../i18n";

/** Scrollable page body used by every module panel. */
export function Page({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`sys-page ${className}`}>{children}</div>;
}

export function PageHeader(props: { title: ReactNode; sub?: ReactNode; icon?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="sys-head">
      {props.icon && <div className="page-icon">{props.icon}</div>}
      <div style={{ minWidth: 0 }}>
        <h2>{props.title}</h2>
        {props.sub && <div className="sub">{props.sub}</div>}
      </div>
      <div className="grow" />
      {props.actions}
    </div>
  );
}

export function Section(props: { title: ReactNode; icon?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={`ui-section ${props.className ?? ""}`}>
      <div className="section-title ui-section-head">
        {props.icon}
        <span>{props.title}</span>
        <div className="grow" />
        {props.actions}
      </div>
      {props.children}
    </section>
  );
}

export function Loading({ label }: { label?: string }) {
  return (
    <div className="ui-center">
      <span className="spinner lg" />
      {label && <div className="muted">{label}</div>}
    </div>
  );
}

/** Error state with retry; `actions` adds more buttons next to it. */
export function ErrorBox({ error, onRetry, actions }: { error: string; onRetry?: () => void; actions?: ReactNode }) {
  const t = useT();
  return (
    <div className="ui-center">
      <AlertTriangle size={28} color="var(--warn)" />
      <div className="err ui-center-text">{error}</div>
      {(onRetry || actions) && (
        <div className="ui-center-actions">
          {actions}
          {onRetry && (
            <button className="btn" onClick={onRetry}>
              {t("common.retry")}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

export function Empty({ icon, title, text, action }: { icon?: ReactNode; title?: ReactNode; text?: ReactNode; action?: ReactNode }) {
  return (
    <div className="ui-empty">
      {icon ?? <Inbox size={28} />}
      {title && <div className="ui-empty-title">{title}</div>}
      {text && <div className="muted">{text}</div>}
      {action}
    </div>
  );
}

/** Two-column key/value list. */
export function KV({ items }: { items: [ReactNode, ReactNode][] }) {
  return (
    <div className="ui-kv">
      {items.map(([k, v], i) => (
        <div className="ui-kv-row" key={i}>
          <div className="k">{k}</div>
          <div className="v">{v}</div>
        </div>
      ))}
    </div>
  );
}
