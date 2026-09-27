import { useMemo, useState } from "react";
import { Eye, EyeOff, Info } from "lucide-react";
import { useRemote } from "../../ui/hooks";
import { Empty, ErrorBox, KV, Loading } from "../../ui/Page";
import { SubTabs } from "../../ui/Tabs";
import { CodeEditor } from "../../ui/CodeEditor";
import { formatBytes } from "../../lib/format";
import { useT } from "../../i18n";
import { BigModal, DockerService, HealthBadge, StateCell, ds, fullDate, shortId, type ContainerDetail } from "./common";

type Tab = "overview" | "env" | "mounts" | "network" | "labels" | "health" | "raw";

const MASK = "••••••••";
const SECRET_RE = /pass|secret|token|key/i;

/** Replaces secret-looking env values in the raw inspect JSON. */
function maskRaw(raw: string): string {
  try {
    const arr = JSON.parse(raw);
    const env: unknown = arr?.Config?.Env;
    if (Array.isArray(env)) {
      arr.Config.Env = env.map((e) => {
        if (typeof e !== "string") return e;
        const i = e.indexOf("=");
        if (i < 0) return e;
        return SECRET_RE.test(e.slice(0, i)) ? `${e.slice(0, i)}=${MASK}` : e;
      });
    }
    return JSON.stringify(arr, null, 2);
  } catch {
    return raw;
  }
}

export function InspectModal({ connId, name, onClose }: { connId: string; name: string; onClose: () => void }) {
  const t = useT();
  const [tab, setTab] = useState<Tab>("overview");
  const [reveal, setReveal] = useState<Record<string, boolean>>({});
  const [revealAll, setRevealAll] = useState(false);
  const d = useRemote(() => ds(connId, (pw) => DockerService.ContainerInspect(connId, name, pw)), [connId, name]);
  const c = d.data;
  const raw = useMemo(() => (c ? (revealAll ? c.raw : maskRaw(c.raw)) : ""), [c, revealAll]);

  const env = c?.env ?? [];
  const mounts = c?.mounts ?? [];
  const nets = c?.networks ?? [];
  const ports = c?.ports ?? [];
  const labels = c?.labels ?? [];
  const hlog = c?.healthLog ?? [];

  return (
    <BigModal
      title={
        <>
          <Info size={16} /> {name}
          {c && <StateCell state={c.state} health={c.health} />}
        </>
      }
      onClose={onClose}
      className="docker-inspect"
    >
      {d.error ? (
        <ErrorBox error={d.error} onRetry={d.reload} />
      ) : !c ? (
        <Loading />
      ) : (
        <>
          <SubTabs<Tab>
            value={tab}
            onChange={setTab}
            items={[
              { id: "overview", label: t("docker.i.overview") },
              { id: "env", label: t("docker.i.env"), badge: env.length },
              { id: "mounts", label: t("docker.i.mounts"), badge: mounts.length },
              { id: "network", label: t("docker.i.network") },
              { id: "labels", label: t("docker.i.labels"), badge: labels.length },
              { id: "health", label: t("docker.i.health"), hidden: !c.health && hlog.length === 0 },
              { id: "raw", label: "JSON" },
            ]}
            right={
              (tab === "env" || tab === "raw") && env.some((e) => e.secret) ? (
                <button className="btn sm ghost" onClick={() => setRevealAll((v) => !v)}>
                  {revealAll ? <EyeOff size={13} /> : <Eye size={13} />} {revealAll ? t("docker.i.hideSecrets") : t("docker.i.showSecrets")}
                </button>
              ) : undefined
            }
          />
          <div className="docker-inspect-body">
            {tab === "overview" && <Overview c={c} />}
            {tab === "env" &&
              (env.length === 0 ? (
                <Empty title={t("docker.i.noEnv")} />
              ) : (
                <div className="table-wrap">
                  <table className="grid docker-kv-table">
                    <thead>
                      <tr>
                        <th>{t("docker.i.key")}</th>
                        <th>{t("docker.i.value")}</th>
                        <th />
                      </tr>
                    </thead>
                    <tbody>
                      {env.map((e, i) => {
                        const shown = !e.secret || revealAll || reveal[e.key];
                        return (
                          <tr key={i}>
                            <td className="mono">{e.key}</td>
                            <td className="mono docker-wrap">{shown ? e.value : MASK}</td>
                            <td style={{ width: 30 }}>
                              {e.secret && !revealAll && (
                                <button className="icon-btn" title={shown ? t("docker.i.hide") : t("docker.i.show")} onClick={() => setReveal((r) => ({ ...r, [e.key]: !r[e.key] }))}>
                                  {shown ? <EyeOff size={13} /> : <Eye size={13} />}
                                </button>
                              )}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              ))}
            {tab === "mounts" &&
              (mounts.length === 0 ? (
                <Empty title={t("docker.i.noMounts")} />
              ) : (
                <div className="table-wrap">
                  <table className="grid">
                    <thead>
                      <tr>
                        <th>{t("docker.i.type")}</th>
                        <th>{t("docker.i.source")}</th>
                        <th>{t("docker.i.dest")}</th>
                        <th>{t("docker.i.mode")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {mounts.map((m, i) => (
                        <tr key={i}>
                          <td>
                            <span className="chip">{m.type}</span>
                          </td>
                          <td className="mono docker-wrap">{m.type === "volume" ? m.name : m.source}</td>
                          <td className="mono docker-wrap">{m.destination}</td>
                          <td>{m.rw ? "rw" : "ro"}{m.mode ? ` (${m.mode})` : ""}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ))}
            {tab === "network" && (
              <>
                <div className="section-title">{t("docker.i.networks")}</div>
                {nets.length === 0 ? (
                  <div className="muted">{t("docker.i.netMode", { mode: c.networkMode })}</div>
                ) : (
                  <div className="table-wrap">
                    <table className="grid">
                      <thead>
                        <tr>
                          <th>{t("docker.col.name")}</th>
                          <th>IP</th>
                          <th>Gateway</th>
                          <th>MAC</th>
                          <th>{t("docker.i.aliases")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {nets.map((n) => (
                          <tr key={n.network}>
                            <td className="mono">{n.network}</td>
                            <td className="mono">{n.ip || "—"}{n.ipv6 ? ` · ${n.ipv6}` : ""}</td>
                            <td className="mono">{n.gateway || "—"}</td>
                            <td className="mono muted">{n.mac || "—"}</td>
                            <td className="muted">{(n.aliases ?? []).join(", ")}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
                <div className="section-title">{t("docker.col.ports")}</div>
                {ports.length === 0 ? (
                  <div className="muted">{t("docker.i.noPorts")}</div>
                ) : (
                  <div className="table-wrap">
                    <table className="grid">
                      <thead>
                        <tr>
                          <th>{t("docker.i.containerPort")}</th>
                          <th>{t("docker.i.hostPort")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {ports.map((pt, i) => (
                          <tr key={i}>
                            <td className="mono">{pt.container}</td>
                            <td className="mono">{pt.hostPort ? `${pt.hostIp || "0.0.0.0"}:${pt.hostPort}` : <span className="muted">{t("docker.i.exposedOnly")}</span>}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </>
            )}
            {tab === "labels" &&
              (labels.length === 0 ? (
                <Empty title={t("docker.i.noLabels")} />
              ) : (
                <div className="table-wrap">
                  <table className="grid docker-kv-table">
                    <tbody>
                      {labels.map((l) => (
                        <tr key={l.key}>
                          <td className="mono">{l.key}</td>
                          <td className="mono docker-wrap">{l.value}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ))}
            {tab === "health" && (
              <>
                <KV
                  items={[
                    [t("docker.i.health"), c.health ? <HealthBadge health={c.health} /> : "—"],
                    [t("docker.i.failingStreak"), c.failingStreak],
                    [t("docker.i.healthCmd"), <span className="mono">{(c.healthCheck ?? []).join(" ") || "—"}</span>],
                  ]}
                />
                <div className="section-title">{t("docker.i.healthLog")}</div>
                {hlog.length === 0 ? (
                  <div className="muted">{t("docker.i.noHealthLog")}</div>
                ) : (
                  <div className="list-rows">
                    {[...hlog].reverse().map((h, i) => (
                      <div className="list-row docker-health-row" key={i}>
                        <span className={`badge ${h.exitCode === 0 ? "ok" : "err"}`}>exit {h.exitCode}</span>
                        <span className="muted">{fullDate(h.start)}</span>
                        <pre className="grow mono docker-pre">{h.output || "—"}</pre>
                      </div>
                    ))}
                  </div>
                )}
              </>
            )}
            {tab === "raw" && <CodeEditor value={raw} language="json" readOnly height="calc(100vh - 260px)" />}
          </div>
        </>
      )}
    </BigModal>
  );
}

function Overview({ c }: { c: ContainerDetail }) {
  const t = useT();
  const cmd = [...(c.entrypoint ?? []), ...(c.cmd ?? [])];
  return (
    <div className="dash-cols">
      <div className="panel-box">
        <div className="box-title">{t("docker.i.container")}</div>
        <KV
          items={[
            ["ID", <span className="mono" title={c.id}>{shortId(c.id)}</span>],
            [t("docker.col.image"), <span className="mono">{c.image}</span>],
            [t("docker.i.imageId"), <span className="mono" title={c.imageId}>{shortId(c.imageId)}</span>],
            [t("docker.col.created"), fullDate(c.created)],
            [t("docker.i.started"), fullDate(c.startedAt) || "—"],
            [t("docker.i.finished"), c.state !== "running" && c.finishedAt ? fullDate(c.finishedAt) : "—"],
            [t("docker.i.exitCode"), c.state === "running" ? "—" : `${c.exitCode}${c.oomKilled ? " (OOM)" : ""}`],
            ...(c.error ? ([[t("docker.i.error"), <span className="err">{c.error}</span>]] as [string, JSX.Element][]) : []),
            ["PID", c.pid || "—"],
            ...(c.project ? ([[t("docker.i.compose"), `${c.project} / ${c.service}`]] as [string, string][]) : []),
          ]}
        />
      </div>
      <div className="panel-box">
        <div className="box-title">{t("docker.i.runtime")}</div>
        <KV
          items={[
            [t("docker.i.command"), <span className="mono docker-wrap">{cmd.join(" ") || "—"}</span>],
            ["Entrypoint", <span className="mono">{(c.entrypoint ?? []).join(" ") || "—"}</span>],
            [t("docker.i.workdir"), <span className="mono">{c.workingDir || "/"}</span>],
            [t("docker.i.user"), c.user || "root"],
            ["Hostname", <span className="mono">{c.hostname}</span>],
            [t("docker.i.restartPolicy"), `${c.restartPolicy} · ${t("docker.i.restarts", { n: c.restartCount })}`],
            [t("docker.i.netModeLabel"), c.networkMode],
            [t("docker.i.limits"), `${c.cpuLimit ? `${c.cpuLimit} CPU` : t("docker.i.noCpuLimit")} · ${c.memoryLimit ? formatBytes(c.memoryLimit) : t("docker.i.noMemLimit")}`],
            [t("docker.i.privileged"), c.privileged ? <span className="badge warn">{t("docker.yes")}</span> : t("docker.no")],
          ]}
        />
      </div>
    </div>
  );
}
