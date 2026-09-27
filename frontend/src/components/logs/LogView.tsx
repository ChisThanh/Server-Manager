import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ArrowDownToLine, Copy, X } from "lucide-react";
import type { LogLine } from "../../../bindings/server-manager/services/logs";
import { useT } from "../../i18n";
import { toast } from "../../store/ui";
import { VirtualRows } from "../../ui/VirtualRows";
import "./logs.css";

/** Height of an unwrapped log row (matches .logs-row line-height). */
const ROW_H = 18;

/** A log line with a stable id (lines are trimmed from the head in live mode). */
export interface ViewLine extends LogLine {
  id: number;
}

let nextId = 1;
export function withIds(lines: LogLine[] | null | undefined): ViewLine[] {
  return (lines ?? []).map((l) => ({ ...l, id: nextId++ }));
}

const LEVEL_TAG: Record<string, string> = {
  emerg: "EMERG",
  alert: "ALERT",
  crit: "CRIT",
  err: "ERROR",
  warning: "WARN",
  notice: "NOTICE",
  info: "INFO",
  debug: "DEBUG",
};

const pad = (n: number, w = 2) => String(n).padStart(w, "0");

function fmtTs(ms: number, withDate: boolean): string {
  if (!ms) return "";
  const d = new Date(ms);
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
  return withDate ? `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}` : time;
}

/** Builds a JS regexp that highlights the search term (null if none/invalid). */
export function highlighter(search: string, regex: boolean, caseSensitive: boolean): RegExp | null {
  if (!search) return null;
  try {
    const src = regex ? search.replace(/^\(\?i\)/, "") : search.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return new RegExp(src, caseSensitive && !search.startsWith("(?i)") ? "g" : "gi");
  } catch {
    return null;
  }
}

function highlight(text: string, re: RegExp | null): ReactNode {
  if (!re || !text) return text;
  const out: ReactNode[] = [];
  let last = 0;
  re.lastIndex = 0;
  let m: RegExpExecArray | null;
  let guard = 0;
  while ((m = re.exec(text)) && guard++ < 200) {
    if (m[0] === "") {
      re.lastIndex++;
      continue;
    }
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(<mark key={m.index}>{m[0]}</mark>);
    last = m.index + m[0].length;
  }
  if (out.length === 0) return text;
  if (last < text.length) out.push(text.slice(last));
  return out;
}

interface RowProps {
  line: ViewLine;
  n: number;
  selected: boolean;
  showSource: boolean;
  withDate: boolean;
  re: RegExp | null;
  onSelect: (id: number, e: React.MouseEvent) => void;
}

const Row = memo(function Row({ line, n, selected, showSource, withDate, re, onSelect }: RowProps) {
  const lvl = line.level || "none";
  return (
    <div className={`logs-row lv-${lvl} ${selected ? "sel" : ""}`}>
      <span className="logs-gutter" onMouseDown={(e) => e.preventDefault()} onClick={(e) => onSelect(line.id, e)}>
        {n}
      </span>
      <span className="logs-ts" title={line.ts ? new Date(line.ts).toISOString() : undefined}>
        {fmtTs(line.ts, withDate)}
      </span>
      <span className={`logs-lvl lv-${lvl}`}>{LEVEL_TAG[line.level] ?? ""}</span>
      {showSource && (
        <span className="logs-src" title={line.source}>
          {line.source}
        </span>
      )}
      <span className="logs-msg">{highlight(line.message || line.raw, re)}</span>
    </div>
  );
});

/**
 * Monospace log view: timestamp column, colored level tags, highlighted
 * matches, line selection + copy, and auto-scroll that pauses when the user
 * scrolls up (with a "jump to latest" button).
 */
export function LogView(props: {
  lines: ViewLine[];
  wrap?: boolean;
  showSource?: boolean;
  re?: RegExp | null;
  empty?: ReactNode;
  className?: string;
  /** Start at the bottom and follow new lines (default true). */
  follow?: boolean;
}) {
  const t = useT();
  const { lines, wrap = false, showSource = true, re = null } = props;
  const box = useRef<HTMLDivElement>(null);
  const stick = useRef(props.follow ?? true);
  const [atBottom, setAtBottom] = useState(true);
  const [sel, setSel] = useState<Set<number>>(new Set());
  const anchor = useRef<number | null>(null);

  // Show dates when the lines span more than one day.
  const withDate = useMemo(() => {
    let min = Infinity;
    let max = 0;
    for (const l of lines) {
      if (!l.ts) continue;
      if (l.ts < min) min = l.ts;
      if (l.ts > max) max = l.ts;
    }
    return max > 0 && (new Date(min).toDateString() !== new Date(max).toDateString() || new Date(max).toDateString() !== new Date().toDateString());
  }, [lines]);

  useLayoutEffect(() => {
    const el = box.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines, wrap]);

  // Forget selected lines that were trimmed away.
  useEffect(() => {
    if (sel.size === 0) return;
    const ids = new Set(lines.map((l) => l.id));
    const kept = [...sel].filter((id) => ids.has(id));
    if (kept.length !== sel.size) setSel(new Set(kept));
  }, [lines]);

  const onScroll = () => {
    const el = box.current;
    if (!el) return;
    const bottom = el.scrollHeight - el.scrollTop - el.clientHeight < 30;
    stick.current = bottom;
    if (bottom !== atBottom) setAtBottom(bottom);
  };

  const jump = () => {
    const el = box.current;
    if (!el) return;
    stick.current = true;
    setAtBottom(true);
    el.scrollTop = el.scrollHeight;
  };

  const linesRef = useRef(lines);
  linesRef.current = lines;
  const onSelect = useCallback((id: number, e: React.MouseEvent) => {
    setSel((prev) => {
      const next = new Set(e.metaKey || e.ctrlKey ? prev : []);
      if (e.shiftKey && anchor.current !== null) {
        const ls = linesRef.current;
        const a = ls.findIndex((l) => l.id === anchor.current);
        const b = ls.findIndex((l) => l.id === id);
        if (a >= 0 && b >= 0) {
          for (let i = Math.min(a, b); i <= Math.max(a, b); i++) next.add(ls[i].id);
          return next;
        }
      }
      if (prev.has(id) && (e.metaKey || e.ctrlKey || prev.size === 1)) next.delete(id);
      else next.add(id);
      anchor.current = id;
      return next;
    });
  }, []);

  const copySel = async () => {
    const text = lines
      .filter((l) => sel.has(l.id))
      .map((l) => l.raw)
      .join("\n");
    try {
      await navigator.clipboard.writeText(text);
      toast(t("logs.copied", { n: sel.size }), "success");
    } catch (e) {
      toast(String(e), "error");
    }
  };

  // ⌘/Ctrl+C copies the selected lines when no text is selected.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "c" && sel.size > 0 && !window.getSelection()?.toString()) {
      e.preventDefault();
      copySel();
    }
    if (e.key === "Escape" && sel.size > 0) setSel(new Set());
  };

  return (
    <div className={`logs-view-wrap ${props.className ?? ""}`}>
      <div ref={box} className={`logs-view ${wrap ? "wrap" : ""}`} onScroll={onScroll} tabIndex={0} onKeyDown={onKeyDown}>
        {lines.length === 0 ? (
          <div className="logs-empty">{props.empty}</div>
        ) : (
          <VirtualRows
            items={lines}
            itemKey={(l) => l.id}
            scrollRef={box}
            estimate={ROW_H}
            layoutKey={`${wrap}|${showSource}`}
            render={(l, i) => <Row key={l.id} line={l} n={i + 1} selected={sel.has(l.id)} showSource={showSource} withDate={withDate} re={re} onSelect={onSelect} />}
          />
        )}
      </div>
      <div className="logs-float">
        {sel.size > 0 && (
          <>
            <button className="btn sm primary" onClick={copySel}>
              <Copy size={13} /> {t("logs.copySel", { n: sel.size })}
            </button>
            <button className="btn sm" onClick={() => setSel(new Set())} title={t("logs.clearSel")}>
              <X size={13} />
            </button>
          </>
        )}
        {!atBottom && lines.length > 0 && (
          <button className="btn sm" onClick={jump}>
            <ArrowDownToLine size={13} /> {t("logs.jumpLatest")}
          </button>
        )}
      </div>
    </div>
  );
}
