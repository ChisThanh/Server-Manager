import { useMemo, useState } from "react";
import { Ban, RefreshCw, ShieldBan, ShieldX, Unlock } from "lucide-react";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { useT } from "../../i18n";
import { toast } from "../../store/ui";
import { SecurityService, act, confirmOpts, fmtTime, sudo, tk, type LastEntry, type SecViewProps } from "./common";

type Range = 24 | 168 | 720;
type View = "failed" | "logins" | "sudo" | "accounts" | "last" | "lastb" | "f2b";

export function EventsView({ connId, active, canSec, go }: SecViewProps) {
  const t = useT();
  const [hours, setHours] = useState<Range>(24);
  const [view, setView] = useState<View>("failed");
  const [hideApp, setHideApp] = useState(true);
  const [q, setQ] = useState("");
  const ev = useRemote(() => sudo(connId, (pw) => SecurityService.Events(connId, hours, pw)), [connId, hours], { enabled: active });
  const d = ev.data;
  const needle = q.trim().toLowerCase();
  const match = (...s: string[]) => !needle || s.some((x) => x.toLowerCase().includes(needle));

  const f2bJails = d?.fail2ban.jails ?? [];
  const banned = useMemo(() => new Set(f2bJails.flatMap((j) => j.banned ?? [])), [d]);
  const sudoRows = (d?.sudo ?? []).filter((e) => (!hideApp || !e.app) && match(e.user, e.command, e.runAs));
  const invalidUsers = (d?.failedByUser ?? []).filter((u) => u.invalid).length;
  const newLogins = (d?.logins ?? []).filter((l) => l.new).length;

  const ban = async (ip: string, jail: string, doBan: boolean) => {
    const ok = await confirmOpts({
      serverId: connId,
      title: doBan ? t("sec.f2b.banQ", { ip }) : t("sec.f2b.unbanQ", { ip }),
      message: t("sec.f2b.jailInfo", { jail }),
      confirmText: doBan ? t("sec.f2b.ban") : t("sec.f2b.unban"),
      danger: doBan,
    });
    if (!ok) return;
    if (await act(() => sudo(connId, (pw) => SecurityService.Fail2banSetBan(connId, jail, ip, doBan, pw)), doBan ? t("sec.f2b.banned", { ip }) : t("sec.f2b.unbanned", { ip }))) ev.reload();
  };

  // Permanent firewall block (ufw/firewalld/iptables, whichever the server uses).
  const blockFw = async (ip: string) => {
    const ok = await confirmOpts({ serverId: connId, title: t("sec.acc.blockQ", { n: 1 }), message: <span className="mono">{ip}</span>, confirmText: t("sec.acc.block"), danger: false });
    if (!ok) return;
    await act(async () => {
      const r = await sudo(connId, (pw) => SecurityService.AccessBlock(connId, { addrs: [ip], comment: "SSH brute force", temporary: false, ports: "" }, pw));
      const skip = r.skipped?.[0];
      if (r.applied?.length) toast(t("sec.acc.blocked", { n: 1 }), "success");
      else if (skip) toast(`${ip}: ${tk(`sec.acc.skip.${skip.reason}`, undefined, skip.reason)}`, "info");
    });
  };

  if (ev.error && !d) return <ErrorBox error={ev.error} onRetry={ev.reload} />;
  if (!d) return <Loading label={t("sec.ev.loading")} />;

  const f2bRunning = d.fail2ban.running;
  const sshJail = f2bJails.find((j) => j.name === "sshd")?.name ?? f2bJails[0]?.name;

  const views: [View, string, number | undefined][] = [
    ["failed", t("sec.ev.v.failed"), d.failedTotal],
    ["logins", t("sec.ev.v.logins"), d.logins?.length],
    ["sudo", t("sec.ev.v.sudo"), undefined],
    ["accounts", t("sec.ev.v.accounts"), d.accounts?.length],
    ["last", "last", undefined],
    ["lastb", "lastb", d.lastb?.length],
    ["f2b", "fail2ban", undefined],
  ];

  return (
    <div className="sec-events">
      <div className="toolbar">
        <div className="segmented sec-seg-sm">
          {([24, 168, 720] as const).map((h) => (
            <button key={h} className={hours === h ? "on" : ""} onClick={() => setHours(h)}>
              {t(`sec.ev.range.${h}`)}
            </button>
          ))}
        </div>
        <input className="input input-sm" placeholder={t("sec.ev.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="grow" />
        <span className="muted sec-small">
          {d.source === "none" ? t("sec.ev.noSource") : t("sec.ev.source", { src: d.sourcePath })}
          {d.truncated ? ` · ${t("sec.ev.truncated")}` : ""}
        </span>
        <button className="icon-btn" onClick={ev.reload} title={t("common.refresh")} disabled={ev.loading}>
          <RefreshCw size={14} className={ev.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {d.limited && <div className="hint-box">{t("sec.ev.limited")}</div>}

      <div className="cards">
        <div className={`card ${d.failedTotal > 50 ? "sec-card-warn" : ""}`}>
          <div className="k">{t("sec.ev.c.failed")}</div>
          <div className="v">{d.failedTotal}</div>
          <div className="s">{t("sec.ev.c.ips", { n: d.failedByIp?.length ?? 0 })}</div>
        </div>
        <div className="card">
          <div className="k">{t("sec.ev.c.invalid")}</div>
          <div className="v">{invalidUsers}</div>
          <div className="s">{t("sec.ev.c.invalidSub")}</div>
        </div>
        <div className={`card ${newLogins ? "sec-card-warn" : ""}`}>
          <div className="k">{t("sec.ev.c.logins")}</div>
          <div className="v">{d.logins?.length ?? 0}</div>
          <div className="s">{t("sec.ev.c.newIps", { n: newLogins })}</div>
        </div>
        <div className="card">
          <div className="k">fail2ban</div>
          <div className="v sec-card-text">{!d.fail2ban.installed ? t("sec.f2b.notInstalled") : f2bRunning ? t("sec.f2b.running") : t("sec.f2b.stopped")}</div>
          <div className="s">{f2bRunning ? t("sec.f2b.bannedNow", { n: banned.size }) : " "}</div>
        </div>
      </div>

      <div className="segmented sec-seg sec-mb">
        {views.map(([k, label, n]) => (
          <button key={k} className={view === k ? "on" : ""} onClick={() => setView(k)}>
            {label}
            {n ? <span className="sec-count">{n}</span> : null}
          </button>
        ))}
      </div>

      {view === "failed" && (
        <div className="sec-grid2">
          <div>
            <div className="sec-subtitle sec-subtitle-row">
              {t("sec.ev.byIp")}
              <div className="grow" />
              <button className="link-btn" onClick={() => go("access")}>
                <ShieldBan size={12} /> {t("sec.tab.access")}
              </button>
            </div>
            {(d.failedByIp ?? []).length === 0 ? (
              <Empty title={t("sec.ev.noFailed")} />
            ) : (
              <div className="table-wrap">
                <table className="grid">
                  <thead>
                    <tr>
                      <th>IP</th>
                      <th className="num">{t("sec.ev.count")}</th>
                      <th>{t("sec.ev.users")}</th>
                      <th>{t("sec.ev.lastSeen")}</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {(d.failedByIp ?? [])
                      .filter((a) => match(a.ip, ...(a.users ?? [])))
                      .map((a) => (
                        <tr key={a.ip} title={t("sec.ev.firstLast", { first: fmtTime(a.first), last: fmtTime(a.last) })}>
                          <td className="mono">
                            {a.ip} {banned.has(a.ip) && <StateBadge tone="err">{t("sec.f2b.bannedTag")}</StateBadge>}
                          </td>
                          <td className="num">{a.count}</td>
                          <td className="mono sec-small sec-wrap">
                            {(a.users ?? []).slice(0, 5).join(", ")}
                            {(a.users ?? []).length > 5 ? "…" : ""}
                            {a.invalid > 0 && <span className="muted"> · {t("sec.ev.nInvalid", { n: a.invalid })}</span>}
                          </td>
                          <td className="sec-small">{fmtTime(a.last)}</td>
                          <td style={{ whiteSpace: "nowrap" }}>
                            {canSec && f2bRunning && sshJail && !banned.has(a.ip) && (
                              <button className="icon-btn" title={t("sec.f2b.banIn", { jail: sshJail })} onClick={() => ban(a.ip, sshJail, true)}>
                                <Ban size={13} />
                              </button>
                            )}
                            {canSec && (
                              <button className="icon-btn" title={t("sec.ev.blockFw")} onClick={() => blockFw(a.ip)}>
                                <ShieldX size={13} />
                              </button>
                            )}
                          </td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
          <div>
            <div className="sec-subtitle">{t("sec.ev.byUser")}</div>
            {(d.failedByUser ?? []).length === 0 ? (
              <Empty title={t("sec.ev.noFailed")} />
            ) : (
              <div className="table-wrap">
                <table className="grid">
                  <thead>
                    <tr>
                      <th>{t("sec.col.user")}</th>
                      <th className="num">{t("sec.ev.count")}</th>
                      <th className="num">IP</th>
                      <th>{t("sec.ev.lastSeen")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(d.failedByUser ?? [])
                      .filter((u) => match(u.user))
                      .map((u) => (
                        <tr key={u.user}>
                          <td className="mono">
                            {u.user || <span className="muted">∅</span>} {u.invalid && <StateBadge tone="muted">{t("sec.ev.invalidUser")}</StateBadge>}
                          </td>
                          <td className="num">{u.count}</td>
                          <td className="num">{u.ips}</td>
                          <td className="sec-small">{fmtTime(u.last)}</td>
                        </tr>
                      ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      )}

      {view === "logins" &&
        ((d.logins ?? []).length === 0 ? (
          <Empty title={t("sec.ev.noLogins")} />
        ) : (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>{t("sec.col.time")}</th>
                  <th>{t("sec.col.user")}</th>
                  <th>IP</th>
                  <th>{t("sec.ev.method")}</th>
                </tr>
              </thead>
              <tbody>
                {(d.logins ?? [])
                  .filter((l) => match(l.user, l.ip))
                  .map((l, i) => (
                    <tr key={i} className={l.new ? "sec-row-new" : ""}>
                      <td className="sec-small">{fmtTime(l.time)}</td>
                      <td className="mono">{l.user}</td>
                      <td className="mono">
                        {l.ip} {l.new && <StateBadge tone="warn">{t("sec.ev.newIp")}</StateBadge>}
                      </td>
                      <td>{l.method}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        ))}

      {view === "sudo" && (
        <>
          <label className="check sec-mb">
            <input type="checkbox" checked={hideApp} onChange={(e) => setHideApp(e.target.checked)} /> {t("sec.ev.hideApp", { user: d.loginUser })}
          </label>
          {sudoRows.length === 0 ? (
            <Empty title={t("sec.ev.noSudo")} />
          ) : (
            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th>{t("sec.col.time")}</th>
                    <th>{t("sec.col.user")}</th>
                    <th>{t("sec.ev.runAs")}</th>
                    <th>{t("sec.ev.command")}</th>
                  </tr>
                </thead>
                <tbody>
                  {sudoRows.slice(0, 500).map((e, i) => (
                    <tr key={i}>
                      <td className="sec-small">{fmtTime(e.time)}</td>
                      <td className="mono">{e.user}</td>
                      <td className="mono">{e.runAs}</td>
                      <td className="cmd mono sec-small" title={e.cwd}>
                        {e.failed && <StateBadge tone="err">{e.reason}</StateBadge>} {e.command}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}

      {view === "accounts" &&
        ((d.accounts ?? []).length === 0 ? (
          <Empty title={t("sec.ev.noAccounts")} />
        ) : (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>{t("sec.col.time")}</th>
                  <th>{t("sec.ev.tool")}</th>
                  <th>{t("sec.ev.message")}</th>
                </tr>
              </thead>
              <tbody>
                {(d.accounts ?? [])
                  .filter((a) => match(a.tool, a.message))
                  .map((a, i) => (
                    <tr key={i}>
                      <td className="sec-small">{fmtTime(a.time)}</td>
                      <td>
                        <StateBadge tone={a.kind === "password" ? "warn" : a.kind === "group" ? "info" : "muted"}>{a.tool}</StateBadge>
                      </td>
                      <td className="mono sec-small sec-wrap">{a.message}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        ))}

      {view === "last" && <LastTable rows={(d.last ?? []).filter((l) => match(l.user, l.host))} empty={t("sec.ev.noLast")} />}
      {view === "lastb" && <LastTable rows={(d.lastb ?? []).filter((l) => match(l.user, l.host))} empty={t("sec.ev.noLastb")} />}

      {view === "f2b" &&
        (!d.fail2ban.installed ? (
          <Empty title={t("sec.f2b.notInstalled")} text={<pre className="sec-pre mono">{"sudo apt install fail2ban   # Debian/Ubuntu\nsudo dnf install fail2ban   # RHEL (EPEL)"}</pre>} />
        ) : !f2bRunning ? (
          <Empty title={t("sec.f2b.stopped")} text={<pre className="sec-pre mono">sudo systemctl enable --now fail2ban</pre>} />
        ) : f2bJails.length === 0 ? (
          <Empty title={t("sec.f2b.noJails")} />
        ) : (
          <div className="sec-jails">
            {f2bJails.map((j) => (
              <div key={j.name} className="panel-box sec-pad">
                <div className="sec-jail-head">
                  <b className="mono">{j.name}</b>
                  <span className="muted sec-small">{t("sec.f2b.stats", { cf: j.currentlyFailed, tf: j.totalFailed, cb: j.currentlyBanned, tb: j.totalBanned })}</span>
                </div>
                <div className="chips sec-mt">
                  {(j.banned ?? []).length === 0 && <span className="muted">{t("sec.f2b.noneBanned")}</span>}
                  {(j.banned ?? []).map((ip) => (
                    <span key={ip} className="chip mono">
                      {ip}
                      {canSec && (
                        <button className="icon-btn sec-chip-x" title={t("sec.f2b.unban")} onClick={() => ban(ip, j.name, false)}>
                          <Unlock size={11} />
                        </button>
                      )}
                    </span>
                  ))}
                </div>
              </div>
            ))}
          </div>
        ))}
    </div>
  );
}

function LastTable({ rows, empty }: { rows: LastEntry[]; empty: string }) {
  const t = useT();
  if (rows.length === 0) return <Empty title={empty} />;
  return (
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
          {rows.map((l, i) => (
            <tr key={i}>
              <td className="mono">{l.user}</td>
              <td className="mono muted">{l.tty}</td>
              <td className="mono">{l.host}</td>
              <td className="sec-small">{fmtTime(l.time)}</td>
              <td className="muted sec-small">{l.detail === "still logged in" ? t("sec.ev.stillIn") : l.detail}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
