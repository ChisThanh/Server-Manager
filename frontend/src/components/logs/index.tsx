import { useEffect, useMemo, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { CaseSensitive, Download, Eraser, Pause, Play, Radio, Regex, RefreshCw, Square, WrapText } from "lucide-react";
import { LogsService, type QuerySpec, type Source } from "../../../bindings/server-manager/services/logs";
import type { PanelProps } from "../../ui/types";
import { useRemote } from "../../ui/hooks";
import { errMsg, formatAppError } from "../../lib/api";
import { formatBytes, formatDate } from "../../lib/format";
import { withSudo } from "../../store/sudo";
import { toast } from "../../store/ui";
import { useT, type Key } from "../../i18n";
import { LogView, highlighter, withIds, type ViewLine } from "./LogView";
import "./logs.css";
import { Select, type SelectOption } from "../../ui/Select";

/** Lines kept on screen while following live. */
const LIVE_MAX = 5000;
const LINE_OPTIONS = [100, 500, 1000, 2000, 5000, 10000];
const LEVELS = ["", "emerg", "alert", "crit", "err", "warning", "notice", "info"] as const;
const RANGES: Record<string, number> = { "15m": 900, "1h": 3600, "6h": 6 * 3600, "24h": 86400, "7d": 7 * 86400 };
const RANGE_KEYS = ["all", "15m", "1h", "6h", "24h", "7d", "custom"] as const;
type Range = (typeof RANGE_KEYS)[number];

interface Sel {
  kind: string;
  target: string;
}

const selKey = (s: Sel | null) => (s ? `${s.kind}\u0001${s.target}` : "");
const CUSTOM = "__custom";

interface Meta {
  count: number;
  tookMs: number;
  sudo: boolean;
  limitReached: boolean;
  truncated: boolean;
  partial: boolean;
}

/** Argument other panels pass with navigate("logs", arg). */
interface LogsArg {
  source?: "unit" | "docker" | "file" | "journal" | "kernel" | "auth";
  unit?: string;
  container?: string;
  path?: string;
}

function toUnix(v: string): number {
  if (!v) return 0;
  const ms = new Date(v).getTime();
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : 0;
}

function trim(lines: ViewLine[]): ViewLine[] {
  return lines.length > LIVE_MAX ? lines.slice(lines.length - LIVE_MAX) : lines;
}

export default function LogsPanel({ connId, visible, connected, arg }: PanelProps) {
  const t = useT();
  // Discover sources the first time the panel is shown.
  const [wanted, setWanted] = useState(false);
  useEffect(() => {
    if (visible && connected) setWanted(true);
  }, [visible, connected]);
  const sources = useRemote(() => LogsService.Sources(connId), [connId], { enabled: wanted && connected });

  const [sel, setSel] = useState<Sel | null>(null);
  const [customOpen, setCustomOpen] = useState(false);
  const [customPath, setCustomPath] = useState("");
  const [level, setLevel] = useState("");
  const [range, setRange] = useState<Range>("all");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [draft, setDraft] = useState("");
  const [search, setSearch] = useState("");
  const [regex, setRegex] = useState(false);
  const [cs, setCs] = useState(false);
  const [lineCount, setLineCount] = useState(500);
  const [wrap, setWrap] = useState(false);

  const [lines, setLines] = useState<ViewLine[]>([]);
  const [meta, setMeta] = useState<Meta | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [downloading, setDownloading] = useState(false);

  const [live, setLive] = useState(false);
  const [paused, setPaused] = useState(false);
  const [pendingN, setPendingN] = useState(0);
  const [dropped, setDropped] = useState(0);
  const streamRef = useRef<string | null>(null);
  const liveRef = useRef(false);
  const pausedRef = useRef(false);
  const pendingRef = useRef<ViewLine[]>([]);
  const earlyRef = useRef<Map<string, { lines: ViewLine[]; done?: { error: string } }>>(new Map());
  const startingRef = useRef(false);
  const seq = useRef(0);
  liveRef.current = live;
  pausedRef.current = paused;

  const list = sources.data?.sources ?? [];
  const groups = useMemo(() => {
    const g = { journal: [] as Source[], units: [] as Source[], files: [] as Source[], rotated: [] as Source[], docker: [] as Source[] };
    for (const s of list) {
      if (s.kind === "journal" || s.kind === "kernel" || s.kind === "auth") g.journal.push(s);
      else if (s.kind === "unit") g.units.push(s);
      else if (s.kind === "file") (s.rotated ? g.rotated : g.files).push(s);
      else if (s.kind === "docker") g.docker.push(s);
    }
    return g;
  }, [list]);

  // Preselect from navigate(), else a sensible default once sources arrive.
  const argUsed = useRef<unknown>(undefined);
  useEffect(() => {
    const a = arg as LogsArg | undefined;
    if (!a || argUsed.current === arg) return;
    argUsed.current = arg;
    if (a.source === "unit" && a.unit) setSel({ kind: "unit", target: a.unit });
    else if (a.source === "docker" && a.container) setSel({ kind: "docker", target: a.container });
    else if (a.source === "file" && a.path) setSel({ kind: "file", target: a.path });
    else if (a.source === "journal" || a.source === "kernel" || a.source === "auth") setSel({ kind: a.source, target: "" });
    setCustomOpen(false);
  }, [arg]);
  useEffect(() => {
    if (sel || !sources.data) return;
    const first = groups.journal[0] ?? groups.files.find((f) => f.common) ?? groups.files[0] ?? groups.docker[0];
    if (first) setSel({ kind: first.kind, target: first.target });
  }, [sources.data]);

  const spec = (): QuerySpec => {
    let since = 0;
    let until = 0;
    if (range === "custom") {
      since = toUnix(from);
      until = toUnix(to);
    } else if (RANGES[range]) {
      since = Math.floor(Date.now() / 1000) - RANGES[range];
    }
    return { kind: sel!.kind, target: sel!.target, lines: lineCount, since, until, level, search, regex, caseSensitive: cs };
  };

  const runQuery = async () => {
    if (!sel || !connected) return;
    const my = ++seq.current;
    setLoading(true);
    setError("");
    try {
      const sp = spec();
      const r = await withSudo(connId, (pw) => LogsService.Query(connId, sp, pw));
      if (my !== seq.current) return;
      setLines(withIds(r.lines));
      setMeta({ count: r.lines?.length ?? 0, tookMs: r.tookMs, sudo: r.sudo, limitReached: r.limitReached, truncated: r.truncated, partial: r.partial });
    } catch (e) {
      if (my !== seq.current) return;
      setError(errMsg(e));
      setLines([]);
      setMeta(null);
    } finally {
      if (my === seq.current) setLoading(false);
    }
  };

  const stopLive = async () => {
    const id = streamRef.current;
    streamRef.current = null;
    setLive(false);
    setPaused(false);
    if (pendingRef.current.length) {
      const p = pendingRef.current;
      pendingRef.current = [];
      setLines((prev) => trim([...prev, ...p]));
    }
    setPendingN(0);
    if (id) {
      try {
        await LogsService.StopStream(id);
      } catch {
        /* already gone */
      }
    }
  };

  const applyIncoming = (incoming: ViewLine[]) => {
    if (!incoming.length) return;
    if (pausedRef.current) {
      pendingRef.current = trim([...pendingRef.current, ...incoming]);
      setPendingN(pendingRef.current.length);
    } else {
      setLines((prev) => trim([...prev, ...incoming]));
    }
  };

  const endStream = (err: string) => {
    streamRef.current = null;
    setLive(false);
    if (err) setError(err);
    else toast(t("logs.streamEnded"));
  };

  const startLive = async () => {
    if (!sel || !connected) return;
    await stopLive();
    ++seq.current; // drop any query in flight
    setLoading(false);
    setError("");
    setLines([]);
    setMeta(null);
    setDropped(0);
    pendingRef.current = [];
    earlyRef.current.clear();
    startingRef.current = true;
    try {
      const sp = { ...spec(), lines: Math.min(lineCount, 1000) };
      const id = await withSudo(connId, (pw) => LogsService.StartStream(connId, sp, pw));
      streamRef.current = id;
      setLive(true);
      // Replay events that arrived before the id was known.
      const early = earlyRef.current.get(id);
      earlyRef.current.clear();
      if (early) {
        applyIncoming(early.lines);
        if (early.done) endStream(early.done.error);
      }
    } catch (e) {
      setError(errMsg(e));
    } finally {
      startingRef.current = false;
    }
  };

  useEffect(() => {
    const off = Events.On("logs:lines", (ev) => {
      const d = ev.data;
      if (!d) return;
      const incoming = withIds(d.lines);
      if (d.streamId !== streamRef.current) {
        if (startingRef.current) {
          const e = earlyRef.current.get(d.streamId) ?? { lines: [] };
          e.lines.push(...incoming);
          if (d.done) e.done = { error: d.error ? formatAppError(d.error) : "" };
          earlyRef.current.set(d.streamId, e);
        }
        return;
      }
      if (d.dropped) setDropped((x) => x + d.dropped);
      applyIncoming(incoming);
      if (d.done) endStream(d.error ? formatAppError(d.error) : "");
    });
    return () => {
      off();
      const id = streamRef.current;
      streamRef.current = null;
      if (id) LogsService.StopStream(id).catch(() => {});
    };
  }, []);

  // Connection lost: the backend ends the stream; reflect it right away.
  useEffect(() => {
    if (!connected && streamRef.current) {
      streamRef.current = null;
      setLive(false);
    }
  }, [connected]);

  // (Re)load whenever the source or a filter changes.
  const customRangeKey = range === "custom" ? `${from}|${to}` : "";
  useEffect(() => {
    if (!sel || !connected || !wanted) return;
    if (liveRef.current) startLive();
    else runQuery();
  }, [selKey(sel), level, range, customRangeKey, search, regex, cs, lineCount, connected, wanted]);

  const re = useMemo(() => highlighter(search, regex, cs), [search, regex, cs]);

  const togglePause = () => {
    if (paused) {
      const p = pendingRef.current;
      pendingRef.current = [];
      setPendingN(0);
      setPaused(false);
      if (p.length) setLines((prev) => trim([...prev, ...p]));
    } else {
      setPaused(true);
    }
  };

  const download = async () => {
    if (!sel) return;
    setDownloading(true);
    try {
      const sp = { ...spec(), lines: 0 };
      const r = await withSudo(connId, (pw) => LogsService.Download(connId, sp, pw, t("logs.downloadTitle")));
      if (r.path) {
        toast(t("logs.downloaded", { lines: r.lines, size: formatBytes(r.bytes), path: r.path }), "success");
        if (r.truncated) toast(t("logs.downloadTruncated"), "error");
      }
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setDownloading(false);
    }
  };

  const onSourceChange = (v: string) => {
    if (v === CUSTOM) {
      setCustomOpen(true);
      return;
    }
    setCustomOpen(false);
    const [kind, target] = v.split("\u0001");
    setSel({ kind, target: target ?? "" });
  };

  const openCustom = () => {
    const p = customPath.trim();
    if (!p) return;
    setSel({ kind: "file", target: p });
    setCustomOpen(false);
  };

  const known = list.some((s) => selKey(s) === selKey(sel));
  const selectValue = customOpen ? CUSTOM : selKey(sel);

  // Built once per source list, not on every streamed line.
  const sourceOptions = useMemo(() => {
    const journalLabel: Record<string, Key> = { journal: "logs.src.journal", kernel: "logs.src.kernel", auth: "logs.src.auth" };
    const out: SelectOption[] = [];
    const add = (group: string, xs: Source[], label: (s: Source) => string, hint?: (s: Source) => string | undefined) => {
      for (const s of xs) {
        const h = [hint?.(s), s.needsSudo ? t("logs.sudoMark") : undefined].filter(Boolean).join(" ");
        out.push({ value: selKey(s), label: label(s), group, hint: h || undefined, text: `${label(s)} ${s.target}` });
      }
    };
    add(t("logs.group.journal"), groups.journal, (s) => t(journalLabel[s.kind]));
    add(t("logs.group.units"), groups.units, (s) => s.target, (s) => (s.state === "failed" ? "failed" : undefined));
    add(t("logs.group.files"), groups.files, (s) => s.target);
    add(t("logs.group.docker"), groups.docker, (s) => s.target, (s) => s.state);
    add(t("logs.group.rotated"), groups.rotated, (s) => s.target);
    const other = t("logs.group.other");
    if (sel && !known) out.push({ value: selKey(sel), label: sel.target || sel.kind, group: other });
    out.push({ value: CUSTOM, label: t("logs.src.custom"), group: other });
    return out;
    // t is stable; a translated string in the deps re-runs this on language change.
  }, [groups, sel, known, t("logs.group.other")]);
  const current = sel ? list.find((s) => selKey(s) === selKey(sel)) : undefined;

  if (!wanted) return <div className="logs-panel" />;

  return (
    <div className="logs-panel">
      <div className="logs-toolbar">
        <Select
          size="sm"
          className="logs-source"
          value={selectValue}
          onChange={onSourceChange}
          title={t("logs.source")}
          disabled={!connected}
          placeholder={sources.loading ? t("common.loading") : t("logs.pickSource")}
          options={sourceOptions}
        />
        <button className="icon-btn" onClick={() => sources.reload()} title={t("logs.refreshSources")} disabled={!connected || sources.loading}>
          <RefreshCw size={14} className={sources.loading ? "spin-icon" : ""} />
        </button>

        <Select size="sm" value={level} onChange={setLevel} title={t("logs.level")} options={LEVELS.map((l) => ({ value: l, label: t((l ? `logs.level.${l}` : "logs.level.all") as Key) }))} />
        <Select size="sm" value={range} onChange={setRange} title={t("logs.range")} disabled={live} options={RANGE_KEYS.map((r) => ({ value: r, label: t(`logs.range.${r}` as Key) }))} />
        <Select size="sm" value={lineCount} onChange={setLineCount} title={t("logs.linesLabel")} options={LINE_OPTIONS.map((n) => ({ value: n, label: t("logs.linesOpt", { n: n.toLocaleString() }) }))} />

        <div className="logs-search">
          <input
            className="input"
            placeholder={t("logs.search")}
            value={draft}
            spellCheck={false}
            onChange={(e) => {
              setDraft(e.target.value);
              if (!e.target.value) setSearch("");
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                if (draft === search) (live ? startLive : runQuery)();
                else setSearch(draft);
              }
              if (e.key === "Escape") {
                setDraft("");
                setSearch("");
              }
            }}
          />
          <div className="logs-search-opts">
            <button className={`icon-btn ${regex ? "active" : ""}`} onClick={() => setRegex(!regex)} title={t("logs.regex")}>
              <Regex size={13} />
            </button>
            <button className={`icon-btn ${cs ? "active" : ""}`} onClick={() => setCs(!cs)} title={t("logs.caseSensitive")}>
              <CaseSensitive size={14} />
            </button>
          </div>
        </div>

        <div className="grow" />
        {!live ? (
          <>
            <button className="btn sm" onClick={runQuery} disabled={!sel || loading || !connected} title={t("logs.run")}>
              {loading ? <span className="spinner" /> : <RefreshCw size={13} />}
            </button>
            <button className="btn sm" onClick={startLive} disabled={!sel || !connected}>
              <Radio size={13} /> {t("logs.live")}
            </button>
          </>
        ) : (
          <>
            <button className="btn sm" onClick={togglePause}>
              {paused ? <Play size={13} /> : <Pause size={13} />} {paused ? (pendingN ? t("logs.resumeN", { n: pendingN }) : t("logs.resume")) : t("logs.pause")}
            </button>
            <button className="btn sm" onClick={stopLive}>
              <Square size={12} /> {t("logs.stopLive")}
            </button>
          </>
        )}
        <button className="icon-btn" onClick={() => setLines([])} title={t("logs.clear")} disabled={lines.length === 0}>
          <Eraser size={14} />
        </button>
        <button className={`icon-btn ${wrap ? "active" : ""}`} onClick={() => setWrap(!wrap)} title={t("logs.wrap")}>
          <WrapText size={14} />
        </button>
        <button className="icon-btn" onClick={download} title={t("logs.download")} disabled={!sel || downloading || !connected}>
          {downloading ? <span className="spinner" /> : <Download size={14} />}
        </button>
      </div>

      {(customOpen || range === "custom") && (
        <div className="logs-custom">
          {customOpen && (
            <>
              <input
                className="input path"
                autoFocus
                placeholder={t("logs.customPathPh")}
                value={customPath}
                spellCheck={false}
                onChange={(e) => setCustomPath(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") openCustom();
                  if (e.key === "Escape") setCustomOpen(false);
                }}
              />
              <button className="btn sm primary" onClick={openCustom} disabled={!customPath.trim().startsWith("/")}>
                {t("logs.open")}
              </button>
            </>
          )}
          {range === "custom" && !live && (
            <>
              <span className="muted">{t("logs.from")}</span>
              <input className="input" type="datetime-local" value={from} onChange={(e) => setFrom(e.target.value)} />
              <span className="muted">{t("logs.to")}</span>
              <input className="input" type="datetime-local" value={to} onChange={(e) => setTo(e.target.value)} />
            </>
          )}
        </div>
      )}

      {sources.error && !sources.data && (
        <div className="logs-error">
          <span className="grow">{sources.error}</span>
          <button className="btn sm" onClick={() => sources.reload()}>
            {t("common.retry")}
          </button>
        </div>
      )}
      {sources.data?.dockerError && <div className="logs-note">{t("logs.dockerError", { msg: sources.data.dockerError })}</div>}
      {error && (
        <div className="logs-error">
          <span className="grow">{error}</span>
          {!live && sel && (
            <button className="btn sm" onClick={runQuery}>
              {t("common.retry")}
            </button>
          )}
        </div>
      )}

      <LogView
        lines={lines}
        wrap={wrap}
        re={re}
        showSource={sel?.kind === "journal" || sel?.kind === "auth" || sel?.kind === "kernel" || sel?.kind === "file"}
        empty={
          loading ? (
            <span className="spinner lg" />
          ) : live ? (
            t("logs.waiting")
          ) : !sel ? (
            sources.data && list.length === 0 ? t("logs.noSources") : t("logs.pickSource")
          ) : error ? (
            ""
          ) : meta ? (
            <div>
              <div>{t("logs.empty")}</div>
              <div className="muted" style={{ marginTop: 4 }}>
                {t("logs.emptyHint")}
              </div>
            </div>
          ) : (
            ""
          )
        }
      />

      <div className="logs-status">
        {live ? <span className={paused ? "warn" : "live"}>{paused ? t("logs.st.paused") : t("logs.st.live")}</span> : null}
        <span>{t("logs.st.lines", { n: lines.length.toLocaleString() })}</span>
        {live && lines.length >= LIVE_MAX && <span>{t("logs.st.cap", { n: LIVE_MAX.toLocaleString() })}</span>}
        {live && dropped > 0 && <span className="warn">{t("logs.st.dropped", { n: dropped })}</span>}
        {!live && meta && (
          <>
            <span>{t("logs.st.took", { ms: meta.tookMs })}</span>
            {meta.sudo && <span>{t("logs.st.sudo")}</span>}
            {meta.limitReached && <span>{t("logs.st.limit")}</span>}
            {meta.truncated && <span className="warn">{t("logs.st.truncated")}</span>}
            {meta.partial && <span className="warn">{t("logs.st.partial")}</span>}
          </>
        )}
        {current?.kind === "file" && current.size > 0 && <span>{t("logs.size", { size: formatBytes(current.size), time: formatDate(current.mtime) })}</span>}
        <span className="grow" />
        <span className="muted">{t("logs.selectHint")}</span>
      </div>
    </div>
  );
}
