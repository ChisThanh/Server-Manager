import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { CornerDownLeft, Trash2 } from "lucide-react";
import { confirmDanger } from "../../ui/confirm";
import { errCode, errMsg } from "../../lib/api";
import { confirmDialog } from "../../store/ui";
import { useT } from "../../i18n";
import { DatabaseService, dbs, ms, type TargetViewProps } from "./common";
import { Select } from "../../ui/Select";

interface Entry {
  id: number;
  db: number;
  cmd: string;
  output: string;
  error: boolean;
  write: boolean;
  elapsed: number;
}

const EXAMPLES = [
  "INFO memory",
  "DBSIZE",
  "SCAN 0 COUNT 100",
  "SLOWLOG GET 10",
  "CONFIG GET maxmemory*",
  "CLIENT LIST",
  "MEMORY STATS",
];
let seq = 1;

export function RedisConsole({
  connId,
  target,
  canEdit,
  dbCount,
}: TargetViewProps & { dbCount: number }) {
  const t = useT();
  const [db, setDb] = useState(() => Number(target.database) || 0);
  const [line, setLine] = useState("");
  const [entries, setEntries] = useState<Entry[]>([]);
  const [busy, setBusy] = useState(false);
  const hist = useRef<string[]>([]);
  const hpos = useRef(-1);
  const outRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    outRef.current?.scrollTo({ top: outRef.current.scrollHeight });
  }, [entries]);

  const push = (e: Omit<Entry, "id">) =>
    setEntries((l) => [...l.slice(-199), { ...e, id: seq++ }]);

  const exec = async (cmd: string, allowWrite: boolean): Promise<void> => {
    try {
      const r = await dbs(connId, (pw) =>
        DatabaseService.RedisCommand(
          connId,
          target.id,
          db,
          cmd,
          allowWrite,
          pw,
        ),
      );
      push({
        db,
        cmd,
        output: r.output,
        error: r.isError,
        write: r.write,
        elapsed: r.elapsed,
      });
    } catch (e) {
      const code = errCode(e);
      if (code === "db.redis.needConfirm" && !allowWrite) {
        const ok = await confirmDanger({
          serverId: connId,
          title: t("db.rd.confirmWrite"),
          message: (
            <>
              <div>{t("db.rd.confirmWriteMsg", { db })}</div>
              <pre className="db-confirm-sql">{cmd}</pre>
            </>
          ),
          confirmText: t("db.rd.runIt"),
        });
        if (ok) return exec(cmd, true);
        push({
          db,
          cmd,
          output: t("db.rd.notRun"),
          error: true,
          write: true,
          elapsed: 0,
        });
        return;
      }
      if (code === "db.redis.keysLarge" && !allowWrite) {
        if (
          await confirmDialog(
            t("db.rd.keysLargeQ"),
            errMsg(e) + "\n\n" + t("db.rd.keysLargeMsg"),
            t("db.rd.runIt"),
            true,
          )
        )
          return exec(cmd, true);
        push({
          db,
          cmd,
          output: t("db.rd.notRun"),
          error: true,
          write: false,
          elapsed: 0,
        });
        return;
      }
      push({
        db,
        cmd,
        output: errMsg(e),
        error: true,
        write: false,
        elapsed: 0,
      });
    }
  };

  const submit = async () => {
    const cmd = line.trim();
    if (!cmd || busy) return;
    hist.current = [cmd, ...hist.current.filter((h) => h !== cmd)].slice(
      0,
      100,
    );
    hpos.current = -1;
    setLine("");
    setBusy(true);
    await exec(cmd, false);
    setBusy(false);
    inputRef.current?.focus();
  };

  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      submit();
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      if (hpos.current + 1 < hist.current.length) {
        hpos.current++;
        setLine(hist.current[hpos.current]);
      }
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      if (hpos.current > 0) {
        hpos.current--;
        setLine(hist.current[hpos.current]);
      } else {
        hpos.current = -1;
        setLine("");
      }
    }
  };

  if (!canEdit) return <div className="hint-box">{t("db.noPermQuery")}</div>;

  return (
    <div className="db-redis">
      <div className="hint-box">{t("db.rd.consoleHint")}</div>
      <div className="db-redis-out" ref={outRef}>
        {entries.length === 0 && (
          <div className="muted">
            {t("db.rd.try")}{" "}
            <span className="chips db-inline-chips">
              {EXAMPLES.map((x) => (
                <button
                  key={x}
                  className="chip db-chip-btn"
                  onClick={() => {
                    setLine(x);
                    inputRef.current?.focus();
                  }}
                >
                  {x}
                </button>
              ))}
            </span>
          </div>
        )}
        {entries.map((e) => (
          <div key={e.id} className="db-redis-entry">
            <div className="db-redis-cmd">
              <span className="muted">db{e.db}&gt;</span> {e.cmd}
              {e.write && (
                <span className="badge warn">{t("db.q.writeShort")}</span>
              )}
              {e.elapsed > 0 && (
                <span className="muted db-redis-ms">{ms(e.elapsed)}</span>
              )}
            </div>
            <pre className={e.error ? "db-redis-err" : ""}>{e.output}</pre>
          </div>
        ))}
      </div>
      <div className="db-redis-input">
        <Select
          className="mono"
          value={db}
          onChange={setDb}
          title={t("db.rd.db")}
          options={Array.from({ length: Math.max(16, dbCount) }, (_, i) => ({ value: i, label: `db${i}` }))}
        />
        <input
          ref={inputRef}
          className="input mono"
          value={line}
          placeholder={t("db.rd.placeholder")}
          onChange={(e) => setLine(e.target.value)}
          onKeyDown={onKey}
          spellCheck={false}
          autoFocus
        />
        <button
          className="btn primary"
          onClick={submit}
          disabled={busy || !line.trim()}
        >
          {busy ? <span className="spinner" /> : <CornerDownLeft size={13} />}
        </button>
        <button
          className="icon-btn"
          onClick={() => setEntries([])}
          title={t("db.rd.clear")}
        >
          <Trash2 size={14} />
        </button>
      </div>
    </div>
  );
}
