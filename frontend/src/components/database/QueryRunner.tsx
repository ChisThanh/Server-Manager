import { useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  BookmarkPlus,
  Bookmark,
  History,
  Lock,
  Play,
  ShieldAlert,
  Trash2,
  Unlock,
} from "lucide-react";
import { CodeEditor } from "../../ui/CodeEditor";
import { confirmDanger } from "../../ui/confirm";
import { useServer } from "../../ui/perm";
import { Empty } from "../../ui/Page";
import { Modal } from "../Overlays";
import { errMsg } from "../../lib/api";
import { formatDate } from "../../lib/format";
import { confirmDialog, promptDialog, toast } from "../../store/ui";
import { useT } from "../../i18n";
import { ResultGrid } from "./ResultGrid";
import {
  DatabaseService,
  dbs,
  ms,
  type HistoryEntry,
  type QueryResult,
  type Snippet,
  type TargetViewProps,
} from "./common";
import { Select } from "../../ui/Select";

const DRAFT_KEY = "sm.db.draft.";

function loadDraft(key: string, engine: string): string {
  try {
    const v = localStorage.getItem(DRAFT_KEY + key);
    if (v !== null) return v;
  } catch {
    /* storage unavailable */
  }
  return engine === "postgres"
    ? "SELECT datname, pg_size_pretty(pg_database_size(datname)) AS size\nFROM pg_database\nORDER BY pg_database_size(datname) DESC;"
    : "SELECT table_schema, table_name, table_rows\nFROM information_schema.tables\nWHERE table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')\nORDER BY table_rows DESC\nLIMIT 50;";
}

function saveDraft(key: string, v: string) {
  try {
    localStorage.setItem(DRAFT_KEY + key, v);
  } catch {
    /* ignore */
  }
}

export function QueryRunner({
  connId,
  target,
  active,
  canEdit,
  initialDb,
}: TargetViewProps & { initialDb?: string }) {
  const t = useT();
  const server = useServer(connId);
  const production = server?.environment === "production";
  const draftKey = `${connId}.${target.id}`;
  const [sql, setSql] = useState(() => loadDraft(draftKey, target.engine));
  const [database, setDatabase] = useState(
    initialDb ?? (target.database || ""),
  );
  const [databases, setDatabases] = useState<string[]>([]);
  const [allowWrite, setAllowWrite] = useState(false);
  const [timeout, setTimeoutSec] = useState(30);
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<QueryResult | null>(null);
  const [error, setError] = useState("");
  const [sel, setSel] = useState(0);
  const [panel, setPanel] = useState<"history" | "snippets" | null>(null);
  const sqlRef = useRef(sql);
  sqlRef.current = sql;

  useEffect(() => {
    setSql(loadDraft(draftKey, target.engine));
    setDatabase(initialDb ?? (target.database || ""));
    setAllowWrite(false);
    setResult(null);
    setError("");
  }, [draftKey]);

  useEffect(() => {
    if (!active) return;
    let cancelled = false;
    dbs(connId, (pw) => DatabaseService.Databases(connId, target.id, pw))
      .then(
        (list) => !cancelled && setDatabases((list ?? []).map((d) => d.name)),
      )
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [connId, target.id, active]);

  const onChange = (v: string) => {
    setSql(v);
    saveDraft(draftKey, v);
  };

  const toggleWrite = async () => {
    if (allowWrite) {
      setAllowWrite(false);
      return;
    }
    const ok = await confirmDanger({
      serverId: connId,
      title: t("db.q.enableWriteQ"),
      message: t("db.q.enableWriteMsg", { name: target.name }),
      confirmText: t("db.q.enableWrite"),
    });
    if (ok) setAllowWrite(true);
  };

  const run = async () => {
    const text = sqlRef.current;
    if (running || !text.trim()) return;
    if (allowWrite && production) {
      const ok = await confirmDanger({
        serverId: connId,
        title: t("db.q.runWriteQ"),
        message: <pre className="db-confirm-sql">{text.slice(0, 600)}</pre>,
        confirmText: t("db.q.run"),
      });
      if (!ok) return;
    }
    setRunning(true);
    setError("");
    try {
      const r = await dbs(connId, (pw) =>
        DatabaseService.RunQuery(
          connId,
          {
            targetId: target.id,
            database,
            sql: text,
            allowWrite,
            timeoutSec: timeout,
            maxRows: 1000,
          },
          pw,
        ),
      );
      setResult(r);
      const stmts = r.statements ?? [];
      let pick = stmts.length - 1;
      for (let i = stmts.length - 1; i >= 0; i--) {
        if (stmts[i].hasRows) {
          pick = i;
          break;
        }
      }
      setSel(Math.max(0, pick));
    } catch (e) {
      setResult(null);
      setError(errMsg(e));
    } finally {
      setRunning(false);
    }
  };

  const stmts = result?.statements ?? [];
  const current = stmts[sel];

  return (
    <div className="db-query">
      <div className="toolbar db-query-bar">
        <Select
          value={database}
          onChange={setDatabase}
          title={t("db.q.database")}
          options={[
            { value: "", label: target.engine === "postgres" ? t("db.q.defaultDbPg", { db: target.database || "postgres" }) : t("db.q.noDb") },
            ...databases,
          ]}
        />
        <button
          className={`btn sm ${allowWrite ? "danger" : ""}`}
          onClick={toggleWrite}
          disabled={!canEdit}
          title={allowWrite ? t("db.q.writeOnHint") : t("db.q.readOnlyHint")}
        >
          {allowWrite ? <Unlock size={13} /> : <Lock size={13} />}{" "}
          {allowWrite ? t("db.q.writeMode") : t("db.q.readOnly")}
        </button>
        <Select
          value={timeout}
          onChange={setTimeoutSec}
          title={t("db.q.timeout")}
          options={[10, 30, 60, 300, 900].map((s) => ({ value: s, label: t("db.q.timeoutOpt", { s: s >= 60 ? `${s / 60}m` : `${s}s` }) }))}
        />
        <div className="grow" />
        <button className="btn ghost sm" onClick={() => setPanel("history")}>
          <History size={13} /> {t("db.q.history")}
        </button>
        <button className="btn ghost sm" onClick={() => setPanel("snippets")}>
          <Bookmark size={13} /> {t("db.q.snippets")}
        </button>
        <button
          className="btn primary"
          onClick={run}
          disabled={running || !canEdit || !sql.trim()}
          title="⌘/Ctrl + Enter"
        >
          {running ? <span className="spinner" /> : <Play size={13} />}{" "}
          {t("db.q.run")}
        </button>
      </div>
      {!canEdit && <div className="hint-box">{t("db.noPermQuery")}</div>}
      {allowWrite && (
        <div className="hint-box db-write-warn">
          <ShieldAlert size={14} /> {t("db.q.writeBanner")}
        </div>
      )}
      <CodeEditor
        value={sql}
        onChange={onChange}
        language="sql"
        height={220}
        onSubmit={run}
      />
      <div className="db-query-status muted">
        {running ? (
          <>
            <span className="spinner" /> {t("db.q.running")}
          </>
        ) : result ? (
          <>
            {t("db.q.done", { n: stmts.length, ms: ms(result.elapsed) })}
            {result.readOnly && (
              <span className="badge">{t("db.q.readOnly")}</span>
            )}
          </>
        ) : (
          t("db.q.hint")
        )}
      </div>
      {error && (
        <div className="db-error">
          <AlertTriangle size={14} />
          <pre>{error}</pre>
        </div>
      )}
      {result && result.errorIndex > 0 && (
        <div className="db-error">
          <AlertTriangle size={14} />
          <div>
            <div className="db-error-title">
              {t("db.q.stmtFailed", { n: result.errorIndex })}
              {result.errorIndex > 1 && !result.readOnly && (
                <span className="muted"> · {t("db.q.earlierCommitted")}</span>
              )}
            </div>
            <pre>{result.error}</pre>
            {result.outputTruncated && (
              <div className="muted">{t("db.q.outputTruncated")}</div>
            )}
          </div>
        </div>
      )}
      {stmts.length > 1 && (
        <div className="segmented db-stmt-tabs">
          {stmts.map((s, i) => (
            <button
              key={i}
              className={sel === i ? "on" : ""}
              onClick={() => setSel(i)}
              title={s.sql}
            >
              {s.index}. {s.command || "SQL"}
              {s.hasRows ? ` (${s.rowCount})` : ""}
            </button>
          ))}
        </div>
      )}
      {current && <ResultGrid result={current} />}
      {panel === "history" && (
        <HistoryModal
          connId={connId}
          targetId={target.id}
          onPick={(h) => {
            onChange(h.sql);
            if (h.database) setDatabase(h.database);
            setPanel(null);
          }}
          onClose={() => setPanel(null)}
        />
      )}
      {panel === "snippets" && (
        <SnippetsModal
          engine={target.engine}
          current={sql}
          onPick={(s) => {
            onChange(s.sql);
            setPanel(null);
          }}
          onClose={() => setPanel(null)}
        />
      )}
    </div>
  );
}

function HistoryModal(props: {
  connId: string;
  targetId: string;
  onPick: (h: HistoryEntry) => void;
  onClose: () => void;
}) {
  const t = useT();
  const [list, setList] = useState<HistoryEntry[] | null>(null);
  const [q, setQ] = useState("");
  const load = () =>
    DatabaseService.History(props.connId, props.targetId)
      .then((l) => setList(l ?? []))
      .catch((e) => toast(errMsg(e), "error"));
  useEffect(() => {
    load();
  }, []);
  const rows = useMemo(
    () =>
      (list ?? []).filter(
        (h) => !q || h.sql.toLowerCase().includes(q.toLowerCase()),
      ),
    [list, q],
  );
  const clear = async () => {
    if (
      !(await confirmDialog(
        t("db.q.clearHistoryQ"),
        undefined,
        t("common.delete"),
        true,
      ))
    )
      return;
    await DatabaseService.ClearHistory(props.connId, props.targetId);
    load();
  };
  return (
    <Modal
      title={t("db.q.history")}
      size="xwide"
      onClose={props.onClose}
      footer={
        <>
          <button
            className="btn ghost"
            onClick={clear}
            disabled={!list?.length}
          >
            <Trash2 size={13} /> {t("db.q.clearHistory")}
          </button>
          <div className="grow" />
          <button className="btn" onClick={props.onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      <input
        className="input db-modal-filter"
        placeholder={t("db.filter")}
        value={q}
        onChange={(e) => setQ(e.target.value)}
        autoFocus
      />
      {list === null ? (
        <span className="spinner" />
      ) : rows.length === 0 ? (
        <Empty text={t("db.q.noHistory")} />
      ) : (
        <div className="list-rows db-history">
          {rows.map((h) => (
            <div
              key={h.id}
              className="list-row clickable"
              onClick={() => props.onPick(h)}
            >
              <span className={`badge ${h.ok ? "ok" : "err"}`}>
                {h.ok ? "OK" : "ERR"}
              </span>
              <span className="grow mono" title={h.error || h.sql}>
                {h.sql.replace(/\s+/g, " ").slice(0, 200)}
              </span>
              {!h.readOnly && (
                <span className="badge warn">{t("db.q.writeShort")}</span>
              )}
              {h.database && <span className="chip">{h.database}</span>}
              <span className="muted db-nowrap">{ms(h.elapsed)}</span>
              <span className="muted db-nowrap">
                {formatDate(Math.floor(h.ts / 1000))}
              </span>
            </div>
          ))}
        </div>
      )}
    </Modal>
  );
}

function SnippetsModal(props: {
  engine: string;
  current: string;
  onPick: (s: Snippet) => void;
  onClose: () => void;
}) {
  const t = useT();
  const [list, setList] = useState<Snippet[] | null>(null);
  const load = () =>
    DatabaseService.Snippets(props.engine)
      .then((l) => setList(l ?? []))
      .catch((e) => toast(errMsg(e), "error"));
  useEffect(() => {
    load();
  }, []);
  const save = async () => {
    const name = await promptDialog(
      t("db.q.saveSnippet"),
      t("db.q.snippetName"),
      "",
      { confirmText: t("common.save") },
    );
    if (!name?.trim()) return;
    try {
      await DatabaseService.SaveSnippet({
        id: "",
        name: name.trim(),
        engine: props.engine,
        sql: props.current,
        updated: 0,
      });
      toast(t("db.q.snippetSaved"), "success");
      load();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  const del = async (s: Snippet) => {
    if (
      !(await confirmDialog(
        t("db.q.deleteSnippetQ", { name: s.name }),
        undefined,
        t("common.delete"),
        true,
      ))
    )
      return;
    await DatabaseService.DeleteSnippet(s.id);
    load();
  };
  return (
    <Modal
      title={t("db.q.snippets")}
      size="wide"
      onClose={props.onClose}
      footer={
        <>
          <button
            className="btn"
            onClick={save}
            disabled={!props.current.trim()}
          >
            <BookmarkPlus size={13} /> {t("db.q.saveCurrent")}
          </button>
          <div className="grow" />
          <button className="btn" onClick={props.onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      {list === null ? (
        <span className="spinner" />
      ) : list.length === 0 ? (
        <Empty text={t("db.q.noSnippets")} />
      ) : (
        <div className="list-rows">
          {list.map((s) => (
            <div
              key={s.id}
              className="list-row clickable"
              onClick={() => props.onPick(s)}
            >
              <Bookmark size={13} />
              <span className="grow">
                <b>{s.name}</b>{" "}
                <span className="muted mono">
                  {s.sql.replace(/\s+/g, " ").slice(0, 120)}
                </span>
              </span>
              <button
                className="icon-btn"
                title={t("common.delete")}
                onClick={(e) => {
                  e.stopPropagation();
                  del(s);
                }}
              >
                <Trash2 size={13} />
              </button>
            </div>
          ))}
        </div>
      )}
    </Modal>
  );
}
