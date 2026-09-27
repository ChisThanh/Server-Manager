import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { SearchAddon } from "@xterm/addon-search";
import "@xterm/xterm/css/xterm.css";
import { attachImeInput } from "./imeInput";
import { Browser, Events } from "@wailsio/runtime";
import { ChevronDown, ChevronUp, Plus, RotateCw, Search, X } from "lucide-react";
import { TerminalService, errMsg, formatAppError } from "../../lib/api";
import { t as tr, useT } from "../../i18n";
import { toast } from "../../store/ui";

/** Console to open instead of a login shell. */
export interface TermExec {
  kind: "docker" | "psql" | "mysql" | "redis";
  target: string;
  title: string;
}

export interface TermRequest {
  cwd: string;
  nonce: number;
  exec?: TermExec;
}

interface TermTab {
  key: number;
  title: string;
  cwd: string;
  exec?: TermExec;
  exited: boolean;
}

let keySeq = 1;

export function TerminalPanel(props: { connId: string; visible: boolean; connected: boolean; request?: TermRequest; defaultCwd: string }) {
  const t = useT();
  const [tabs, setTabs] = useState<TermTab[]>([]);
  const [active, setActive] = useState<number | null>(null);
  const lastNonce = useRef(0);

  const add = (cwd: string, exec?: TermExec) => {
    const tab = { key: keySeq++, title: exec ? exec.title : cwd.split("/").pop() || "/", cwd, exec, exited: false };
    setTabs((ts) => [...ts, tab]);
    setActive(tab.key);
  };

  // Open a first shell the first time the panel becomes visible.
  useEffect(() => {
    if (props.visible && props.connected && tabs.length === 0 && !props.request) add(props.defaultCwd);
  }, [props.visible, props.connected]);

  // "Open terminal here" from the file explorer.
  useEffect(() => {
    if (props.request && props.request.nonce !== lastNonce.current && props.connected) {
      lastNonce.current = props.request.nonce;
      add(props.request.cwd, props.request.exec);
    }
  }, [props.request?.nonce, props.connected]);

  const close = (key: number) => {
    setTabs((ts) => {
      const idx = ts.findIndex((t) => t.key === key);
      const next = ts.filter((t) => t.key !== key);
      if (active === key) setActive(next[Math.max(0, idx - 1)]?.key ?? null);
      return next;
    });
  };

  return (
    <div className="term-wrap">
      <div className="term-tabs">
        {tabs.map((tab, i) => (
          <div key={tab.key} className={`term-tab ${tab.key === active ? "active" : ""} ${tab.exited ? "exited" : ""}`} onClick={() => setActive(tab.key)}>
            <span>
              {i + 1}: {tab.title}
              {tab.exited ? " " + t("term.exited") : ""}
            </span>
            <button
              className="icon-btn"
              style={{ width: 18, height: 18 }}
              onClick={(e) => {
                e.stopPropagation();
                close(tab.key);
              }}
            >
              <X size={12} />
            </button>
          </div>
        ))}
        <button className="icon-btn" title={t("term.new")} onClick={() => add(props.defaultCwd)} disabled={!props.connected}>
          <Plus size={15} />
        </button>
      </div>
      <div className="term-host">
        {tabs.map((tab) => (
          <TermView
            key={tab.key}
            connId={props.connId}
            cwd={tab.cwd}
            exec={tab.exec}
            visible={props.visible && tab.key === active}
            onExit={() => setTabs((ts) => ts.map((x) => (x.key === tab.key ? { ...x, exited: true } : x)))}
            onRestart={() => {
              close(tab.key);
              add(tab.cwd, tab.exec);
            }}
          />
        ))}
        {tabs.length === 0 && (
          <div className="center-msg">
            <button className="btn" onClick={() => add(props.defaultCwd)} disabled={!props.connected}>
              <Plus size={14} /> {t("term.open")}
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

function TermView(props: { connId: string; cwd: string; exec?: TermExec; visible: boolean; onExit: () => void; onRestart: () => void }) {
  const t = useT();
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const idRef = useRef<string | null>(null);
  const searchRef = useRef<SearchAddon | null>(null);
  const [exited, setExited] = useState(false);
  const [search, setSearch] = useState<string | null>(null);
  const [noMatch, setNoMatch] = useState(false);
  const onExitRef = useRef(props.onExit);
  onExitRef.current = props.onExit;

  useEffect(() => {
    const term = new Terminal({
      fontFamily: "'SF Mono', Menlo, Monaco, Consolas, 'Liberation Mono', monospace",
      fontSize: 13,
      lineHeight: 1.15,
      cursorBlink: true,
      scrollback: 10000,
      allowProposedApi: true,
      macOptionIsMeta: true,
      theme: {
        background: "#121214",
        foreground: "#e4e4e7",
        cursor: "#4f8cff",
        selectionBackground: "#3a4a6b",
        black: "#18181b",
        brightBlack: "#5f5f68",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    const searchAddon = new SearchAddon();
    term.loadAddon(searchAddon);
    searchRef.current = searchAddon;
    term.loadAddon(new WebLinksAddon((_e, uri) => Browser.OpenURL(uri)));
    term.open(hostRef.current!);
    termRef.current = term;
    fitRef.current = fit;
    try {
      fit.fit();
    } catch {
      /* not visible yet */
    }

    let disposed = false;
    // Keystrokes are sent one request at a time, batching whatever was typed
    // meanwhile, so input can never arrive at the server out of order.
    let pending = "";
    let inFlight = false;
    const flush = async () => {
      if (inFlight || !pending || !idRef.current) return;
      inFlight = true;
      const chunk = pending;
      pending = "";
      try {
        await TerminalService.Write(idRef.current, chunk);
      } catch {
        /* session gone; exit event handles UI */
      }
      inFlight = false;
      flush();
    };
    const send = (d: string) => {
      pending += d;
      flush();
    };
    const onData = term.onData(send);
    // Typed text goes through the IME-aware layer (Vietnamese Telex/VNI etc.).
    const detachIme = attachImeInput(term, send, true);

    const offData = Events.On("term:data", (ev) => {
      if (ev.data.id !== idRef.current) return;
      const bin = atob(ev.data.data);
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      term.write(bytes);
    });
    const offExit = Events.On("term:exit", (ev) => {
      if (ev.data.id !== idRef.current) return;
      const why = formatAppError(ev.data.error);
      term.write(`\r\n\x1b[90m[${tr("term.ended")}${why ? ": " + why : ""}]\x1b[0m\r\n`);
      idRef.current = null;
      setExited(true);
      onExitRef.current();
    });

    const onResize = term.onResize(({ cols, rows }) => {
      if (idRef.current) TerminalService.Resize(idRef.current, cols, rows).catch(() => {});
    });

    (props.exec
      ? TerminalService.OpenExec(props.connId, props.exec.kind, props.exec.target, term.cols, term.rows)
      : TerminalService.Open(props.connId, props.cwd, term.cols, term.rows)
    )
      .then((id) => {
        if (disposed) {
          TerminalService.Close(id);
          return;
        }
        idRef.current = id;
        flush();
      })
      .catch((e) => {
        term.write(`\x1b[31m${tr("term.openFailed", { error: errMsg(e) })}\x1b[0m\r\n`);
        setExited(true);
        onExitRef.current();
      });

    const ro = new ResizeObserver(() => {
      if (hostRef.current && hostRef.current.offsetWidth > 0) {
        try {
          fit.fit();
        } catch {
          /* ignore */
        }
      }
    });
    ro.observe(hostRef.current!);

    return () => {
      disposed = true;
      ro.disconnect();
      detachIme();
      onData.dispose();
      onResize.dispose();
      offData();
      offExit();
      if (idRef.current) TerminalService.Close(idRef.current);
      term.dispose();
    };
  }, []);

  useEffect(() => {
    if (!props.visible) return;
    requestAnimationFrame(() => {
      try {
        fitRef.current?.fit();
      } catch {
        /* ignore */
      }
      termRef.current?.focus();
    });
  }, [props.visible]);

  const find = (q: string, back = false) => {
    const opts = { decorations: { matchOverviewRuler: "#f5a524", activeMatchColorOverviewRuler: "#4f8cff", matchBackground: "#5a4a1a", activeMatchBackground: "#2a4a8a" } };
    const ok = q ? (back ? searchRef.current?.findPrevious(q, opts) : searchRef.current?.findNext(q, opts)) : false;
    setNoMatch(!!q && !ok);
  };
  const closeSearch = () => {
    setSearch(null);
    searchRef.current?.clearDecorations();
    termRef.current?.focus();
  };

  return (
    <>
      {search !== null && props.visible && (
        <div className="term-search">
          <Search size={13} className="muted" />
          <input
            autoFocus
            className={`input input-sm ${noMatch ? "no-match" : ""}`}
            placeholder={t("term.searchPh")}
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              find(e.target.value);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") find(search, e.shiftKey);
              if (e.key === "Escape") closeSearch();
            }}
          />
          {noMatch && <span className="muted">{t("term.noMatch")}</span>}
          <button className="icon-btn" onClick={() => find(search, true)}>
            <ChevronUp size={14} />
          </button>
          <button className="icon-btn" onClick={() => find(search)}>
            <ChevronDown size={14} />
          </button>
          <button className="icon-btn" onClick={closeSearch}>
            <X size={14} />
          </button>
        </div>
      )}
      <div
        ref={hostRef}
        className={`term-inner ${props.visible ? "" : "hidden"}`}
        onKeyDownCapture={(e) => {
          // ⌘F / Ctrl+Shift+F opens search (plain Ctrl+F belongs to the shell).
          if (e.key.toLowerCase() === "f" && ((e.metaKey && !e.ctrlKey) || (e.ctrlKey && e.shiftKey))) {
            e.preventDefault();
            e.stopPropagation();
            setSearch(termRef.current?.getSelection() || search || "");
          }
        }}
        onContextMenu={(e) => {
        e.preventDefault();
        const t = termRef.current;
        if (t?.hasSelection()) {
          navigator.clipboard.writeText(t.getSelection());
          toast(tr("common.copied"));
        } else {
          navigator.clipboard.readText().then((txt) => txt && t?.paste(txt)).catch(() => {});
        }
      }} />
      {exited && props.visible && (
        <button className="btn sm" style={{ position: "absolute", right: 16, top: 12, zIndex: 3 }} onClick={props.onRestart}>
          <RotateCw size={13} /> {t("term.reopen")}
        </button>
      )}
    </>
  );
}
