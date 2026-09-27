import type { ReactNode } from "react";
import {
  Activity,
  Clock,
  Cpu,
  Database,
  Gauge,
  HardDrive,
  Layers,
  Lock,
  RefreshCcw,
  Save,
  Users,
  Zap,
} from "lucide-react";
import type { MyOverview, Overview, PgOverview, RedisOverview } from "./common";
import { num, pct, shortDur } from "./common";
import { Bar, StateBadge } from "../../ui/Status";
import { KV, Section } from "../../ui/Page";
import { formatBytes, formatDate, formatDuration } from "../../lib/format";
import { useT } from "../../i18n";

function Card(props: {
  icon: ReactNode;
  k: ReactNode;
  v: ReactNode;
  s?: ReactNode;
  bar?: number;
  warn?: number;
  crit?: number;
}) {
  return (
    <div className="card">
      <div className="k">
        {props.icon}
        {props.k}
      </div>
      <div className="v">{props.v}</div>
      {props.s !== undefined && <div className="s">{props.s}</div>}
      {props.bar !== undefined && (
        <Bar pct={props.bar} warn={props.warn} crit={props.crit} />
      )}
    </div>
  );
}

/** Hit ratios are good when high: map to a "usage" bar that turns red when low. */
function missBar(ratio: number): number | undefined {
  return ratio < 0 ? undefined : ratio * 100;
}

export function OverviewView({
  ov,
  onGoSlow,
}: {
  ov: Overview;
  onGoSlow?: () => void;
}) {
  if (ov.postgres)
    return <PgView ov={ov} p={ov.postgres} onGoSlow={onGoSlow} />;
  if (ov.mysql) return <MyView ov={ov} m={ov.mysql} />;
  if (ov.redis) return <RedisView ov={ov} r={ov.redis} />;
  return null;
}

function PgView({
  ov,
  p,
  onGoSlow,
}: {
  ov: Overview;
  p: PgOverview;
  onGoSlow?: () => void;
}) {
  const t = useT();
  const connPct =
    p.maxConnections > 0 ? (p.connections / p.maxConnections) * 100 : 0;
  const dbs = (p.databases ?? []).slice(0, 8);
  return (
    <>
      <div className="cards">
        <Card
          icon={<Database size={13} />}
          k={t("db.ov.version")}
          v={
            <span className="db-ver">
              PostgreSQL {ov.version.split(" ")[0]}
            </span>
          }
          s={
            <>
              {t("db.ov.uptime")}: {formatDuration(ov.uptime)}
              {p.inRecovery && (
                <>
                  {" "}
                  · <StateBadge tone="warn">{t("db.ov.standby")}</StateBadge>
                </>
              )}
            </>
          }
        />
        <Card
          icon={<Users size={13} />}
          k={t("db.ov.connections")}
          v={`${p.connections} / ${p.maxConnections}`}
          bar={connPct}
          warn={70}
          crit={90}
          s={
            (p.byState ?? []).map((b) => `${b.name}: ${b.count}`).join(" · ") ||
            "–"
          }
        />
        <Card
          icon={<Gauge size={13} />}
          k={t("db.ov.cacheHit")}
          v={pct(p.cacheHitRatio, 2)}
          s={t("db.ov.cacheHitHint")}
          bar={missBar(p.cacheHitRatio)}
          warn={101}
          crit={101}
        />
        <Card
          icon={<HardDrive size={13} />}
          k={t("db.ov.size")}
          v={formatBytes(p.totalSize)}
          s={t("db.ov.dbCount", { n: (p.databases ?? []).length })}
        />
        <Card
          icon={<Clock size={13} />}
          k={t("db.ov.longest")}
          v={p.longestQuery > 0 ? shortDur(p.longestQuery) : "–"}
          s={
            <>
              {t("db.ov.idleInTx")}:{" "}
              <b className={p.idleInTx > 0 ? "db-warn" : ""}>{p.idleInTx}</b> ·{" "}
              {t("db.ov.lockWaits")}:{" "}
              <b className={p.locksWaiting > 0 ? "db-warn" : ""}>
                {p.locksWaiting}
              </b>
            </>
          }
        />
        <Card
          icon={<Activity size={13} />}
          k={t("db.ov.xacts")}
          v={num(p.xactCommit)}
          s={
            <>
              {t("db.ov.rollbacks")}: {num(p.xactRollback)} ·{" "}
              {t("db.ov.deadlocks")}:{" "}
              <b className={p.deadlocks > 0 ? "db-warn" : ""}>
                {num(p.deadlocks)}
              </b>
            </>
          }
        />
      </div>
      {!p.statStatements && (
        <div className="hint-box">
          {t("db.ov.pgssMissing")}{" "}
          {onGoSlow && (
            <button className="btn ghost sm" onClick={onGoSlow}>
              {t("db.ov.howEnable")}
            </button>
          )}
        </div>
      )}
      <div className="dash-cols">
        <div className="panel-box">
          <div className="box-title">
            <HardDrive size={13} /> {t("db.ov.largestDbs")}
          </div>
          <div className="list-rows">
            {dbs.map((d) => (
              <div className="list-row" key={d.name}>
                <span className="grow mono">{d.name}</span>
                <span className="muted">{d.owner}</span>
                <span className="mono db-w80 db-right">
                  {d.size >= 0 ? formatBytes(d.size) : "–"}
                </span>
              </div>
            ))}
          </div>
        </div>
        <div className="panel-box">
          <div className="box-title">
            <RefreshCcw size={13} /> {t("db.ov.replication")}
          </div>
          {p.inRecovery ? (
            <KV
              items={[
                [t("db.ov.role"), t("db.ov.standby")],
                [
                  t("db.ov.replayDelay"),
                  p.replayDelay >= 0 ? shortDur(p.replayDelay) : "–",
                ],
              ]}
            />
          ) : (p.replicas ?? []).length === 0 ? (
            <div className="muted">{t("db.ov.noReplicas")}</div>
          ) : (
            <div className="list-rows">
              {(p.replicas ?? []).map((r) => (
                <div className="list-row" key={r.pid}>
                  <span className="grow mono">{r.app || r.client}</span>
                  <StateBadge
                    state={r.state === "streaming" ? "running" : r.state}
                  >
                    {r.state}
                  </StateBadge>
                  <span className="muted">{r.syncState}</span>
                  <span className="mono">
                    {r.lagBytes >= 0 ? formatBytes(r.lagBytes) : "–"}
                  </span>
                  <span className="mono muted">{shortDur(r.replayLag)}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
      <Section title={t("db.ov.settings")} icon={<Cpu size={14} />}>
        <div className="panel-box">
          <KV
            items={(p.settings ?? []).map((s) => [
              <span className="mono">{s.name}</span>,
              <span className="mono">{s.value || "–"}</span>,
            ])}
          />
        </div>
      </Section>
    </>
  );
}

function MyView({ ov, m }: { ov: Overview; m: MyOverview }) {
  const t = useT();
  const connPct =
    m.maxConnections > 0 ? (m.threadsConnected / m.maxConnections) * 100 : 0;
  const flavor = m.flavor === "mariadb" ? "MariaDB" : "MySQL";
  return (
    <>
      <div className="cards">
        <Card
          icon={<Layers size={13} />}
          k={t("db.ov.version")}
          v={
            <span className="db-ver">
              {flavor} {ov.version.split("-")[0]}
            </span>
          }
          s={
            <>
              {t("db.ov.uptime")}: {formatDuration(ov.uptime)} ·{" "}
              {m.versionComment}
            </>
          }
        />
        <Card
          icon={<Users size={13} />}
          k={t("db.ov.threads")}
          v={`${m.threadsConnected} / ${m.maxConnections}`}
          bar={connPct}
          warn={70}
          crit={90}
          s={
            <>
              {t("db.ov.running")}: {m.threadsRunning} · {t("db.ov.maxUsed")}:{" "}
              {m.maxUsedConnections}
            </>
          }
        />
        <Card
          icon={<Gauge size={13} />}
          k={t("db.ov.bufferPool")}
          v={pct(m.bufferPoolHitRatio, 2)}
          bar={missBar(m.bufferPoolHitRatio)}
          warn={101}
          crit={101}
          s={
            <>
              {formatBytes(m.bufferPoolSize)} · {t("db.ov.used")}{" "}
              {pct(m.bufferPoolUsed, 0)}
            </>
          }
        />
        <Card
          icon={<Activity size={13} />}
          k={t("db.ov.questions")}
          v={num(m.questions)}
          s={
            <>
              {t("db.ov.qps")}:{" "}
              {ov.uptime > 0 ? (m.questions / ov.uptime).toFixed(1) : "–"} ·{" "}
              {t("db.ov.slowCount")}:{" "}
              <b className={m.slowQueries > 0 ? "db-warn" : ""}>
                {num(m.slowQueries)}
              </b>
            </>
          }
        />
        <Card
          icon={<HardDrive size={13} />}
          k={t("db.ov.databases")}
          v={num(m.databases)}
          s={
            <>
              {t("db.ov.abortedConnects")}: {num(m.abortedConnects)} ·{" "}
              {t("db.ov.totalConnections")}: {num(m.connections)}
            </>
          }
        />
        <Card
          icon={<Clock size={13} />}
          k={t("db.ov.slowLog")}
          v={
            m.slowLog.enabled ? (
              <StateBadge tone="ok">{t("db.on")}</StateBadge>
            ) : (
              <StateBadge tone="muted">{t("db.off")}</StateBadge>
            )
          }
          s={
            <>
              long_query_time = {m.slowLog.longQueryTime}s ·{" "}
              {m.slowLog.output || "–"}
            </>
          }
        />
      </div>
      <div className="dash-cols">
        <div className="panel-box">
          <div className="box-title">
            <RefreshCcw size={13} /> {t("db.ov.replication")}
          </div>
          {(m.replication ?? []).length === 0 ? (
            <div className="muted">{t("db.ov.notReplica")}</div>
          ) : (
            <KV
              items={(m.replication ?? []).map((r) => [
                <span className="mono">{r.name}</span>,
                <span className="mono">{r.value || "–"}</span>,
              ])}
            />
          )}
        </div>
        <div className="panel-box">
          <div className="box-title">
            <Cpu size={13} /> {t("db.ov.settings")}
          </div>
          <KV
            items={(m.settings ?? []).map((s) => [
              <span className="mono">{s.name}</span>,
              <span className="mono">{s.value || "–"}</span>,
            ])}
          />
        </div>
      </div>
    </>
  );
}

function RedisView({ ov, r }: { ov: Overview; r: RedisOverview }) {
  const t = useT();
  const memPct =
    r.maxMemory > 0 ? (r.usedMemory / r.maxMemory) * 100 : undefined;
  return (
    <>
      {r.configError && (
        <div className="hint-box">
          {t("db.rd.configDisabled", { err: r.configError })}
        </div>
      )}
      <div className="cards">
        <Card
          icon={<Zap size={13} />}
          k={t("db.ov.version")}
          v={<span className="db-ver">Redis {ov.version}</span>}
          s={
            <>
              {t("db.ov.uptime")}: {formatDuration(ov.uptime)} · {r.mode} ·{" "}
              {r.role}
            </>
          }
        />
        <Card
          icon={<Users size={13} />}
          k={t("db.rd.clients")}
          v={num(r.connectedClients)}
          s={
            <>
              {t("db.rd.blocked")}: {r.blockedClients} · max {num(r.maxClients)}
            </>
          }
          bar={
            r.maxClients > 0
              ? (r.connectedClients / r.maxClients) * 100
              : undefined
          }
        />
        <Card
          icon={<HardDrive size={13} />}
          k={t("db.rd.memory")}
          v={formatBytes(r.usedMemory)}
          bar={memPct}
          s={
            <>
              {t("db.rd.peak")} {formatBytes(r.usedMemoryPeak)} · max{" "}
              {r.maxMemory > 0
                ? formatBytes(r.maxMemory)
                : t("db.rd.unlimited")}{" "}
              · {r.maxMemoryPolicy || "–"}
            </>
          }
        />
        <Card
          icon={<Activity size={13} />}
          k={t("db.rd.ops")}
          v={`${r.opsPerSec.toFixed(0)}/s`}
          s={
            <>
              {t("db.rd.totalCmds")}: {num(r.totalCommands)}
            </>
          }
        />
        <Card
          icon={<Gauge size={13} />}
          k={t("db.rd.hitRatio")}
          v={pct(r.hitRatio)}
          bar={missBar(r.hitRatio)}
          warn={101}
          crit={101}
          s={
            <>
              {t("db.rd.hits")} {num(r.hits)} · {t("db.rd.misses")}{" "}
              {num(r.misses)}
            </>
          }
        />
        <Card
          icon={<Lock size={13} />}
          k={t("db.rd.keysOut")}
          v={num(r.evictedKeys)}
          s={
            <>
              {t("db.rd.evicted")} · {t("db.rd.expired")}: {num(r.expiredKeys)}
            </>
          }
        />
      </div>
      <div className="dash-cols">
        <div className="panel-box">
          <div className="box-title">
            <Save size={13} /> {t("db.rd.persistence")}
          </div>
          <KV
            items={[
              [
                "RDB",
                <>
                  {r.rdbLastSave > 0 ? formatDate(r.rdbLastSave) : "–"}{" "}
                  <StateBadge
                    state={r.rdbLastStatus === "ok" ? "ok" : "failed"}
                  >
                    {r.rdbLastStatus || "–"}
                  </StateBadge>
                  {r.rdbSaving && <> · {t("db.rd.saving")}</>}
                </>,
              ],
              [t("db.rd.changes"), num(r.rdbChanges)],
              [
                "AOF",
                r.aofEnabled ? (
                  <StateBadge
                    state={r.aofLastStatus === "ok" ? "ok" : "failed"}
                  >
                    {t("db.on")} · {r.aofLastStatus}
                  </StateBadge>
                ) : (
                  <StateBadge tone="muted">{t("db.off")}</StateBadge>
                ),
              ],
            ]}
          />
        </div>
        <div className="panel-box">
          <div className="box-title">
            <RefreshCcw size={13} /> {t("db.ov.replication")}
          </div>
          <KV
            items={[
              [t("db.ov.role"), r.role],
              ...(r.masterHost
                ? ([
                    [
                      t("db.rd.master"),
                      <>
                        {r.masterHost}{" "}
                        <StateBadge
                          state={r.masterLinkStatus === "up" ? "up" : "down"}
                        >
                          {r.masterLinkStatus}
                        </StateBadge>
                      </>,
                    ],
                  ] as [ReactNode, ReactNode][])
                : []),
              [
                t("db.rd.replicas"),
                (r.replicas ?? []).length === 0 ? (
                  "0"
                ) : (
                  <div className="mono">
                    {(r.replicas ?? []).map((x) => (
                      <div key={x}>{x}</div>
                    ))}
                  </div>
                ),
              ],
            ]}
          />
        </div>
      </div>
      <Section title={t("db.rd.keyspace")} icon={<Database size={14} />}>
        {(r.keyspace ?? []).length === 0 ? (
          <div className="muted">{t("db.rd.noKeys")}</div>
        ) : (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>DB</th>
                  <th className="num">{t("db.rd.keys")}</th>
                  <th className="num">{t("db.rd.expires")}</th>
                  <th className="num">{t("db.rd.avgTtl")}</th>
                </tr>
              </thead>
              <tbody>
                {(r.keyspace ?? []).map((k) => (
                  <tr key={k.db}>
                    <td className="mono">db{k.db}</td>
                    <td className="num">{num(k.keys)}</td>
                    <td className="num">{num(k.expires)}</td>
                    <td className="num">
                      {k.avgTtl > 0 ? shortDur(k.avgTtl / 1000) : "–"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
      <Section title={t("db.rd.info")} icon={<Cpu size={14} />}>
        {(r.sections ?? []).map((s) => (
          <details key={s.name} className="db-info-sec">
            <summary>{s.name || "–"}</summary>
            <KV
              items={(s.items ?? []).map((kv) => [
                <span className="mono">{kv.name}</span>,
                <span className="mono db-wrap">{kv.value}</span>,
              ])}
            />
          </details>
        ))}
      </Section>
    </>
  );
}
