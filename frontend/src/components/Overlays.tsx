import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { AlertTriangle, CheckCircle2, Info, X, XCircle, Upload, Download } from "lucide-react";
import { useUI, type DialogField } from "../store/ui";
import { useApp } from "../store/app";
import { FileService } from "../lib/api";
import { formatBytes } from "../lib/format";
import { formatAppError } from "../lib/api";
import { useT } from "../i18n";

export function Modal(props: {
  title: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  onClose: () => void;
  size?: "wide" | "xwide";
}) {
  const self = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      // An open dropdown inside the modal takes Escape first.
      if (document.querySelector("[data-popover-open]")) return;
      // With stacked modals only the top visible one closes.
      const shown = Array.from(document.querySelectorAll(".overlay")).filter((el) => el.getClientRects().length > 0);
      if (shown[shown.length - 1] !== self.current) return;
      e.stopPropagation();
      props.onClose();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [props.onClose]);
  return (
    <div className="overlay" ref={self} onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className={`modal ${props.size ?? ""}`} role="dialog">
        <div className="modal-head">{props.title}</div>
        <div className="modal-body">{props.children}</div>
        {props.footer && <div className="modal-foot">{props.footer}</div>}
      </div>
    </div>
  );
}

function DialogView({ id }: { id: number }) {
  const t = useT();
  const d = useUI((s) => s.dialogs.find((x) => x.id === id))!;
  const close = useUI((s) => s.closeDialog);
  const [values, setValues] = useState<Record<string, string | boolean>>(() =>
    Object.fromEntries((d.fields ?? []).map((f) => [f.name, f.value ?? (f.type === "checkbox" ? false : "")])),
  );
  const [touched, setTouched] = useState(false);

  const errors = Object.fromEntries(
    (d.fields ?? []).map((f) => [f.name, f.validate && f.type !== "checkbox" ? f.validate(String(values[f.name] ?? "")) : null]),
  );
  const valid = Object.values(errors).every((e) => !e);
  const submit = (action = "ok") => {
    setTouched(true);
    if (action === "ok" && !valid) return;
    close(id, { action, values });
  };

  return (
    <Modal
      title={
        <>
          {d.danger && <AlertTriangle size={18} color="var(--warn)" />}
          {d.title}
        </>
      }
      size={d.wide ? "wide" : undefined}
      onClose={() => close(id, null)}
      footer={
        <>
          {d.extra?.map((b) => (
            <button key={b.id} className={`btn ${b.danger ? "danger" : ""}`} onClick={() => submit(b.id)}>
              {b.label}
            </button>
          ))}
          <button className="btn" onClick={() => close(id, null)}>
            {d.cancelText ?? t("common.cancel")}
          </button>
          <button className={`btn ${d.danger ? "danger" : "primary"}`} onClick={() => submit()} autoFocus={!d.fields?.length}>
            {d.confirmText ?? t("common.ok")}
          </button>
        </>
      }
    >
      {d.message && <div className="modal-msg">{d.message}</div>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        {d.fields?.map((f) => (
          <FieldInput
            key={f.name}
            field={f}
            value={values[f.name]}
            error={touched ? errors[f.name] : null}
            onChange={(v) => setValues((s) => ({ ...s, [f.name]: v }))}
          />
        ))}
        <button type="submit" hidden />
      </form>
    </Modal>
  );
}

function FieldInput(props: {
  field: DialogField;
  value: string | boolean | undefined;
  error: string | null;
  onChange: (v: string | boolean) => void;
}) {
  const { field: f } = props;
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (!f.autoFocus || !ref.current) return;
    ref.current.focus();
    // Pre-select the file name without its extension, like Finder/VS Code.
    const v = ref.current.value;
    const dot = v.lastIndexOf(".");
    ref.current.setSelectionRange(0, dot > 0 ? dot : v.length);
  }, []);
  if (f.type === "checkbox") {
    return (
      <label className="check field">
        <input type="checkbox" checked={Boolean(props.value)} onChange={(e) => props.onChange(e.target.checked)} />
        {f.label}
      </label>
    );
  }
  return (
    <div className="field">
      <label>{f.label}</label>
      {f.type === "textarea" ? (
        <textarea className="input" value={String(props.value ?? "")} onChange={(e) => props.onChange(e.target.value)} placeholder={f.placeholder} />
      ) : (
        <input
          ref={ref}
          className={`input ${props.error ? "invalid" : ""}`}
          type={f.type ?? "text"}
          value={String(props.value ?? "")}
          placeholder={f.placeholder}
          onChange={(e) => props.onChange(e.target.value)}
          spellCheck={false}
          autoComplete="off"
          autoCapitalize="off"
        />
      )}
      {props.error && <div className="error">{props.error}</div>}
    </div>
  );
}

export function DialogHost() {
  const dialogs = useUI((s) => s.dialogs);
  return (
    <>
      {dialogs.map((d) => (
        <DialogView key={d.id} id={d.id} />
      ))}
    </>
  );
}

export function Toasts() {
  const toasts = useUI((s) => s.toasts);
  const dismiss = useUI((s) => s.dismissToast);
  return (
    <div className="toasts">
      {toasts.map((t) => (
        <div key={t.id} className={`toast ${t.kind}`}>
          {t.kind === "success" ? (
            <CheckCircle2 size={16} color="var(--ok)" />
          ) : t.kind === "error" ? (
            <XCircle size={16} color="var(--err)" />
          ) : (
            <Info size={16} color="var(--accent)" />
          )}
          <div className="text">{t.text}</div>
          <button className="icon-btn" style={{ width: 18, height: 18 }} onClick={() => dismiss(t.id)}>
            <X size={12} />
          </button>
        </div>
      ))}
    </div>
  );
}

export function ContextMenu() {
  const menu = useUI((s) => s.menu);
  const hide = useUI((s) => s.hideMenu);
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ x: 0, y: 0 });

  useLayoutEffect(() => {
    if (!menu || !ref.current) return;
    const r = ref.current.getBoundingClientRect();
    setPos({
      x: Math.min(menu.x, window.innerWidth - r.width - 6),
      y: Math.min(menu.y, window.innerHeight - r.height - 6),
    });
  }, [menu]);

  useEffect(() => {
    if (!menu) return;
    const close = (e: Event) => {
      if (ref.current?.contains(e.target as Node)) return;
      hide();
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && hide();
    window.addEventListener("mousedown", close, true);
    window.addEventListener("blur", hide);
    window.addEventListener("keydown", onKey);
    window.addEventListener("resize", hide);
    return () => {
      window.removeEventListener("mousedown", close, true);
      window.removeEventListener("blur", hide);
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("resize", hide);
    };
  }, [menu, hide]);

  if (!menu) return null;
  return (
    <div ref={ref} className="ctx-menu" style={{ left: pos.x || menu.x, top: pos.y || menu.y }} onContextMenu={(e) => e.preventDefault()}>
      {menu.items.map((it, i) =>
        it.separator ? (
          <div key={i} className="ctx-sep" />
        ) : (
          <div
            key={i}
            className={`ctx-item ${it.danger ? "danger" : ""} ${it.disabled ? "disabled" : ""}`}
            onClick={() => {
              hide();
              it.onClick?.();
            }}
          >
            {it.icon}
            {it.label}
            {it.shortcut && <span className="sc">{it.shortcut}</span>}
          </div>
        ),
      )}
    </div>
  );
}

export function Transfers() {
  const t = useT();
  const transfers = useApp((s) => s.transfers);
  const dismiss = useApp((s) => s.dismissTransfer);
  const list = Object.values(transfers);
  const [collapsed, setCollapsed] = useState(false);
  if (list.length === 0) return null;
  const running = list.filter((t) => t.state === "running").length;
  return (
    <div className="transfers">
      <div className="transfers-head">
        <span className="grow">{running > 0 ? t("tr.running", { n: running }) : t("tr.title")}</span>
        <button className="icon-btn" onClick={() => setCollapsed((c) => !c)} title={collapsed ? t("common.expand") : t("common.collapse")}>
          {collapsed ? "▴" : "▾"}
        </button>
      </div>
      {!collapsed &&
        list.map((tr) => {
          const pct = tr.total > 0 ? Math.min(100, (tr.done / tr.total) * 100) : tr.state === "done" ? 100 : 0;
          const name = tr.more > 0 ? t("tr.more", { name: tr.name, n: tr.more }) : tr.name;
          const error = formatAppError(tr.error);
          return (
            <div key={tr.id} className={`transfer ${tr.state}`}>
              <div className="top">
                {tr.kind === "upload" ? <Upload size={13} /> : <Download size={13} />}
                <span className="name" title={name}>
                  {name}
                </span>
                {tr.state === "running" ? (
                  <button className="icon-btn" title={t("tr.cancel")} onClick={() => FileService.CancelTransfer(tr.id)}>
                    <X size={13} />
                  </button>
                ) : (
                  <button className="icon-btn" title={t("common.hide")} onClick={() => dismiss(tr.id)}>
                    <X size={13} />
                  </button>
                )}
              </div>
              <div className="meter">
                <div style={{ width: `${pct}%` }} />
              </div>
              <div className="info">
                <span title={error || tr.current}>
                  {tr.state === "error"
                    ? error
                    : tr.state === "cancelled"
                      ? t("tr.cancelled")
                      : tr.state === "done"
                        ? t("tr.doneTo", { target: tr.target })
                        : tr.files > 1
                          ? t("tr.files", { done: tr.filesDone, total: tr.files })
                          : tr.target}
                </span>
                <span>
                  {formatBytes(tr.done)} / {formatBytes(tr.total)}
                </span>
              </div>
            </div>
          );
        })}
    </div>
  );
}
