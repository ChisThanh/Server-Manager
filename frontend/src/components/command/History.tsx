import { useState } from "react";
import { RefreshCw, Search, Trash2 } from "lucide-react";
import { CommandService } from "../../../bindings/server-manager/services/command";
import type { HistoryItem } from "../../../bindings/server-manager/services/command";
import { useRemote } from "../../ui/hooks";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { useT } from "../../i18n";
import { useCmd } from "./store";
import { presetLabel } from "./ActionPanel";
import { firstLine, formatMs, formatTime } from "./util";

function title(h: HistoryItem): string {
  if (h.kind === "preset") return presetLabel(h.preset);
  if (h.snippet) return h.snippet;
  return firstLine(h.title);
}

export function History({ onOpen }: { onOpen: (id: string) => void }) {
  const t = useT();
  const version = useCmd((s) => s.historyVersion);
  const [q, setQ] = useState("");
  const list = useRemote(async () => (await CommandService.History(200)) ?? [], [version]);
  const items = (list.data ?? []).filter((h) => {
    const n = q.trim().toLowerCase();
    return !n || [h.title, h.actor, h.snippet, presetLabel(h.preset)].some((v) => v.toLowerCase().includes(n));
  });

  const remove = async (h: HistoryItem) => {
    try {
      await CommandService.DeleteHistory(h.id);
      list.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };
  const clear = async () => {
    if (!(await confirmDialog(t("cmd.historyClearTitle"), t("cmd.historyClearMsg"), t("common.delete"), true))) return;
    try {
      await CommandService.DeleteHistory("");
      list.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  if (list.error && !list.data) return <ErrorBox error={list.error} onRetry={list.reload} />;
  if (!list.data) return <Loading />;
  return (
    <div>
      <div className="toolbar">
        <div className="cmd-search">
          <Search size={13} />
          <input className="input input-sm" placeholder={t("cmd.historySearch")} value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <div className="grow" />
        <button className="btn sm ghost" onClick={list.reload} disabled={list.loading}>
          <RefreshCw size={13} className={list.loading ? "spin-icon" : ""} /> {t("common.refresh")}
        </button>
        {list.data.length > 0 && (
          <button className="btn sm ghost" onClick={clear}>
            <Trash2 size={13} /> {t("cmd.historyClear")}
          </button>
        )}
      </div>
      {list.data.length === 0 ? (
        <Empty title={t("cmd.historyEmpty")} text={t("cmd.historyEmptyHint")} />
      ) : (
        <div className="table-wrap">
          <table className="grid cmd-history">
            <thead>
              <tr>
                <th>{t("cmd.col.time")}</th>
                <th>{t("cmd.col.command")}</th>
                <th className="num">{t("cmd.col.targets")}</th>
                <th>{t("cmd.col.success")}</th>
                <th>{t("cmd.col.status")}</th>
                <th className="num">{t("cmd.col.duration")}</th>
                <th>{t("cmd.col.actor")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.length === 0 && (
                <tr>
                  <td colSpan={8} className="muted cmd-empty">
                    {t("cmd.noMatch")}
                  </td>
                </tr>
              )}
              {items.map((h) => {
                const s = h.summary;
                const pct = s.total ? Math.round((s.ok / s.total) * 100) : 0;
                const tone = h.status === "done" ? "ok" : h.status === "running" ? "info" : h.status === "cancelled" || h.status === "interrupted" ? "muted" : "err";
                return (
                  <tr key={h.id} className="clickable" onClick={() => onOpen(h.id)}>
                    <td className="muted">{formatTime(h.started)}</td>
                    <td className="cmd" title={h.title}>
                      {h.kind === "preset" && <span className="badge">{t("cmd.kind.preset")}</span>} {title(h)}
                    </td>
                    <td className="num">{s.total}</td>
                    <td>
                      <div className="cmd-ratio">
                        <div className="cmd-ratio-bar">
                          <div style={{ width: `${pct}%` }} />
                        </div>
                        <span>
                          {s.ok}/{s.total}
                        </span>
                      </div>
                    </td>
                    <td>
                      <StateBadge tone={tone}>{t(`cmd.status.${h.status}` as "cmd.status.done")}</StateBadge>
                    </td>
                    <td className="num">{h.finished ? formatMs(h.finished - h.started) : ""}</td>
                    <td className="muted">{h.actor}</td>
                    <td>
                      {h.status !== "running" && (
                        <button
                          className="icon-btn"
                          title={t("common.delete")}
                          onClick={(e) => {
                            e.stopPropagation();
                            remove(h);
                          }}
                        >
                          <Trash2 size={13} />
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
