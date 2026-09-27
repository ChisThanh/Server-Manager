import { useEffect, useMemo, useState } from "react";
import { RefreshCw, Skull, Square } from "lucide-react";
import { SystemService, errMsg, type Process } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { withSudo } from "../../store/sudo";
import { confirmDialog, toast } from "../../store/ui";
import { useT } from "../../i18n";

type SortKey = "cpu" | "mem" | "pid" | "user" | "command";

export function Processes({ connId, visible, connected }: { connId: string; visible: boolean; connected: boolean }) {
  const t = useT();
  const [list, setList] = useState<Process[] | null>(null);
  const [error, setError] = useState("");
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({ key: "cpu", desc: true });
  const [auto, setAuto] = useState(true);

  const load = async () => {
    try {
      setList((await SystemService.Processes(connId)) ?? []);
      setError("");
    } catch (e) {
      setError(errMsg(e));
    }
  };

  useEffect(() => {
    if (!visible || !connected) return;
    load();
    if (!auto) return;
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, [visible, connected, auto]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const r = (list ?? []).filter((p) => !needle || p.command.toLowerCase().includes(needle) || p.user.toLowerCase().includes(needle) || String(p.pid) === needle);
    const dir = sort.desc ? -1 : 1;
    return r.sort((a, b) => {
      const x = a[sort.key];
      const y = b[sort.key];
      return (typeof x === "number" ? x - (y as number) : String(x).localeCompare(String(y))) * dir;
    });
  }, [list, q, sort]);

  const kill = async (p: Process, signal: string) => {
    const ok = await confirmDialog(
      signal === "KILL" ? t("proc.killTitle") : t("proc.stopTitle"),
      `PID ${p.pid} (${p.user})\n${p.command}`,
      signal === "KILL" ? t("proc.killBtn") : t("proc.stopBtn"),
      true,
    );
    if (!ok) return;
    try {
      await SystemService.Kill(connId, p.pid, signal, false, "");
    } catch (e) {
      if (!/not permitted|permission/i.test(errMsg(e))) {
        toast(errMsg(e), "error");
        return;
      }
      if (!(await confirmDialog(t("sudo.retryTitle"), t("sudo.retryMsg"), t("common.useSudo")))) return;
      try {
        await withSudo(connId, (pw) => SystemService.Kill(connId, p.pid, signal, true, pw));
      } catch (e2) {
        toast(errMsg(e2), "error");
        return;
      }
    }
    toast(t("proc.sent", { sig: signal, pid: p.pid }), "success");
    setTimeout(load, 500);
  };

  const th = (key: SortKey, label: string, num = false) => (
    <th className={`sortable ${num ? "num" : ""}`} onClick={() => setSort((s) => ({ key, desc: s.key === key ? !s.desc : key !== "user" && key !== "command" }))}>
      {label}
      {sort.key === key ? (sort.desc ? " ↓" : " ↑") : ""}
    </th>
  );

  return (
    <div className="sys-page">
      <div className="toolbar">
        <input className="input" placeholder={t("proc.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <label className="check">
          <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} /> {t("proc.auto")}
        </label>
        <div className="grow" />
        {error && <span className="badge err">{error}</span>}
        <span className="muted">{t("proc.count", { n: rows.length })}</span>
        <button className="icon-btn" onClick={load} title={t("common.refresh")}>
          <RefreshCw size={14} />
        </button>
      </div>
      <div className="table-wrap">
        <table className="grid">
          <thead>
            <tr>
              {th("pid", "PID", true)}
              {th("user", t("proc.user"))}
              {th("cpu", "CPU %", true)}
              {th("mem", "RAM %", true)}
              <th className="num">RSS</th>
              <th>{t("proc.time")}</th>
              {th("command", t("proc.cmd"))}
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((p) => (
              <tr key={p.pid}>
                <td className="num">{p.pid}</td>
                <td>{p.user}</td>
                <td className="num">{p.cpu.toFixed(1)}</td>
                <td className="num">{p.mem.toFixed(1)}</td>
                <td className="num">{formatBytes(p.rss)}</td>
                <td className="muted">{p.elapsed}</td>
                <td className="cmd" title={p.command}>
                  {p.command}
                </td>
                <td style={{ width: 60 }}>
                  <div style={{ display: "flex" }}>
                    <button className="icon-btn" title={t("proc.stop")} onClick={() => kill(p, "TERM")}>
                      <Square size={12} />
                    </button>
                    <button className="icon-btn" title={t("proc.kill")} onClick={() => kill(p, "KILL")}>
                      <Skull size={13} />
                    </button>
                  </div>
                </td>
              </tr>
            ))}
            {list !== null && rows.length === 0 && (
              <tr>
                <td colSpan={8} className="muted" style={{ textAlign: "center", padding: 24 }}>
                  {t("app.noMatch")}
                </td>
              </tr>
            )}
          </tbody>
        </table>
        {list === null && !error && (
          <div style={{ padding: 20, display: "grid", placeItems: "center" }}>
            <span className="spinner lg" />
          </div>
        )}
      </div>
    </div>
  );
}
