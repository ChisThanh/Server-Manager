import { useMemo, useState } from "react";
import { ArrowDownToLine, Disc, HardDrive, Network as NetIcon, RefreshCw, Trash2, Eraser } from "lucide-react";
import { useRemote } from "../../ui/hooks";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { runJob } from "../../ui/jobs";
import { confirmDanger } from "../../ui/confirm";
import { errMsg } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { promptDialog, toast } from "../../store/ui";
import { useT } from "../../i18n";
import { DockerService, IMAGE_RE, POLL_MS, ago, cmp, confirmOpts, ds, fullDate, shortId, useFilter, useSort, type Image, type ViewProps, type Volume } from "./common";

function Toolbar(props: { q: string; setQ: (v: string) => void; placeholder: string; count: string; loading: boolean; error: string; onReload: () => void; children?: React.ReactNode }) {
  const t = useT();
  return (
    <div className="toolbar">
      <input className="input" placeholder={props.placeholder} value={props.q} onChange={(e) => props.setQ(e.target.value)} />
      {props.children}
      <div className="grow" />
      {props.error && (
        <span className="badge err" title={props.error}>
          {t("docker.refreshFailed")}
        </span>
      )}
      <span className="muted">{props.count}</span>
      <button className="icon-btn" onClick={props.onReload} title={t("common.refresh")} disabled={props.loading}>
        <RefreshCw size={14} className={props.loading ? "spin-icon" : ""} />
      </button>
    </div>
  );
}

function UsedBy({ names }: { names: string[] | null }) {
  const t = useT();
  const list = names ?? [];
  if (list.length === 0) return <span className="muted">{t("docker.unused")}</span>;
  return (
    <span className="chips docker-usedby" title={list.join(", ")}>
      {list.slice(0, 3).map((n) => (
        <span className="chip" key={n}>
          {n}
        </span>
      ))}
      {list.length > 3 && <span className="chip">+{list.length - 3}</span>}
    </span>
  );
}

// ---------------------------------------------------------------- images

type ImgSort = "repository" | "size" | "created" | "used";

export function ImagesView(p: ViewProps) {
  const t = useT();
  const list = useRemote(() => ds(p.connId, (pw) => DockerService.Images(p.connId, pw)), [p.connId], { enabled: p.active, poll: p.auto ? POLL_MS : undefined });
  const [q, setQ] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const { sort, th } = useSort<ImgSort>("repository");
  const filtered = useFilter(list.data ?? undefined, q, (r) => [r.repository, r.tag, r.id, ...(r.usedBy ?? [])]);
  const rows = useMemo(() => {
    const key = (r: Image) => (sort.key === "used" ? (r.usedBy ?? []).length : sort.key === "repository" ? `${r.repository}:${r.tag}` : r[sort.key]);
    return [...filtered].sort((a, b) => cmp(key(a), key(b)) * (sort.desc ? -1 : 1));
  }, [filtered, sort]);
  const total = useMemo(() => (list.data ?? []).reduce((s, i) => s + i.size, 0), [list.data]);
  const dangling = useMemo(() => (list.data ?? []).filter((i) => i.dangling).length, [list.data]);

  const pull = async () => {
    const ref = await promptDialog(t("docker.img.pullTitle"), t("docker.img.ref"), "", {
      placeholder: "nginx:alpine",
      message: t("docker.img.pullMsg"),
      confirmText: t("docker.img.pull"),
      validate: (v) => (IMAGE_RE.test(v.trim()) ? null : t("docker.img.refInvalid")),
    });
    if (!ref) return;
    await runJob(t("docker.img.pullJob", { ref: ref.trim() }), () => ds(p.connId, (pw) => DockerService.PullImage(p.connId, ref.trim(), pw)));
    list.reload();
  };

  const prune = async () => {
    const r = await confirmOpts({
      serverId: p.connId,
      title: t("docker.img.pruneQ"),
      message: t("docker.img.pruneMsg", { n: dangling }),
      confirmText: t("docker.img.prune"),
      options: [{ name: "all", label: t("docker.img.pruneAll") }],
    });
    if (!r) return;
    setBusy("__prune");
    try {
      const res = await ds(p.connId, (pw) => DockerService.PruneImages(p.connId, r.all, pw));
      toast(t("docker.reclaimed", { size: formatBytes(res.reclaimed) }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
      list.reload();
    }
  };

  const remove = async (im: Image) => {
    const ref = im.dangling ? im.id : `${im.repository}:${im.tag}`;
    const used = im.usedBy ?? [];
    const r = await confirmOpts({
      serverId: p.connId,
      title: t("docker.img.removeQ", { ref: im.dangling ? shortId(im.id) : ref }),
      message: used.length ? t("docker.img.inUseMsg", { list: used.join(", ") }) : t("docker.img.removeMsg"),
      confirmText: t("common.delete"),
      options: used.length ? [{ name: "force", label: t("docker.img.optForce") }] : [],
    });
    if (!r) return;
    if (used.length && !r.force) {
      toast(t("err.docker.imageInUse", { name: ref, containers: used.join(", ") }), "error");
      return;
    }
    setBusy(im.id + ref);
    try {
      await ds(p.connId, (pw) => DockerService.RemoveImage(p.connId, ref, !!r.force, pw));
      toast(t("docker.img.removed", { ref }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
      list.reload();
    }
  };

  if (list.error && !list.data) return <ErrorBox error={list.error} onRetry={list.reload} />;
  if (!list.data) return <Loading label={t("common.loading")} />;
  return (
    <>
      <Toolbar
        q={q}
        setQ={setQ}
        placeholder={t("docker.img.filter")}
        count={t("docker.img.count", { n: rows.length, size: formatBytes(total) })}
        loading={list.loading}
        error={list.error}
        onReload={list.reload}
      >
        {p.canEdit && (
          <>
            <button className="btn sm" onClick={pull}>
              <ArrowDownToLine size={13} /> {t("docker.img.pull")}
            </button>
            <button className="btn sm" onClick={prune} disabled={busy === "__prune"}>
              {busy === "__prune" ? <span className="spinner" /> : <Eraser size={13} />} {t("docker.img.prune")}
              {dangling > 0 && <span className="docker-count">{dangling}</span>}
            </button>
          </>
        )}
      </Toolbar>
      {(list.data ?? []).length === 0 ? (
        <Empty icon={<Disc size={28} />} title={t("docker.img.none")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                {th("repository", t("docker.img.repo"))}
                <th>{t("docker.img.tag")}</th>
                <th>ID</th>
                {th("size", t("docker.col.size"), { num: true, defDesc: true })}
                {th("created", t("docker.col.created"), { defDesc: true })}
                {th("used", t("docker.usedBy"), { defDesc: true })}
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((im) => (
                <tr key={`${im.id}|${im.repository}|${im.tag}`}>
                  <td className="mono">{im.dangling ? <span className="muted">&lt;none&gt;</span> : im.repository}</td>
                  <td className="mono">{im.dangling ? <span className="badge warn">{t("docker.img.dangling")}</span> : im.tag}</td>
                  <td className="mono muted" title={im.id + (im.digest ? `\n${im.digest}` : "")}>
                    {shortId(im.id)}
                  </td>
                  <td className="num">{formatBytes(im.size)}</td>
                  <td className="muted" title={fullDate(im.created)}>
                    {ago(im.created)}
                  </td>
                  <td>
                    <UsedBy names={im.usedBy} />
                  </td>
                  <td className="docker-actions">
                    {p.canEdit &&
                      (busy === im.id + (im.dangling ? im.id : `${im.repository}:${im.tag}`) ? (
                        <span className="spinner" />
                      ) : (
                        <button className="icon-btn" title={t("common.delete")} onClick={() => remove(im)}>
                          <Trash2 size={13} />
                        </button>
                      ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {rows.length === 0 && <div className="docker-nomatch muted">{t("docker.noMatch")}</div>}
        </div>
      )}
    </>
  );
}

// ---------------------------------------------------------------- volumes

type VolSort = "name" | "driver" | "size" | "created" | "used";

export function VolumesView(p: ViewProps) {
  const t = useT();
  const list = useRemote(() => ds(p.connId, (pw) => DockerService.Volumes(p.connId, pw)), [p.connId], { enabled: p.active, poll: p.auto ? POLL_MS : undefined });
  const sizes = useRemote(() => ds(p.connId, (pw) => DockerService.VolumeSizes(p.connId, pw)), [p.connId], { enabled: p.active });
  const [q, setQ] = useState("");
  const [only, setOnly] = useState<"all" | "used" | "unused">("all");
  const [busy, setBusy] = useState<string | null>(null);
  const { sort, th } = useSort<VolSort>("name");
  const withSize = useMemo(
    () => (list.data ?? []).map((v) => ({ ...v, size: sizes.data?.[v.name] ?? v.size })),
    [list.data, sizes.data],
  );
  const filtered = useFilter(withSize, q, (r) => [r.name, r.project, r.mountpoint, ...(r.usedBy ?? [])]);
  const rows = useMemo(() => {
    const f = filtered.filter((v) => (only === "used" ? (v.usedBy ?? []).length > 0 : only === "unused" ? (v.usedBy ?? []).length === 0 : true));
    const key = (v: Volume) => (sort.key === "used" ? (v.usedBy ?? []).length : v[sort.key]);
    return [...f].sort((a, b) => cmp(key(a), key(b)) * (sort.desc ? -1 : 1));
  }, [filtered, only, sort]);
  const unused = withSize.filter((v) => (v.usedBy ?? []).length === 0).length;

  const remove = async (v: Volume) => {
    const ok = await confirmDanger({
      serverId: p.connId,
      title: t("docker.vol.removeQ", { name: v.name }),
      message: t("docker.vol.removeMsg"),
      confirmText: t("common.delete"),
    });
    if (!ok) return;
    setBusy(v.name);
    try {
      await ds(p.connId, (pw) => DockerService.RemoveVolume(p.connId, v.name, pw));
      toast(t("docker.vol.removed", { name: v.name }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
      list.reload();
    }
  };

  const prune = async () => {
    const r = await confirmOpts({
      serverId: p.connId,
      title: t("docker.vol.pruneQ"),
      message: t("docker.vol.pruneMsg"),
      confirmText: t("docker.vol.prune"),
      options: [{ name: "all", label: t("docker.vol.pruneAll") }],
    });
    if (!r) return;
    setBusy("__prune");
    try {
      const res = await ds(p.connId, (pw) => DockerService.PruneVolumes(p.connId, r.all, pw));
      toast(t("docker.reclaimed", { size: formatBytes(res.reclaimed) }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
      list.reload();
      sizes.reload();
    }
  };

  if (list.error && !list.data) return <ErrorBox error={list.error} onRetry={list.reload} />;
  if (!list.data) return <Loading label={t("common.loading")} />;
  return (
    <>
      <Toolbar
        q={q}
        setQ={setQ}
        placeholder={t("docker.vol.filter")}
        count={t("docker.vol.count", { n: rows.length })}
        loading={list.loading}
        error={list.error}
        onReload={() => {
          list.reload();
          sizes.reload();
        }}
      >
        <div className="segmented docker-seg">
          {(["all", "used", "unused"] as const).map((k) => (
            <button key={k} className={only === k ? "on" : ""} onClick={() => setOnly(k)}>
              {t(`docker.vol.f.${k}`)}
            </button>
          ))}
        </div>
        {p.canEdit && (
          <button className="btn sm" onClick={prune} disabled={busy === "__prune"}>
            {busy === "__prune" ? <span className="spinner" /> : <Eraser size={13} />} {t("docker.vol.prune")}
            {unused > 0 && <span className="docker-count">{unused}</span>}
          </button>
        )}
      </Toolbar>
      {(list.data ?? []).length === 0 ? (
        <Empty icon={<HardDrive size={28} />} title={t("docker.vol.none")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                {th("name", t("docker.col.name"))}
                {th("driver", t("docker.vol.driver"))}
                <th>{t("docker.vol.mountpoint")}</th>
                {th("size", t("docker.col.size"), { num: true, defDesc: true })}
                {th("used", t("docker.usedBy"), { defDesc: true })}
                {th("created", t("docker.col.created"), { defDesc: true })}
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((v) => {
                const used = (v.usedBy ?? []).length > 0;
                return (
                  <tr key={v.name}>
                    <td className="docker-name-cell">
                      <div className="mono docker-name" title={v.name}>
                        {v.anonymous ? shortId(v.name) : v.name}
                      </div>
                      <div className="docker-sub">
                        {v.anonymous && <span className="muted">{t("docker.vol.anonymous")}</span>}
                        {v.project && <span className="chip">{v.project}</span>}
                      </div>
                    </td>
                    <td>{v.driver}</td>
                    <td className="mono muted docker-path" title={v.mountpoint}>
                      {v.mountpoint}
                    </td>
                    <td className="num">{v.size >= 0 ? formatBytes(v.size) : sizes.loading ? <span className="spinner" /> : <span className="muted">—</span>}</td>
                    <td>
                      <UsedBy names={v.usedBy} />
                    </td>
                    <td className="muted" title={fullDate(v.created)}>
                      {ago(v.created)}
                    </td>
                    <td className="docker-actions">
                      {p.canEdit &&
                        (busy === v.name ? (
                          <span className="spinner" />
                        ) : (
                          <button className="icon-btn" title={used ? t("docker.vol.inUse") : t("common.delete")} disabled={used} onClick={() => remove(v)}>
                            <Trash2 size={13} />
                          </button>
                        ))}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {rows.length === 0 && <div className="docker-nomatch muted">{t("docker.noMatch")}</div>}
        </div>
      )}
    </>
  );
}

// ---------------------------------------------------------------- networks

export function NetworksView(p: ViewProps) {
  const t = useT();
  const list = useRemote(() => ds(p.connId, (pw) => DockerService.Networks(p.connId, pw)), [p.connId], { enabled: p.active, poll: p.auto ? POLL_MS : undefined });
  const [q, setQ] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const rows = useFilter(list.data ?? undefined, q, (r) => [r.name, r.driver, r.project, ...(r.subnets ?? []), ...(r.containers ?? []).map((c) => c.name)]);

  const remove = async (name: string) => {
    const ok = await confirmDanger({ serverId: p.connId, title: t("docker.net.removeQ", { name }), message: t("docker.net.removeMsg"), confirmText: t("common.delete") });
    if (!ok) return;
    setBusy(name);
    try {
      await ds(p.connId, (pw) => DockerService.RemoveNetwork(p.connId, name, pw));
      toast(t("docker.net.removed", { name }), "success");
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(null);
      list.reload();
    }
  };

  if (list.error && !list.data) return <ErrorBox error={list.error} onRetry={list.reload} />;
  if (!list.data) return <Loading label={t("common.loading")} />;
  return (
    <>
      <Toolbar q={q} setQ={setQ} placeholder={t("docker.net.filter")} count={t("docker.net.count", { n: rows.length })} loading={list.loading} error={list.error} onReload={list.reload} />
      {(list.data ?? []).length === 0 ? (
        <Empty icon={<NetIcon size={28} />} title={t("docker.net.none")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                <th>{t("docker.col.name")}</th>
                <th>{t("docker.net.driver")}</th>
                <th>{t("docker.net.scope")}</th>
                <th>{t("docker.net.subnet")}</th>
                <th>Gateway</th>
                <th>{t("docker.net.containers")}</th>
                <th>{t("docker.col.created")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((n) => {
                const cs = n.containers ?? [];
                return (
                  <tr key={n.id}>
                    <td className="docker-name-cell">
                      <div className="mono docker-name">{n.name}</div>
                      <div className="docker-sub">
                        {n.builtin && <span className="muted">{t("docker.net.builtin")}</span>}
                        {n.internal && <span className="badge">internal</span>}
                        {n.project && <span className="chip">{n.project}</span>}
                      </div>
                    </td>
                    <td>{n.driver}</td>
                    <td className="muted">{n.scope}</td>
                    <td className="mono">{(n.subnets ?? []).join(", ") || "—"}</td>
                    <td className="mono muted">{(n.gateways ?? []).join(", ") || "—"}</td>
                    <td>
                      {cs.length === 0 ? (
                        <span className="muted">—</span>
                      ) : (
                        <span className="chips docker-usedby" title={cs.map((c) => `${c.name} ${c.ipv4}`).join("\n")}>
                          {cs.slice(0, 3).map((c) => (
                            <span className="chip" key={c.name}>
                              {c.name}
                              {c.ipv4 && <span className="muted"> {c.ipv4.replace(/\/\d+$/, "")}</span>}
                            </span>
                          ))}
                          {cs.length > 3 && <span className="chip">+{cs.length - 3}</span>}
                        </span>
                      )}
                    </td>
                    <td className="muted" title={fullDate(n.created)}>
                      {ago(n.created)}
                    </td>
                    <td className="docker-actions">
                      {p.canEdit &&
                        !n.builtin &&
                        (busy === n.name ? (
                          <span className="spinner" />
                        ) : (
                          <button className="icon-btn" title={cs.length ? t("docker.net.inUse") : t("common.delete")} disabled={cs.length > 0} onClick={() => remove(n.name)}>
                            <Trash2 size={13} />
                          </button>
                        ))}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {rows.length === 0 && <div className="docker-nomatch muted">{t("docker.noMatch")}</div>}
        </div>
      )}
    </>
  );
}
