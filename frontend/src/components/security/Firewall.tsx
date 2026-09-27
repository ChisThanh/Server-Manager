import { useEffect, useMemo, useState } from "react";
import { Flame, Plus, Power, PowerOff, RefreshCw, RotateCcw, Save, ShieldOff, Trash2, X } from "lucide-react";
import { Empty, ErrorBox, KV, Loading, Section } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { CodeEditor } from "../../ui/CodeEditor";
import { useRemote } from "../../ui/hooks";
import { Modal } from "../Overlays";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import {
  SecurityService,
  act,
  confirmOpts,
  errCode,
  errMsg,
  isCancelled,
  sudo,
  tk,
  validAddr,
  validPortSpec,
  type FirewallInfo,
  type IptChain,
  type IptRule,
  type RulePrefill,
  type SecViewProps,
  type UFWRule,
} from "./common";
import { Select } from "../../ui/Select";

type Backend = "ufw" | "firewalld" | "iptables" | "nftables";

interface FwProps {
  connId: string;
  active: boolean;
  canSec: boolean;
  info: FirewallInfo;
  prefill?: RulePrefill;
  clearPrefill: () => void;
  refreshDetect: () => void;
}

/** Runs a change; on a "would cut SSH" guard asks for a typed confirmation
 *  and retries with force. */
async function withForce(connId: string, sshPorts: number[], run: (force: boolean) => Promise<unknown>, okMsg: string): Promise<boolean> {
  try {
    await run(false);
    toast(okMsg, "success");
    return true;
  } catch (e) {
    if (isCancelled(e)) return false;
    const code = errCode(e);
    if (code !== "sec.fw.blocksSsh" && code !== "sec.fw.sshRule") {
      toast(errMsg(e), "error");
      return false;
    }
    const word = String(sshPorts[0] ?? 22);
    const ok = await confirmOpts({
      serverId: connId,
      title: tk("sec.fw.sshRiskTitle"),
      message: tk(code === "sec.fw.blocksSsh" ? "sec.fw.blocksSshMsg" : "sec.fw.sshRuleMsg", { port: word }),
      confirmText: tk("sec.fw.doAnyway"),
      typeWord: word,
    });
    if (!ok) return false;
    return act(() => run(true), okMsg);
  }
}

export function FirewallView({ connId, active, canSec, prefill, clearPrefill }: SecViewProps & { prefill?: RulePrefill; clearPrefill: () => void }) {
  const t = useT();
  const detect = useRemote(() => sudo(connId, (pw) => SecurityService.FirewallDetect(connId, pw)), [connId], { enabled: active });
  const [chosen, setChosen] = useState<Backend | null>(null);
  const info = detect.data;
  const installed = (info?.backends ?? []).filter((b) => b.installed);
  const backend: Backend | null = chosen ?? (info && info.primary !== "none" ? (info.primary as Backend) : null);

  useEffect(() => {
    // A prefilled rule goes to the primary backend.
    if (prefill && info && info.primary !== "none" && info.primary !== "nftables") setChosen(info.primary as Backend);
  }, [prefill?.seq, info]);

  if (detect.error && !info) return <ErrorBox error={detect.error} onRetry={detect.reload} />;
  if (!info) return <Loading label={t("sec.fw.detecting")} />;
  if (installed.length === 0 || !backend)
    return (
      <Empty
        icon={<ShieldOff size={30} />}
        title={t("sec.fw.noneInstalled")}
        text={
          <div>
            <p>{t("sec.fw.noneHint")}</p>
            <pre className="sec-pre mono">{"sudo apt install ufw        # Debian/Ubuntu\nsudo dnf install firewalld   # RHEL/Rocky/Alma/Fedora\nsudo apk add iptables        # Alpine"}</pre>
          </div>
        }
        action={
          <button className="btn" onClick={detect.reload}>
            {t("common.refresh")}
          </button>
        }
      />
    );

  const props: FwProps = { connId, active, canSec, info, prefill, clearPrefill, refreshDetect: detect.reload };
  return (
    <div className="sec-fw">
      <div className="toolbar">
        <div className="segmented sec-seg">
          {installed.map((b) => (
            <button key={b.name} className={backend === b.name ? "on" : ""} onClick={() => setChosen(b.name as Backend)}>
              {b.name}
              {b.active && <span className="sec-dot-on" />}
            </button>
          ))}
        </div>
        <span className="muted sec-small">
          {installed.map((b) => `${b.name} ${b.version}`.trim()).join(" · ")}
          {info.clientIp ? ` · ${t("sec.fw.yourIp", { ip: info.clientIp })}` : ""}
        </span>
        <div className="grow" />
        <button className="icon-btn" onClick={detect.reload} title={t("common.refresh")} disabled={detect.loading}>
          <RefreshCw size={14} className={detect.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {backend === "ufw" && <UFWView {...props} />}
      {backend === "firewalld" && <FirewalldView {...props} />}
      {backend === "iptables" && <IptablesView {...props} />}
      {backend === "nftables" && <NftView {...props} />}
    </div>
  );
}

// ================= UFW =================

function UFWView({ connId, active, canSec, info, prefill, clearPrefill, refreshDetect }: FwProps) {
  const t = useT();
  const st = useRemote(() => sudo(connId, (pw) => SecurityService.UFWStatus(connId, pw)), [connId], { enabled: active });
  const [adding, setAdding] = useState<RulePrefill | null>(null);
  const [busy, setBusy] = useState(false);
  const d = st.data;
  const sshPorts = d?.sshPorts?.length ? d.sshPorts : info.sshPorts ?? [22];

  useEffect(() => {
    if (prefill && canSec) {
      setAdding(prefill);
      clearPrefill();
    }
  }, [prefill?.seq]);

  const reload = () => {
    st.reload();
    refreshDetect();
  };
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    const r = await act(fn, ok);
    setBusy(false);
    if (r) reload();
    return r;
  };

  const enable = async () => {
    const ok = await confirmOpts({
      serverId: connId,
      title: t("sec.ufw.enableQ"),
      message: t("sec.ufw.enableMsg", { ports: sshPorts.map((p) => `${p}/tcp`).join(", ") }),
      confirmText: t("sec.ufw.enable"),
    });
    if (!ok) return;
    setBusy(true);
    try {
      const allowed = await sudo(connId, (pw) => SecurityService.UFWEnable(connId, pw));
      toast(t("sec.ufw.enabled", { ports: (allowed ?? []).join(", ") }), "success");
      reload();
    } catch (e) {
      if (!isCancelled(e)) toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const del = async (r: UFWRule) => {
    const ok = await confirmOpts({ serverId: connId, title: t("sec.fw.deleteRuleQ"), message: <span className="mono">{`[${r.num}] ${r.raw}`}</span>, confirmText: t("common.delete") });
    if (!ok) return;
    setBusy(true);
    const done = await withForce(connId, sshPorts, (force) => sudo(connId, (pw) => SecurityService.UFWDeleteRule(connId, r.num, r.raw, force, pw)), t("sec.fw.ruleDeleted"));
    setBusy(false);
    if (done) reload();
  };

  const setDefault = async (direction: string, policy: string) => {
    const ok = await confirmOpts({ serverId: connId, title: t("sec.ufw.defaultQ"), message: t("sec.ufw.defaultMsg", { direction: tk(`sec.ufw.dir.${direction}`), policy }), confirmText: t("common.confirm") });
    if (!ok) return;
    setBusy(true);
    try {
      const allowed = await sudo(connId, (pw) => SecurityService.UFWSetDefault(connId, direction, policy, pw));
      toast(allowed && allowed.length ? t("sec.ufw.defaultSetSsh", { ports: allowed.join(", ") }) : t("sec.fw.done"), "success");
      reload();
    } catch (e) {
      if (!isCancelled(e)) toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  if (st.error && !d) return <ErrorBox error={st.error} onRetry={st.reload} />;
  if (!d) return <Loading />;

  return (
    <>
      <div className="sec-fw-head">
        <StateBadge tone={d.active ? "ok" : "warn"}>{d.active ? t("sec.fw.active") : t("sec.fw.inactive")}</StateBadge>
        <span className="muted">{t("sec.ufw.logging", { v: d.logging || "—" })}</span>
        <div className="grow" />
        {canSec && (
          <div className="sec-btn-row">
            {d.active ? (
              <button className="btn sm danger" disabled={busy} onClick={async () => (await confirmOpts({ serverId: connId, title: t("sec.ufw.disableQ"), message: t("sec.ufw.disableMsg"), confirmText: t("sec.ufw.disable") })) && run(() => sudo(connId, (pw) => SecurityService.UFWDisable(connId, pw)), t("sec.ufw.disabled"))}>
                <PowerOff size={13} /> {t("sec.ufw.disable")}
              </button>
            ) : (
              <button className="btn sm primary" disabled={busy} onClick={enable}>
                <Power size={13} /> {t("sec.ufw.enable")}
              </button>
            )}
            <button className="btn sm" disabled={busy} onClick={() => run(() => sudo(connId, (pw) => SecurityService.UFWReload(connId, pw)), t("sec.ufw.reloaded"))}>
              <RotateCcw size={13} /> {t("sec.ufw.reload")}
            </button>
            <button className="btn sm ghost" disabled={busy} onClick={async () => (await confirmOpts({ serverId: connId, title: t("sec.ufw.resetQ"), message: t("sec.ufw.resetMsg"), confirmText: t("sec.ufw.reset"), typeWord: "reset" })) && run(() => sudo(connId, (pw) => SecurityService.UFWReset(connId, pw)), t("sec.ufw.resetDone"))}>
              <Trash2 size={13} /> {t("sec.ufw.reset")}
            </button>
            <button className="btn sm primary" disabled={busy} onClick={() => setAdding({ seq: Date.now() })}>
              <Plus size={13} /> {t("sec.fw.addRule")}
            </button>
          </div>
        )}
      </div>

      <div className="sec-policy-row">
        {(["incoming", "outgoing", "routed"] as const).map((dir) => (
          <label key={dir} className="sec-policy">
            <span className="muted">{t(`sec.ufw.dir.${dir}`)}</span>
            <Select
              size="sm"
              value={d.defaults[dir] || ""}
              disabled={!canSec || busy}
              onChange={(v) => setDefault(dir, v)}
              placeholder="—"
              options={(dir === "routed" && d.defaults.routed === "disabled" ? ["disabled", "allow", "deny", "reject"] : ["allow", "deny", "reject"]).map((p) => ({ value: p, disabled: p === "disabled" }))}
            />
          </label>
        ))}
        <label className="sec-policy">
          <span className="muted">{t("sec.ufw.logLevel")}</span>
          <Select
            size="sm"
            value={(d.logging || "off").split(" ")[0] === "on" ? (/\((\w+)\)/.exec(d.logging)?.[1] ?? "low") : "off"}
            disabled={!canSec || busy}
            onChange={(v) => run(() => sudo(connId, (pw) => SecurityService.UFWSetLogging(connId, v, pw)), t("sec.fw.done"))}
            options={["off", "low", "medium", "high", "full"]}
          />
        </label>
      </div>

      {!d.active && <div className="hint-box sec-warnbox">{t("sec.ufw.inactiveHint", { ports: sshPorts.join(", ") })}</div>}

      {d.active ? (
        (d.rules ?? []).length === 0 ? (
          <Empty title={t("sec.fw.noRules")} />
        ) : (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th className="num">#</th>
                  <th>{t("sec.fw.to")}</th>
                  <th>{t("sec.fw.action")}</th>
                  <th>{t("sec.fw.from")}</th>
                  <th>{t("sec.fw.comment")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(d.rules ?? []).map((r) => (
                  <tr key={r.num}>
                    <td className="num muted">{r.num}</td>
                    <td className="mono">{r.to}</td>
                    <td>
                      <StateBadge tone={r.action === "ALLOW" ? "ok" : r.action === "LIMIT" ? "info" : "err"}>
                        {r.action} {r.direction}
                      </StateBadge>
                    </td>
                    <td className="mono">{r.from}</td>
                    <td className="muted">{r.comment}</td>
                    <td style={{ width: 40 }}>
                      {canSec && (
                        <button className="icon-btn" title={t("common.delete")} disabled={busy} onClick={() => del(r)}>
                          <Trash2 size={13} />
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )
      ) : (
        <Section title={t("sec.ufw.added")}>
          {(d.added ?? []).length === 0 ? (
            <div className="muted">{t("sec.fw.noRules")}</div>
          ) : (
            <div className="list-rows">
              {(d.added ?? []).map((line) => (
                <div key={line} className="list-row">
                  <span className="mono sec-small grow">{line}</span>
                  {canSec && (
                    <button
                      className="icon-btn"
                      title={t("common.delete")}
                      disabled={busy}
                      onClick={async () =>
                        (await confirmOpts({ serverId: connId, title: t("sec.fw.deleteRuleQ"), message: <span className="mono">{line}</span>, confirmText: t("common.delete") })) &&
                        run(() => sudo(connId, (pw) => SecurityService.UFWDeleteAdded(connId, line, pw)), t("sec.fw.ruleDeleted"))
                      }
                    >
                      <Trash2 size={13} />
                    </button>
                  )}
                </div>
              ))}
            </div>
          )}
        </Section>
      )}

      {adding && (
        <UFWRuleModal
          prefill={adding}
          ruleCount={(d.rules ?? []).length}
          onClose={() => setAdding(null)}
          onSubmit={async (rule) => {
            const ok = await withForce(connId, sshPorts, (force) => sudo(connId, (pw) => SecurityService.UFWAddRule(connId, { ...rule, force }, pw)), t("sec.fw.ruleAdded"));
            if (ok) {
              setAdding(null);
              reload();
            }
          }}
        />
      )}
    </>
  );
}

function UFWRuleModal({ prefill, ruleCount, onClose, onSubmit }: { prefill: RulePrefill; ruleCount: number; onClose: () => void; onSubmit: (r: { action: string; direction: string; port: string; proto: string; from: string; to: string; interface: string; comment: string; position: number; force: boolean }) => Promise<void> }) {
  const t = useT();
  const [f, setF] = useState({
    action: (prefill.action ?? "allow") as string,
    direction: "in",
    port: prefill.port ?? "",
    proto: prefill.proto ?? "tcp",
    from: prefill.from ?? "",
    to: "",
    iface: "",
    comment: prefill.comment ?? "",
    // Deny rules only win when placed before the allow rules.
    position: prefill.action === "deny" && ruleCount > 0 ? "1" : "",
  });
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const pick = (k: keyof typeof f) => (v: string) => setF({ ...f, [k]: v });
  const multi = /[,:-]/.test(f.port);
  const errs = {
    port: !validPortSpec(f.port) ? t("sec.fw.badPort") : multi && f.proto === "any" ? t("sec.fw.rangeProto") : "",
    from: !validAddr(f.from) ? t("sec.fw.badAddr") : "",
    to: !validAddr(f.to) ? t("sec.fw.badAddr") : "",
    position: f.position && !/^\d+$/.test(f.position) ? t("sec.fw.badNumber") : "",
    broad: !f.port.trim() && !f.from.trim() && !f.to.trim() && (f.action === "allow" || f.action === "limit") ? t("sec.fw.tooBroad") : "",
  };
  const ok = Object.values(errs).every((e) => !e);
  return (
    <Modal
      title={t("sec.fw.addRuleTitle", { backend: "ufw" })}
      size="wide"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button
            className="btn primary"
            disabled={!ok || busy}
            onClick={async () => {
              setBusy(true);
              await onSubmit({ action: f.action, direction: f.direction, port: f.port.trim(), proto: f.proto, from: f.from.trim() || "any", to: f.to.trim() || "any", interface: f.iface.trim(), comment: f.comment, position: f.position ? +f.position : 0, force: false });
              setBusy(false);
            }}
          >
            {busy ? <span className="spinner" /> : <Plus size={13} />} {t("sec.fw.addRule")}
          </button>
        </>
      }
    >
      <div className="sec-form-grid">
        <div className="field">
          <label>{t("sec.fw.action")}</label>
          <Select value={f.action} onChange={pick("action")} options={["allow", "deny", "reject", "limit"].map((a) => ({ value: a, label: tk(`sec.fw.act.${a}`, undefined, a) }))} />
        </div>
        <div className="field">
          <label>{t("sec.fw.direction")}</label>
          <Select
            value={f.direction}
            onChange={pick("direction")}
            disabled={f.action === "limit"}
            options={[
              { value: "in", label: t("sec.fw.dirIn") },
              { value: "out", label: t("sec.fw.dirOut") },
            ]}
          />
        </div>
        <div className="field">
          <label>{t("sec.fw.port")}</label>
          <input className={`input ${errs.port ? "invalid" : ""}`} placeholder="22, 8000:8100, 80,443" value={f.port} onChange={set("port")} autoFocus />
          <div className={errs.port ? "error" : "hint"}>{errs.port || t("sec.fw.portHint")}</div>
        </div>
        <div className="field">
          <label>{t("sec.fw.proto")}</label>
          <Select value={f.proto} onChange={pick("proto")} options={["tcp", "udp", { value: "any", label: t("sec.fw.any") }]} />
        </div>
        <div className="field">
          <label>{t("sec.fw.from")}</label>
          <input className={`input mono ${errs.from ? "invalid" : ""}`} placeholder={t("sec.fw.anyAddr")} value={f.from} onChange={set("from")} />
          {errs.from && <div className="error">{errs.from}</div>}
        </div>
        <div className="field">
          <label>{t("sec.fw.to")}</label>
          <input className={`input mono ${errs.to ? "invalid" : ""}`} placeholder={t("sec.fw.anyAddr")} value={f.to} onChange={set("to")} />
          {errs.to && <div className="error">{errs.to}</div>}
        </div>
        <div className="field">
          <label>{t("sec.fw.iface")}</label>
          <input className="input mono" placeholder="eth0" value={f.iface} onChange={set("iface")} />
        </div>
        <div className="field">
          <label>{t("sec.fw.position")}</label>
          <input className={`input ${errs.position ? "invalid" : ""}`} placeholder={t("sec.fw.positionHint")} value={f.position} onChange={set("position")} />
        </div>
      </div>
      <div className="field">
        <label>{t("sec.fw.comment")}</label>
        <input className="input" maxLength={80} value={f.comment} onChange={set("comment")} />
      </div>
      {errs.broad && <div className="hint-box sec-warnbox">{errs.broad}</div>}
      {(f.action === "deny" || f.action === "reject") && <div className="hint-box">{t("sec.fw.denyOrderHint")}</div>}
    </Modal>
  );
}

// ================= firewalld =================

function FirewalldView({ connId, active, canSec, info, prefill, clearPrefill }: FwProps) {
  const t = useT();
  const st = useRemote(() => sudo(connId, (pw) => SecurityService.FirewalldStatus(connId, pw)), [connId], { enabled: active });
  const [zone, setZone] = useState("");
  const [kind, setKind] = useState("port");
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const d = st.data;
  const z = d?.details?.find((x) => x.name === zone) ?? d?.details?.[0];
  const sshPorts = info.sshPorts ?? [22];

  useEffect(() => {
    if (prefill && canSec) {
      setKind(prefill.action === "deny" ? "rich-rule" : "port");
      setValue(
        prefill.action === "deny" && prefill.from
          ? `rule family="${prefill.from.includes(":") ? "ipv6" : "ipv4"}" source address="${prefill.from}" drop`
          : `${(prefill.port ?? "").replace(":", "-")}/${prefill.proto === "udp" ? "udp" : "tcp"}`,
      );
      clearPrefill();
    }
  }, [prefill?.seq]);

  const change = async (k: string, v: string, add: boolean) => {
    if (!z) return;
    if (!add) {
      const ok = await confirmOpts({ serverId: connId, title: t("sec.fw.deleteRuleQ"), message: <span className="mono">{`${z.name}: ${k} ${v}`}</span>, confirmText: t("common.delete") });
      if (!ok) return;
    }
    setBusy(true);
    const ok = await withForce(connId, sshPorts, (force) => sudo(connId, (pw) => SecurityService.FirewalldChange(connId, z.name, k, v, add, force, pw)), add ? t("sec.fw.ruleAdded") : t("sec.fw.ruleDeleted"));
    setBusy(false);
    if (ok) {
      if (add) setValue("");
      st.reload();
    }
  };

  if (st.error && !d) return <ErrorBox error={st.error} onRetry={st.reload} />;
  if (!d) return <Loading />;
  if (!d.running)
    return <Empty icon={<Flame size={28} />} title={t("sec.fwd.notRunning")} text={<pre className="sec-pre mono">sudo systemctl enable --now firewalld</pre>} />;

  const list = (label: string, k: string, items: string[] | null) => (
    <div className="sec-fwd-list">
      <div className="sec-fwd-label">{label}</div>
      <div className="chips">
        {(items ?? []).length === 0 && <span className="muted">—</span>}
        {(items ?? []).map((v) => (
          <span key={v} className="chip mono">
            {v}
            {canSec && (
              <button className="icon-btn sec-chip-x" title={t("common.delete")} disabled={busy} onClick={() => change(k, v, false)}>
                <X size={11} />
              </button>
            )}
          </span>
        ))}
      </div>
    </div>
  );

  const valid =
    kind === "port"
      ? /^\d{1,5}(-\d{1,5})?\/(tcp|udp)$/.test(value.trim())
      : kind === "service"
        ? /^[a-z0-9][a-z0-9._+-]{0,63}$/.test(value.trim())
        : kind === "source"
          ? validAddr(value) && value.trim() !== "" && value.trim() !== "any"
          : value.trim().startsWith("rule ");

  return (
    <>
      <div className="sec-fw-head">
        <StateBadge tone="ok">{t("sec.fwd.running")}</StateBadge>
        <label className="sec-policy">
          <span className="muted">{t("sec.fwd.zone")}</span>
          <Select
            size="sm"
            value={z?.name ?? ""}
            onChange={setZone}
            options={(d.details ?? []).map((x) => ({
              value: x.name,
              hint: [x.default && t("sec.fwd.default"), x.active && t("sec.fwd.active")].filter(Boolean).join(" · ") || undefined,
            }))}
          />
        </label>
        <span className="muted sec-small">{t("sec.fwd.allZones", { list: (d.zones ?? []).join(", ") })}</span>
        <div className="grow" />
        <button className="icon-btn" onClick={st.reload} title={t("common.refresh")} disabled={st.loading}>
          <RefreshCw size={14} className={st.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {z && (
        <div className="panel-box sec-pad">
          <KV
            items={[
              [t("sec.fwd.target"), <span className="mono">{z.target || "default"}</span>],
              [t("sec.fwd.interfaces"), <span className="mono">{(z.interfaces ?? []).join(", ") || "—"}</span>],
              [t("sec.fwd.masquerade"), z.masquerade ? t("sec.yes") : t("sec.no")],
              [t("sec.fwd.forwardPorts"), <span className="mono">{(z.forwardPorts ?? []).join(", ") || "—"}</span>],
            ]}
          />
          {list(t("sec.fwd.services"), "service", z.services)}
          {list(t("sec.fwd.ports"), "port", z.ports)}
          {list(t("sec.fwd.sources"), "source", z.sources)}
          <div className="sec-fwd-list">
            <div className="sec-fwd-label">{t("sec.fwd.rich")}</div>
            <div className="list-rows">
              {(z.richRules ?? []).length === 0 && <span className="muted">—</span>}
              {(z.richRules ?? []).map((r) => (
                <div key={r} className="list-row">
                  <span className="mono sec-small grow">{r}</span>
                  {canSec && (
                    <button className="icon-btn" title={t("common.delete")} disabled={busy} onClick={() => change("rich-rule", r, false)}>
                      <Trash2 size={13} />
                    </button>
                  )}
                </div>
              ))}
            </div>
          </div>
          {canSec && (
            <div className="toolbar sec-mt">
              <Select
                size="sm"
                value={kind}
                onChange={setKind}
                options={[
                  { value: "port", label: t("sec.fwd.kind.port") },
                  { value: "service", label: t("sec.fwd.kind.service") },
                  { value: "source", label: t("sec.fwd.kind.source") },
                  { value: "rich-rule", label: t("sec.fwd.kind.rich") },
                ]}
              />
              <input className="input input-sm mono grow" placeholder={tk(`sec.fwd.ph.${kind}`)} value={value} onChange={(e) => setValue(e.target.value)} />
              <button className="btn sm primary" disabled={!valid || busy} onClick={() => change(kind, value.trim(), true)}>
                <Plus size={13} /> {t("sec.fw.add")}
              </button>
            </div>
          )}
          <div className="muted sec-small sec-mt">{t("sec.fwd.permanentHint")}</div>
        </div>
      )}
    </>
  );
}

// ================= iptables =================

function IptablesView({ connId, active, canSec, info, prefill, clearPrefill }: FwProps) {
  const t = useT();
  const [v6, setV6] = useState(false);
  const st = useRemote(() => sudo(connId, (pw) => SecurityService.IptablesStatus(connId, v6, pw)), [connId, v6], { enabled: active });
  const [adding, setAdding] = useState<RulePrefill | null>(null);
  const [busy, setBusy] = useState(false);
  const d = st.data;
  const sshPorts = info.sshPorts ?? [22];

  useEffect(() => {
    if (prefill && canSec) {
      setAdding(prefill);
      clearPrefill();
    }
  }, [prefill?.seq]);

  const del = async (c: IptChain, r: IptRule) => {
    const ok = await confirmOpts({ serverId: connId, title: t("sec.fw.deleteRuleQ"), message: <span className="mono">{r.spec}</span>, confirmText: t("common.delete") });
    if (!ok) return;
    setBusy(true);
    const done = await withForce(connId, sshPorts, (force) => sudo(connId, (pw) => SecurityService.IptablesDeleteRule(connId, v6, c.name, r.num, r.spec, force, pw)), t("sec.fw.ruleDeleted"));
    setBusy(false);
    if (done) st.reload();
  };

  const save = async () => {
    setBusy(true);
    try {
      const m = await sudo(connId, (pw) => SecurityService.IptablesSave(connId, pw));
      toast(t("sec.ipt.saved", { method: m }), "success");
    } catch (e) {
      if (!isCancelled(e)) toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  if (st.error && !d) return <ErrorBox error={st.error} onRetry={st.reload} />;
  if (!d) return <Loading />;
  return (
    <>
      <div className="sec-fw-head">
        <div className="segmented sec-seg-sm">
          <button className={!v6 ? "on" : ""} onClick={() => setV6(false)}>
            IPv4
          </button>
          <button className={v6 ? "on" : ""} onClick={() => setV6(true)}>
            IPv6
          </button>
        </div>
        {d.variant && <span className="badge">{d.variant}</span>}
        <span className="muted sec-small">{d.persist ? t("sec.ipt.persistVia", { method: d.persist }) : t("sec.ipt.noPersist")}</span>
        <div className="grow" />
        <button className="icon-btn" onClick={st.reload} title={t("common.refresh")} disabled={st.loading}>
          <RefreshCw size={14} className={st.loading ? "spin-icon" : ""} />
        </button>
        {canSec && (
          <div className="sec-btn-row">
            <button className="btn sm" disabled={busy || !d.persist} title={d.persist ? "" : t("sec.ipt.noPersist")} onClick={save}>
              <Save size={13} /> {t("sec.ipt.save")}
            </button>
            <button className="btn sm primary" disabled={busy} onClick={() => setAdding({ seq: Date.now() })}>
              <Plus size={13} /> {t("sec.fw.addRule")}
            </button>
          </div>
        )}
      </div>
      {d.managedBy && <div className="hint-box sec-warnbox">{t("sec.ipt.managedBy", { tool: d.managedBy })}</div>}
      <div className="hint-box">{t("sec.ipt.persistHint")}</div>
      {(d.chains ?? []).map((c) => (
        <Section
          key={c.name}
          title={
            <>
              {c.name} <StateBadge tone={c.policy === "ACCEPT" ? "muted" : "warn"}>{t("sec.ipt.policy", { p: c.policy })}</StateBadge>
            </>
          }
        >
          {(c.rules ?? []).length === 0 ? (
            <div className="muted sec-small">{t("sec.fw.noRules")}</div>
          ) : (
            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th className="num">#</th>
                    <th>{t("sec.ipt.target")}</th>
                    <th>{t("sec.fw.proto")}</th>
                    <th>{t("sec.fw.from")}</th>
                    <th>{t("sec.fw.to")}</th>
                    <th>{t("sec.ipt.iface")}</th>
                    <th>{t("sec.ipt.match")}</th>
                    <th className="num">{t("sec.ipt.pkts")}</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {(c.rules ?? []).map((r) => (
                    <tr key={r.num} title={r.spec}>
                      <td className="num muted">{r.num}</td>
                      <td>
                        <StateBadge tone={r.target === "ACCEPT" ? "ok" : r.target === "DROP" || r.target === "REJECT" ? "err" : "muted"}>{r.target || "—"}</StateBadge>
                      </td>
                      <td className="mono">{r.proto}</td>
                      <td className="mono">{r.source}</td>
                      <td className="mono">{r.dest}</td>
                      <td className="mono muted">{[r.in, r.out].filter((x) => x && x !== "*").join(" → ") || "*"}</td>
                      <td className="mono sec-small">{r.extra}</td>
                      <td className="num muted">{r.pkts}</td>
                      <td style={{ width: 40 }}>
                        {canSec && (
                          <button className="icon-btn" title={t("common.delete")} disabled={busy} onClick={() => del(c, r)}>
                            <Trash2 size={13} />
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Section>
      ))}
      {adding && (
        <IptRuleModal
          v6={v6}
          prefill={adding}
          onClose={() => setAdding(null)}
          onSubmit={async (rule) => {
            const ok = await withForce(connId, sshPorts, (force) => sudo(connId, (pw) => SecurityService.IptablesAddRule(connId, { ...rule, force }, pw)), t("sec.fw.ruleAdded"));
            if (ok) {
              setAdding(null);
              st.reload();
            }
          }}
        />
      )}
    </>
  );
}

function IptRuleModal({ v6, prefill, onClose, onSubmit }: { v6: boolean; prefill: RulePrefill; onClose: () => void; onSubmit: (r: { v6: boolean; chain: string; target: string; proto: string; port: string; source: string; interface: string; comment: string; top: boolean; force: boolean }) => Promise<void> }) {
  const t = useT();
  const [f, setF] = useState({
    chain: "INPUT",
    target: (prefill.action === "deny" ? "DROP" : "ACCEPT") as string,
    proto: prefill.proto ?? "tcp",
    port: prefill.port ?? "",
    source: prefill.from ?? "",
    iface: "",
    comment: prefill.comment ?? "",
    top: true,
  });
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const pick = (k: keyof typeof f) => (v: string) => setF({ ...f, [k]: v });
  const portErr = f.port.includes(",") || !validPortSpec(f.port) ? t("sec.fw.badPort") : f.port && f.proto !== "tcp" && f.proto !== "udp" ? t("sec.fw.rangeProto") : "";
  const srcErr = !validAddr(f.source) ? t("sec.fw.badAddr") : f.source && f.source !== "any" && f.source.includes(":") !== v6 ? t("sec.ipt.familyMismatch") : "";
  const ok = !portErr && !srcErr;
  return (
    <Modal
      title={t("sec.fw.addRuleTitle", { backend: v6 ? "ip6tables" : "iptables" })}
      size="wide"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button
            className="btn primary"
            disabled={!ok || busy}
            onClick={async () => {
              setBusy(true);
              await onSubmit({ v6, chain: f.chain, target: f.target, proto: f.proto, port: f.port.trim(), source: f.source.trim() || "any", interface: f.iface.trim(), comment: f.comment, top: f.top, force: false });
              setBusy(false);
            }}
          >
            {busy ? <span className="spinner" /> : <Plus size={13} />} {t("sec.fw.addRule")}
          </button>
        </>
      }
    >
      <div className="sec-form-grid">
        <div className="field">
          <label>{t("sec.ipt.chain")}</label>
          <Select value={f.chain} onChange={pick("chain")} options={["INPUT", "OUTPUT", "FORWARD"]} />
        </div>
        <div className="field">
          <label>{t("sec.ipt.target")}</label>
          <Select value={f.target} onChange={pick("target")} options={["ACCEPT", "DROP", "REJECT"]} />
        </div>
        <div className="field">
          <label>{t("sec.fw.proto")}</label>
          <Select value={f.proto} onChange={pick("proto")} options={["tcp", "udp", "icmp", "all"]} />
        </div>
        <div className="field">
          <label>{t("sec.fw.port")}</label>
          <input className={`input ${portErr ? "invalid" : ""}`} placeholder="22, 8000:8100" value={f.port} onChange={set("port")} autoFocus />
          {portErr && <div className="error">{portErr}</div>}
        </div>
        <div className="field">
          <label>{t("sec.ipt.source")}</label>
          <input className={`input mono ${srcErr ? "invalid" : ""}`} placeholder={t("sec.fw.anyAddr")} value={f.source} onChange={set("source")} />
          {srcErr && <div className="error">{srcErr}</div>}
        </div>
        <div className="field">
          <label>{t("sec.fw.iface")}</label>
          <input className="input mono" placeholder="eth0" value={f.iface} onChange={set("iface")} />
        </div>
      </div>
      <div className="field">
        <label>{t("sec.fw.comment")}</label>
        <input className="input" maxLength={200} value={f.comment} onChange={set("comment")} />
      </div>
      <label className="check">
        <input type="checkbox" checked={f.top} onChange={(e) => setF({ ...f, top: e.target.checked })} /> {t("sec.ipt.top")}
      </label>
      <div className="hint-box sec-mt">{t("sec.ipt.persistHint")}</div>
    </Modal>
  );
}

// ================= nftables =================

function NftView({ connId, active }: FwProps) {
  const t = useT();
  const rs = useRemote(() => sudo(connId, (pw) => SecurityService.NftRuleset(connId, pw)), [connId], { enabled: active });
  const text = useMemo(() => rs.data ?? "", [rs.data]);
  if (rs.error && rs.data === undefined) return <ErrorBox error={rs.error} onRetry={rs.reload} />;
  if (rs.data === undefined) return <Loading />;
  return (
    <>
      <div className="sec-fw-head">
        <span className="muted">{t("sec.nft.readOnly")}</span>
        <div className="grow" />
        <button className="icon-btn" onClick={rs.reload} title={t("common.refresh")} disabled={rs.loading}>
          <RefreshCw size={14} className={rs.loading ? "spin-icon" : ""} />
        </button>
      </div>
      {text.trim() === "" ? <Empty title={t("sec.nft.empty")} /> : <CodeEditor value={text} readOnly height={520} language="plaintext" />}
    </>
  );
}
