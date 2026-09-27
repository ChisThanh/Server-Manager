import { useMemo } from "react";
import { AlertOctagon, AlertTriangle, CheckCircle2, ChevronRight, HelpCircle, Info, RefreshCw } from "lucide-react";
import { ErrorBox, Loading, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { useT } from "../../i18n";
import { SecurityService, fmtTime, statusTone, sudo, tk, type Check, type SecTab, type SecViewProps } from "./common";

const ORDER: Record<string, number> = { crit: 0, warn: 1, unknown: 2, info: 3, ok: 4 };

function StatusIcon({ s }: { s: string }) {
  switch (s) {
    case "ok":
      return <CheckCircle2 size={18} className="sec-ic ok" />;
    case "warn":
      return <AlertTriangle size={18} className="sec-ic warn" />;
    case "crit":
      return <AlertOctagon size={18} className="sec-ic crit" />;
    case "info":
      return <Info size={18} className="sec-ic info" />;
  }
  return <HelpCircle size={18} className="sec-ic muted" />;
}

export function OverviewView({ connId, active, go }: SecViewProps) {
  const t = useT();
  const ov = useRemote(() => sudo(connId, (pw) => SecurityService.Overview(connId, pw)), [connId], { enabled: active });
  const d = ov.data;

  const checks = useMemo(() => [...(d?.checks ?? [])].sort((a, b) => (ORDER[a.status] ?? 5) - (ORDER[b.status] ?? 5)), [d]);
  const counts = useMemo(() => {
    const c = { ok: 0, warn: 0, crit: 0 };
    for (const x of d?.checks ?? []) if (x.status in c) c[x.status as keyof typeof c]++;
    return c;
  }, [d]);

  if (ov.error && !d) return <ErrorBox error={ov.error} onRetry={ov.reload} />;
  if (!d) return <Loading label={t("sec.ov.running")} />;

  const grade = d.score >= 85 ? "ok" : d.score >= 60 ? "warn" : "crit";

  return (
    <div className="sec-ov">
      <div className="sec-ov-head">
        <div className={`sec-score ${grade}`}>
          <div className="sec-score-num">{d.score}</div>
          <div className="sec-score-lbl">{t("sec.ov.score")}</div>
        </div>
        <div className="sec-ov-meta">
          <div className="sec-ov-title">{t(grade === "ok" ? "sec.ov.gradeOk" : grade === "warn" ? "sec.ov.gradeWarn" : "sec.ov.gradeCrit")}</div>
          <div className="muted">
            {d.distro || "Linux"} · {t("sec.ov.firewallBackend", { name: d.firewall === "none" ? t("sec.fw.none") : d.firewall })} · {t("sec.ov.checkedAt", { time: fmtTime(d.time) })}
          </div>
          <div className="sec-ov-counts">
            <StateBadge tone="err">{t("sec.ov.nCrit", { n: counts.crit })}</StateBadge>
            <StateBadge tone="warn">{t("sec.ov.nWarn", { n: counts.warn })}</StateBadge>
            <StateBadge tone="ok">{t("sec.ov.nOk", { n: counts.ok })}</StateBadge>
          </div>
          {d.limited && <div className="hint-box sec-mt">{t("sec.ov.limited")}</div>}
        </div>
        <div className="grow" />
        <button className="btn sm" onClick={ov.reload} disabled={ov.loading}>
          <RefreshCw size={13} className={ov.loading ? "spin-icon" : ""} /> {t("sec.ov.rerun")}
        </button>
      </div>

      <div className="sec-checks">
        {checks.map((c) => (
          <CheckRow key={c.id} c={c} go={go} />
        ))}
      </div>

      {d.logins && d.logins.length > 0 && (
        <Section title={t("sec.ov.lastLogins")}>
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>{t("sec.col.user")}</th>
                  <th>{t("sec.col.tty")}</th>
                  <th>{t("sec.col.from")}</th>
                  <th>{t("sec.col.time")}</th>
                  <th>{t("sec.col.session")}</th>
                </tr>
              </thead>
              <tbody>
                {d.logins.map((l, i) => (
                  <tr key={i}>
                    <td className="mono">{l.user}</td>
                    <td className="mono muted">{l.tty}</td>
                    <td className="mono">{l.host}</td>
                    <td>{fmtTime(l.time)}</td>
                    <td className="muted">{l.detail === "still logged in" ? t("sec.ev.stillIn") : l.detail}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Section>
      )}
    </div>
  );
}

function CheckRow({ c, go }: { c: Check; go: SecViewProps["go"] }) {
  const t = useT();
  const params = (c.params ?? {}) as Record<string, string>;
  const msg = tk(`sec.chk.${c.id}.${c.code}`, params, c.code);
  const fix = c.status === "warn" || c.status === "crit" ? tk(`sec.fix.${c.id}`, params, "") : "";
  const items = c.items ?? [];
  return (
    <div className={`sec-check ${c.status}`}>
      <StatusIcon s={c.status} />
      <div className="sec-check-body">
        <div className="sec-check-title">
          <span>{tk(`sec.chk.${c.id}`)}</span>
          <StateBadge tone={statusTone(c.status)}>{tk(`sec.status.${c.status}`)}</StateBadge>
        </div>
        <div className="sec-check-msg">
          {msg}
          {c.id === "updates" && params.age ? <span className="muted"> · {t("sec.chk.updates.age", { days: params.age })}</span> : null}
        </div>
        {items.length > 0 && (
          <div className="chips sec-mt">
            {items.slice(0, 12).map((it) => (
              <span key={it} className="chip mono">
                {it}
              </span>
            ))}
            {items.length > 12 && <span className="muted">+{items.length - 12}</span>}
          </div>
        )}
        {fix && <div className="sec-check-fix">{fix}</div>}
      </div>
      {c.tab && (
        <button className="btn ghost sm" onClick={() => go(c.tab as SecTab)}>
          {t("sec.ov.open")} <ChevronRight size={13} />
        </button>
      )}
    </div>
  );
}
