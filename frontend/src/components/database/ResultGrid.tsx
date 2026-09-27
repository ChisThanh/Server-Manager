import { useMemo, useRef, useState } from "react";
import { VirtualRows } from "../../ui/VirtualRows";
import { Copy } from "lucide-react";
import type { StatementResult } from "./common";
import { Modal } from "../Overlays";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";

const MAX_CELL = 200;

function csvField(v: string | null): string {
  if (v === null) return "";
  return /[",\n\r]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v;
}

/** Result set of one statement: columns + rows, NULL shown distinctly. */
export function ResultGrid({ result }: { result: StatementResult }) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const t = useT();
  const [cellView, setCellView] = useState<{
    col: string;
    value: string;
  } | null>(null);
  const cols = result.columns ?? [];
  const rows = (result.rows ?? []) as (string | null)[][];
  const numeric = useMemo(
    () =>
      cols.map(
        (_, i) =>
          rows.length > 0 &&
          rows.every(
            (r) =>
              r[i] === null ||
              r[i] === undefined ||
              /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i.test(r[i] ?? ""),
          ),
      ),
    [result],
  );

  const copyCSV = async () => {
    const text = [
      cols.map(csvField).join(","),
      ...rows.map((r) => r.map((v) => csvField(v ?? null)).join(",")),
    ].join("\n");
    try {
      await navigator.clipboard.writeText(text);
      toast(t("db.q.copied", { n: rows.length }), "success");
    } catch {
      toast(t("db.q.copyFailed"), "error");
    }
  };

  if (!result.hasRows) {
    return (
      <div className="db-affected">
        {result.command || "OK"}
        {result.rowCount >= 0 && (
          <span className="muted">
            {" "}
            · {t("db.q.affected", { n: result.rowCount })}
          </span>
        )}
      </div>
    );
  }
  return (
    <div className="db-result">
      <div className="db-result-bar">
        <span className="muted">
          {result.truncated
            ? t("db.q.truncated", {
                shown: rows.length,
                total: result.rowCount,
              })
            : t("db.q.rows", { n: result.rowCount })}
        </span>
        <div className="grow" />
        {rows.length > 0 && (
          <button className="btn ghost sm" onClick={copyCSV}>
            <Copy size={12} /> CSV
          </button>
        )}
      </div>
      {cols.length === 0 ? (
        <div className="muted db-pad">{t("db.q.noRows")}</div>
      ) : (
        <div className="db-grid-wrap" ref={wrapRef}>
          <table className="grid db-grid">
            <thead>
              <tr>
                <th className="num db-rownum">#</th>
                {cols.map((c, i) => (
                  <th key={i} className={numeric[i] ? "num" : ""}>
                    {c || <span className="muted">?column?</span>}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              <VirtualRows
                items={rows}
                itemKey={(_r, ri) => ri}
                scrollRef={wrapRef}
                estimate={25}
                spacer="tr"
                colSpan={cols.length + 1}
                render={(r, ri) => (
                <tr key={ri}>
                  <td className="num muted db-rownum">{ri + 1}</td>
                  {cols.map((c, ci) => {
                    const v = r[ci];
                    if (v === null || v === undefined) {
                      return (
                        <td key={ci} className="db-null">
                          NULL
                        </td>
                      );
                    }
                    const long = v.length > MAX_CELL || v.includes("\n");
                    const shown =
                      v.length > MAX_CELL ? v.slice(0, MAX_CELL) + "…" : v;
                    return (
                      <td
                        key={ci}
                        className={`${numeric[ci] ? "num" : ""} ${long ? "db-long" : ""} ${v === "" ? "db-empty" : ""}`}
                        title={long ? t("db.q.cellHint") : undefined}
                        onDoubleClick={() => setCellView({ col: c, value: v })}
                      >
                        {v === ""
                          ? "''"
                          : shown.replace(/\t/g, "→").split("\n")[0]}
                        {v.includes("\n") && <span className="db-nl">↵</span>}
                      </td>
                    );
                  })}
                </tr>
                )}
              />
            </tbody>
          </table>
          {rows.length === 0 && (
            <div className="muted db-pad">{t("db.q.noRows")}</div>
          )}
        </div>
      )}
      {cellView && (
        <Modal
          title={cellView.col}
          size="wide"
          onClose={() => setCellView(null)}
          footer={
            <>
              <button
                className="btn"
                onClick={() =>
                  navigator.clipboard
                    .writeText(cellView.value)
                    .then(() => toast(t("db.copiedValue"), "success"))
                }
              >
                <Copy size={13} /> {t("db.copy")}
              </button>
              <button className="btn primary" onClick={() => setCellView(null)}>
                {t("common.close")}
              </button>
            </>
          }
        >
          <pre className="log-view db-cell-full">{cellView.value}</pre>
          <div className="muted">
            {t("db.q.chars", { n: cellView.value.length })}
          </div>
        </Modal>
      )}
    </div>
  );
}
