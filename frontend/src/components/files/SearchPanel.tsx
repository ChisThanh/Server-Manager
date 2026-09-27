import { useState } from "react";
import { FileService, errMsg, type SearchHit } from "../../lib/api";
import { basename } from "../../lib/format";
import { openFile } from "../../store/editor";
import { FileIcon } from "./icons";
import { useT } from "../../i18n";

export function SearchPanel({ connId, root }: { connId: string; root: string }) {
  const t = useT();
  const [q, setQ] = useState("");
  const [content, setContent] = useState(true);
  const [busy, setBusy] = useState(false);
  const [hits, setHits] = useState<SearchHit[] | null>(null);
  const [error, setError] = useState("");

  const run = async () => {
    if (!q.trim()) return;
    setBusy(true);
    setError("");
    try {
      setHits((await FileService.Search(connId, root, q, content)) ?? []);
    } catch (e) {
      setError(errMsg(e));
      setHits(null);
    } finally {
      setBusy(false);
    }
  };

  const rel = (p: string) => (p.startsWith(root + "/") ? p.slice(root.length + 1) : p);

  return (
    <div className="search-panel">
      <form
        className="search-form"
        onSubmit={(e) => {
          e.preventDefault();
          run();
        }}
      >
        <input className="input input-sm" autoFocus placeholder={content ? t("search.contentPh") : t("search.namePh")} value={q} onChange={(e) => setQ(e.target.value)} spellCheck={false} />
        <div className="segmented">
          <button type="button" className={content ? "on" : ""} onClick={() => setContent(true)}>
            {t("search.content")}
          </button>
          <button type="button" className={!content ? "on" : ""} onClick={() => setContent(false)}>
            {t("search.name")}
          </button>
        </div>
        <div className="muted" style={{ fontSize: 11 }}>
          {t("search.in")} <span className="mono">{root}</span> · {t("search.skip")}
        </div>
      </form>
      <div className="search-results">
        {busy && (
          <div className="tree-msg" style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <span className="spinner" /> {t("search.searching")}
          </div>
        )}
        {error && <div className="tree-msg err" title={error}><span className="txt">{error}</span></div>}
        {!busy && hits && hits.length === 0 && <div className="tree-msg">{t("search.none")}</div>}
        {!busy && hits && hits.length >= 500 && <div className="tree-msg">{t("search.capped")}</div>}
        {!busy &&
          hits?.map((h, i) => (
            <div key={i} className="search-hit" onClick={() => openFile(connId, h.path, h.line || undefined)} title={h.path}>
              <div className="p" style={{ display: "flex", gap: 6, alignItems: "center", direction: "ltr" }}>
                <FileIcon name={basename(h.path)} isDir={false} size={13} />
                <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>{rel(h.path)}</span>
              </div>
              {h.line > 0 && (
                <div className="t">
                  <span className="ln">{h.line}</span>
                  {h.text.trim()}
                </div>
              )}
            </div>
          ))}
      </div>
    </div>
  );
}
