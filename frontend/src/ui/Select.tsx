import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown, Search } from "lucide-react";
import { useT } from "../i18n";

/* A dropdown that looks the same on every platform and grows a search box
   when the list is long. Drop-in for <select>: onChange gets the value. */

export type SelectOption<V extends Val = string> = {
  value: V;
  label?: ReactNode;
  /** Text matched by the search box (defaults to the label when it's a string, else the value). */
  text?: string;
  /** Muted text on the right of the option. */
  hint?: ReactNode;
  icon?: ReactNode;
  disabled?: boolean;
  /** Consecutive options with the same group get a header. */
  group?: string;
};

export type Val = string | number;

type Props<V extends Val> = {
  value: V;
  onChange: (value: V) => void;
  options: readonly (SelectOption<V> | V)[];
  size?: "sm";
  /** Borderless, for status bars and footers. */
  ghost?: boolean;
  disabled?: boolean;
  placeholder?: ReactNode;
  /** Force the search box on/off; by default it shows past 8 options. */
  searchable?: boolean;
  /** Fill the container's width. */
  block?: boolean;
  mono?: boolean;
  invalid?: boolean;
  className?: string;
  style?: CSSProperties;
  title?: string;
  id?: string;
  "aria-label"?: string;
};

const SEARCH_AFTER = 8;
// Past this many options the trigger stops sizing itself to the widest label.
const SIZER_MAX = 40;
// Rendering thousands of rows in a menu is pointless; the search narrows it.
const RENDER_MAX = 400;

const norm = (s: string) =>
  s
    .normalize("NFD")
    .replace(/[̀-ͯ]/g, "")
    .replace(/đ/g, "d")
    .replace(/Đ/g, "D")
    .toLowerCase();

function toOpt<V extends Val>(o: SelectOption<V> | V): SelectOption<V> {
  return typeof o === "object" ? o : { value: o };
}
const labelOf = (o: SelectOption<Val>) => o.label ?? String(o.value);
const textOf = (o: SelectOption<Val>) => o.text ?? (typeof o.label === "string" ? o.label : String(o.value));

export function Select<V extends Val>(p: Props<V>) {
  const t = useT();
  const opts = useMemo(() => p.options.map(toOpt), [p.options]);
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const current = opts.find((o) => o.value === p.value);
  const searchable = p.searchable ?? opts.length > SEARCH_AFTER;

  const [seed, setSeed] = useState("");
  // Latest value/onChange behind a ref so the menu's callbacks stay stable.
  const latest = useRef(p);
  latest.current = p;
  const close = useCallback((refocus = true) => {
    setOpen(false);
    if (refocus) trigger.current?.focus();
  }, []);
  const pick = useCallback(
    (v: V) => {
      close();
      if (v !== latest.current.value) latest.current.onChange(v);
    },
    [close],
  );

  // Arrow keys / type-ahead on the closed trigger step through the options
  // without opening (like a native select); Enter/Space/Alt+Down open it.
  const onKey = (e: KeyboardEvent) => {
    if (p.disabled) return;
    const enabled = opts.filter((o) => !o.disabled);
    const i = enabled.findIndex((o) => o.value === p.value);
    const step = (to: number) => {
      const o = enabled[Math.max(0, Math.min(enabled.length - 1, to))];
      if (o && o.value !== p.value) p.onChange(o.value);
    };
    if (e.key === "Enter" || e.key === " " || (e.altKey && e.key === "ArrowDown")) setOpen(true);
    else if (e.key === "ArrowDown") step(i + 1);
    else if (e.key === "ArrowUp") step(i - 1);
    else if (e.key === "Home") step(0);
    else if (e.key === "End") step(enabled.length - 1);
    else if (e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) {
      if (searchable) {
        setSeed(e.key);
        setOpen(true);
      } else {
        const k = norm(e.key);
        const hit = enabled.find((o, j) => j > i && norm(textOf(o)).startsWith(k)) ?? enabled.find((o) => norm(textOf(o)).startsWith(k));
        if (hit) step(enabled.indexOf(hit));
      }
    } else return;
    e.preventDefault();
  };

  const cls = ["ui-select", p.size === "sm" && "sel-sm", p.ghost && "sel-ghost", p.block && "sel-block", p.mono && "mono", p.invalid && "invalid", open && "open", p.className].filter(Boolean).join(" ");
  return (
    <>
      <button
        ref={trigger}
        type="button"
        id={p.id}
        className={cls}
        style={p.style}
        disabled={p.disabled}
        title={p.title ?? (current ? textOf(current) : undefined)}
        aria-label={p["aria-label"]}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => {
          setSeed("");
          setOpen((o) => !o);
        }}
        onKeyDown={onKey}
      >
        <span className="sel-value">
          <span className={`sel-cur${current ? "" : " ph"}`}>
            {current?.icon}
            <span className="sel-text">{current ? labelOf(current) : (p.placeholder ?? String(p.value ?? ""))}</span>
          </span>
          {!p.block && opts.length <= SIZER_MAX &&
            opts.map((o) => (
              <span key={String(o.value)} className="sel-cur sel-sizer" aria-hidden>
                {o.icon}
                <span className="sel-text">{labelOf(o)}</span>
              </span>
            ))}
        </span>
        <ChevronDown className="sel-chev" size={p.size === "sm" ? 13 : 14} />
      </button>
      {open && trigger.current && (
        <Menu anchor={trigger.current} opts={opts} value={p.value} searchable={searchable} seed={seed} mono={p.mono} placeholder={t("app.selectSearch")} empty={t("app.noMatch")} onPick={pick} onClose={close} />
      )}
    </>
  );
}

function Menu<V extends Val>(p: {
  anchor: HTMLElement;
  opts: SelectOption<V>[];
  value: V;
  searchable: boolean;
  seed: string;
  mono?: boolean;
  placeholder: string;
  empty: string;
  onPick: (v: V) => void;
  onClose: (refocus?: boolean) => void;
}) {
  const [q, setQ] = useState(p.seed);
  const menu = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const search = useRef<HTMLInputElement>(null);
  // Invisible (but focusable, unlike visibility:hidden) until placed; the
  // layout effect places it before the first paint.
  const [pos, setPos] = useState<CSSProperties>({ opacity: 0, top: 0, left: 0 });

  const shown = useMemo(() => {
    const k = norm(q.trim());
    return k ? p.opts.filter((o) => norm(textOf(o)).includes(k) || norm(String(o.value)).includes(k)) : p.opts;
  }, [q, p.opts]);
  const firstEnabled = (from: number, dir: 1 | -1) => {
    for (let i = from; i >= 0 && i < shown.length; i += dir) if (!shown[i].disabled) return i;
    return -1;
  };
  const [active, setActive] = useState(() => {
    const i = shown.findIndex((o) => o.value === p.value);
    return i >= 0 ? i : firstEnabled(0, 1);
  });
  // Typing resets the highlight to the first match.
  const firstRender = useRef(true);
  useEffect(() => {
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    setActive(firstEnabled(0, 1));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shown]);

  // Place under the trigger, or above it when there's more room there.
  useLayoutEffect(() => {
    const r = p.anchor.getBoundingClientRect();
    const vh = window.innerHeight;
    const vw = window.innerWidth;
    const below = vh - r.bottom - 8;
    const above = r.top - 8;
    const want = Math.min(menu.current?.scrollHeight ?? 300, 340);
    const up = below < want && above > below;
    const maxW = Math.max(r.width, Math.min(480, vw - 16));
    const left = Math.max(8, Math.min(r.left, vw - 8 - Math.max(r.width, menu.current?.offsetWidth ?? r.width)));
    setPos({
      left,
      minWidth: r.width,
      maxWidth: maxW,
      maxHeight: Math.max(120, Math.min(340, up ? above : below)),
      ...(up ? { bottom: vh - r.top + 4 } : { top: r.bottom + 4 }),
    });
  }, [p.anchor]);

  useLayoutEffect(() => {
    (p.searchable ? search.current : list.current)?.focus({ preventScroll: true });
  }, [p.searchable]);

  useLayoutEffect(() => {
    list.current?.querySelector<HTMLElement>(`[data-i="${active}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active, pos]);

  // Close on outside press, on scroll of anything but the menu, on resize/blur.
  useEffect(() => {
    const inside = (n: EventTarget | null) => n instanceof Node && (menu.current?.contains(n) || p.anchor.contains(n));
    const down = (e: MouseEvent) => !inside(e.target) && p.onClose(false);
    const scroll = (e: Event) => !(e.target instanceof Node && menu.current?.contains(e.target)) && p.onClose(false);
    const away = () => p.onClose(false);
    // Escape closes the menu wherever focus is (and before a modal sees it:
    // Modal ignores Escape while a [data-popover-open] element exists).
    const esc = (e: globalThis.KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.preventDefault();
      p.onClose();
    };
    window.addEventListener("keydown", esc);
    document.addEventListener("mousedown", down, true);
    window.addEventListener("scroll", scroll, true);
    window.addEventListener("resize", away);
    window.addEventListener("blur", away);
    return () => {
      document.removeEventListener("mousedown", down, true);
      window.removeEventListener("scroll", scroll, true);
      window.removeEventListener("resize", away);
      window.removeEventListener("blur", away);
      window.removeEventListener("keydown", esc);
    };
  }, [p.anchor, p.onClose]);

  const onKey = (e: KeyboardEvent) => {
    const move = (to: number, dir: 1 | -1) => {
      const i = firstEnabled(to, dir);
      if (i >= 0) setActive(i);
    };
    if (e.key === "ArrowDown") move(active + 1, 1);
    else if (e.key === "ArrowUp") move(active - 1, -1);
    else if (e.key === "PageDown") move(Math.min(shown.length - 1, active + 8), 1);
    else if (e.key === "PageUp") move(Math.max(0, active - 8), -1);
    else if (e.key === "Home" && !p.searchable) move(0, 1);
    else if (e.key === "End" && !p.searchable) move(shown.length - 1, -1);
    else if (e.key === "Enter") {
      const o = shown[active];
      if (o && !o.disabled) p.onPick(o.value);
    } else if (e.key === "Tab") p.onClose(false);
    else if (!p.searchable && e.key.length === 1 && !e.metaKey && !e.ctrlKey) {
      const k = norm(e.key);
      const hit = shown.findIndex((o, j) => j > active && !o.disabled && norm(textOf(o)).startsWith(k));
      const i = hit >= 0 ? hit : shown.findIndex((o) => !o.disabled && norm(textOf(o)).startsWith(k));
      if (i >= 0) setActive(i);
    } else return;
    e.preventDefault();
    e.stopPropagation();
  };

  const onHover = useCallback((i: number) => setActive(i), []);
  const onPick = p.onPick;
  const rows: ReactNode[] = [];
  const n = Math.min(shown.length, RENDER_MAX);
  for (let i = 0; i < n; i++) {
    const o = shown[i];
    if (o.group && o.group !== shown[i - 1]?.group)
      rows.push(
        <div key={`g:${o.group}:${i}`} className="sel-group" role="presentation">
          {o.group}
        </div>,
      );
    rows.push(<Row key={String(o.value)} i={i} o={o} active={i === active} selected={o.value === p.value} onHover={onHover} onPick={onPick} />);
  }

  return createPortal(
    <div ref={menu} className={`sel-menu${p.mono ? " mono" : ""}`} style={pos} data-popover-open onKeyDown={onKey}>
      {p.searchable && (
        <div className="sel-search">
          <Search size={13} />
          <input ref={search} value={q} onChange={(e) => setQ(e.target.value)} placeholder={p.placeholder} spellCheck={false} autoComplete="off" aria-autocomplete="list" />
        </div>
      )}
      <div ref={list} className="sel-list" role="listbox" tabIndex={-1} aria-activedescendant={active >= 0 ? `sel-o-${active}` : undefined}>
        {rows}
        {shown.length === 0 && <div className="sel-empty">{p.empty}</div>}
        {shown.length > n && <div className="sel-empty">+{shown.length - n} …</div>}
      </div>
    </div>,
    document.body,
  );
}

const Row = memo(function Row<V extends Val>(p: { i: number; o: SelectOption<V>; active: boolean; selected: boolean; onHover: (i: number) => void; onPick: (v: V) => void }) {
  const { o } = p;
  return (
    <div
      id={`sel-o-${p.i}`}
      data-i={p.i}
      role="option"
      aria-selected={p.selected}
      aria-disabled={o.disabled || undefined}
      className={`sel-opt${p.active ? " active" : ""}${p.selected ? " selected" : ""}${o.disabled ? " disabled" : ""}`}
      onMouseMove={p.active || o.disabled ? undefined : () => p.onHover(p.i)}
      onMouseDown={(e) => e.preventDefault()}
      onClick={o.disabled ? undefined : () => p.onPick(o.value)}
    >
      {o.icon}
      <span className="sel-text">{labelOf(o)}</span>
      {o.hint != null && <span className="sel-hint">{o.hint}</span>}
      <Check className="sel-check" size={13} />
    </div>
  );
}) as <V extends Val>(p: { i: number; o: SelectOption<V>; active: boolean; selected: boolean; onHover: (i: number) => void; onPick: (v: V) => void }) => ReactNode;
