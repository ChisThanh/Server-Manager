import { useMemo, useState, type ReactNode } from "react";
import { KeyRound, Lock, MoreHorizontal, Plus, RefreshCw, ShieldCheck, Unlock, UserPlus, Users as UsersIcon } from "lucide-react";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { Modal } from "../Overlays";
import { toast, useUI } from "../../store/ui";
import { useT } from "../../i18n";
import { SecurityService, act, confirmOpts, errCode, errMsg, fmtTime, isCancelled, reUser, sudo, type LinuxUser, type SecViewProps, type UsersResult } from "./common";
import { Select } from "../../ui/Select";

type SortKey = "name" | "uid" | "last";

export function UsersView({ connId, active, canUsers, go }: SecViewProps) {
  const t = useT();
  const res = useRemote(() => sudo(connId, (pw) => SecurityService.Users(connId, pw)), [connId], { enabled: active });
  const [q, setQ] = useState("");
  const [showSystem, setShowSystem] = useState(false);
  const [sort, setSort] = useState<{ k: SortKey; asc: boolean }>({ k: "uid", asc: true });
  const [adding, setAdding] = useState(false);
  const [groupsFor, setGroupsFor] = useState<LinuxUser | null>(null);
  const showMenu = useUI((s) => s.showMenu);
  const d = res.data;

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const list = (d?.users ?? []).filter((u) => (showSystem || !u.system) && (!needle || u.name.toLowerCase().includes(needle) || u.gecos.toLowerCase().includes(needle) || (u.groups ?? []).some((g) => g.includes(needle))));
    const dir = sort.asc ? 1 : -1;
    return list.sort((a, b) => {
      if (sort.k === "name") return a.name.localeCompare(b.name) * dir;
      if (sort.k === "last") return (a.lastLogin - b.lastLogin) * dir;
      return (a.uid - b.uid) * dir;
    });
  }, [d, q, showSystem, sort]);

  const reload = () => res.reload();
  const done = (ok: boolean) => ok && reload();

  const setPassword = async (u: LinuxUser) => {
    const r = await useUI.getState().openDialog({
      title: t("sec.users.setPwTitle", { name: u.name }),
      message: u.isLogin ? <div className="hint-box sec-warnbox">{t("sec.users.setPwSelf")}</div> : undefined,
      confirmText: t("sec.users.setPw"),
      fields: [
        { name: "p1", label: t("sec.users.newPw"), type: "password", autoFocus: true, validate: (v) => (v.length < 8 ? t("sec.users.pwShort") : /[\r\n]/.test(v) ? t("sec.users.pwInvalid") : null) },
        { name: "p2", label: t("sec.users.confirmPw"), type: "password" },
      ],
    });
    if (r?.action !== "ok") return;
    if (r.values.p1 !== r.values.p2) {
      toast(t("sec.users.pwMismatch"), "error");
      return;
    }
    done(await act(() => sudo(connId, (pw) => SecurityService.SetPassword(connId, u.name, String(r.values.p1), pw)), t("sec.users.pwSet", { name: u.name })));
  };

  const lock = async (u: LinuxUser, locked: boolean) => {
    if (locked) {
      const ok = await confirmOpts({ serverId: connId, title: t("sec.users.lockQ", { name: u.name }), message: t("sec.users.lockMsg"), confirmText: t("sec.users.lock") });
      if (!ok) return;
    }
    done(await act(() => sudo(connId, (pw) => SecurityService.SetLocked(connId, u.name, locked, pw)), locked ? t("sec.users.locked", { name: u.name }) : t("sec.users.unlocked", { name: u.name })));
  };

  const remove = async (u: LinuxUser) => {
    const opts = await confirmOpts({
      serverId: connId,
      title: t("sec.users.deleteQ", { name: u.name }),
      message: t("sec.users.deleteMsg", { home: u.home }),
      confirmText: t("common.delete"),
      typeWord: u.name,
      options: [{ name: "home", label: t("sec.users.removeHome", { home: u.home }) }],
    });
    if (!opts) return;
    done(await act(() => sudo(connId, (pw) => SecurityService.DeleteUser(connId, u.name, !!opts.home, pw)), t("sec.users.deleted", { name: u.name })));
  };

  const menu = (u: LinuxUser, x: number, y: number) => {
    const self = u.isLogin;
    showMenu(x, y, [
      { label: t("sec.users.keys"), icon: <KeyRound size={13} />, onClick: () => go("keys", { user: u.isLogin ? "" : u.name }) },
      { separator: true },
      { label: t("sec.users.setPw"), disabled: !canUsers, onClick: () => setPassword(u) },
      u.locked
        ? { label: t("sec.users.unlock"), icon: <Unlock size={13} />, disabled: !canUsers, onClick: () => lock(u, false) }
        : { label: t("sec.users.lock"), icon: <Lock size={13} />, disabled: !canUsers || self, onClick: () => lock(u, true) },
      { label: t("sec.users.groups"), disabled: !canUsers, onClick: () => setGroupsFor(u) },
      { separator: true },
      { label: t("common.delete"), danger: true, disabled: !canUsers || self || u.uid === 0 || u.system, onClick: () => remove(u) },
    ]);
  };

  const th = (k: SortKey, label: string, cls = "") => (
    <th className={`sortable ${cls}`} onClick={() => setSort((s) => ({ k, asc: s.k === k ? !s.asc : true }))}>
      {label} {sort.k === k ? (sort.asc ? "▲" : "▼") : ""}
    </th>
  );

  if (res.error && !d) return <ErrorBox error={res.error} onRetry={res.reload} />;
  if (!d) return <Loading />;

  return (
    <div className="sec-users">
      <div className="toolbar">
        <input className="input input-sm" placeholder={t("sec.users.filter")} value={q} onChange={(e) => setQ(e.target.value)} />
        <label className="check">
          <input type="checkbox" checked={showSystem} onChange={(e) => setShowSystem(e.target.checked)} /> {t("sec.users.showSystem")}
        </label>
        <div className="grow" />
        <span className="muted sec-small">{t("sec.users.count", { n: rows.length, total: d.users?.length ?? 0 })}</span>
        <button className="icon-btn" onClick={reload} title={t("common.refresh")} disabled={res.loading}>
          <RefreshCw size={14} className={res.loading ? "spin-icon" : ""} />
        </button>
        {canUsers && (
          <button className="btn primary sm" onClick={() => setAdding(true)}>
            <UserPlus size={13} /> {t("sec.users.add")}
          </button>
        )}
      </div>
      {d.limited && <div className="hint-box">{t("sec.users.limited")}</div>}
      {!canUsers && <div className="hint-box">{t("sec.users.readOnly")}</div>}
      {rows.length === 0 ? (
        <Empty icon={<UsersIcon size={28} />} title={t("sec.users.none")} />
      ) : (
        <div className="table-wrap">
          <table className="grid">
            <thead>
              <tr>
                {th("name", t("sec.col.user"))}
                {th("uid", "UID", "num")}
                <th>{t("sec.users.groupsCol")}</th>
                <th>{t("sec.users.shell")}</th>
                <th>{t("sec.users.status")}</th>
                {th("last", t("sec.users.lastLogin"))}
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((u) => (
                <tr
                  key={u.name}
                  onContextMenu={(e) => {
                    e.preventDefault();
                    menu(u, e.clientX, e.clientY);
                  }}
                >
                  <td>
                    <div className="mono">
                      {u.name} {u.isLogin && <StateBadge tone="info">{t("sec.users.you")}</StateBadge>}
                      {u.admin && (
                        <span className="sec-admin" title={t("sec.users.adminHint")}>
                          <ShieldCheck size={12} />
                        </span>
                      )}
                    </div>
                    <div className="muted sec-small">{u.gecos.split(",")[0] || u.home}</div>
                  </td>
                  <td className="num mono">{u.uid}</td>
                  <td>
                    <div className="chips">
                      {u.primaryGroup && <span className="chip mono sec-small sec-chip-primary">{u.primaryGroup}</span>}
                      {(u.groups ?? []).map((g) => (
                        <span key={g} className={`chip mono sec-small ${["sudo", "wheel", "admin"].includes(g) ? "sec-chip-admin" : ""}`}>
                          {g}
                        </span>
                      ))}
                    </div>
                  </td>
                  <td className="mono sec-small">{u.shell}</td>
                  <td>
                    <UserStatus u={u} />
                  </td>
                  <td className="sec-small">
                    {u.lastLogin ? (
                      <>
                        {fmtTime(u.lastLogin)}
                        {u.lastFrom && <div className="muted mono">{u.lastFrom}</div>}
                      </>
                    ) : (
                      <span className="muted">{t("sec.users.never")}</span>
                    )}
                  </td>
                  <td style={{ width: 40 }}>
                    <button className="icon-btn" onClick={(e) => menu(u, e.clientX, e.clientY)} title={t("sec.users.actions")}>
                      <MoreHorizontal size={14} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {adding && <AddUserModal connId={connId} data={d} onClose={() => setAdding(false)} onDone={() => { setAdding(false); reload(); }} />}
      {groupsFor && <GroupsModal connId={connId} user={groupsFor} data={d} onClose={() => setGroupsFor(null)} onDone={() => { setGroupsFor(null); reload(); }} />}
    </div>
  );
}

function UserStatus({ u }: { u: LinuxUser }) {
  const t = useT();
  const parts: ReactNode[] = [];
  if (u.pwStatus === "NP") parts.push(<StateBadge key="np" tone="err">{t("sec.users.st.empty")}</StateBadge>);
  else if (u.locked) parts.push(<StateBadge key="l" tone="warn">{t("sec.users.st.locked")}</StateBadge>);
  else if (u.pwStatus === "NS") parts.push(<StateBadge key="ns" tone="muted">{t("sec.users.st.noPw")}</StateBadge>);
  else if (u.pwStatus === "P") parts.push(<StateBadge key="p" tone="ok">{t("sec.users.st.pw")}</StateBadge>);
  if (u.expired) parts.push(<StateBadge key="e" tone="warn">{t("sec.users.st.expired")}</StateBadge>);
  if (!u.canLogin) parts.push(<StateBadge key="nl" tone="muted">{t("sec.users.st.nologin")}</StateBadge>);
  return <div className="chips">{parts}</div>;
}

function AddUserModal({ connId, data, onClose, onDone }: { connId: string; data: UsersResult; onClose: () => void; onDone: () => void }) {
  const t = useT();
  const shells = (data.shells ?? []).length ? data.shells! : ["/bin/sh"];
  const [f, setF] = useState({
    name: "",
    gecos: "",
    shell: shells.includes("/bin/bash") ? "/bin/bash" : shells[0],
    createHome: true,
    admin: false,
    password: "",
    password2: "",
    key: "",
  });
  const [busy, setBusy] = useState(false);
  const exists = (data.users ?? []).some((u) => u.name === f.name) || (data.groups ?? []).some((g) => g.name === f.name);
  const nameErr = f.name && !reUser.test(f.name) ? t("sec.users.badName") : exists ? t("sec.users.exists") : "";
  const pwErr = f.password && f.password.length < 8 ? t("sec.users.pwShort") : f.password !== f.password2 ? t("sec.users.pwMismatch") : "";
  const keyErr = f.key.trim() && !/^(?:\S+\s+)?(ssh-|ecdsa-|sk-)\S+\s+[A-Za-z0-9+/=]{40,}/.test(f.key.trim()) ? t("sec.keys.badFormat") : "";
  const ok = f.name && !nameErr && !pwErr && !keyErr;
  const adminGroup = (data.adminGroups ?? [])[0];

  const submit = async () => {
    setBusy(true);
    try {
      await sudo(connId, (pw) =>
        SecurityService.AddUser(connId, { name: f.name, gecos: f.gecos, shell: f.shell, createHome: f.createHome, admin: f.admin, password: f.password, sshKey: f.key.trim() }, pw),
      );
      toast(t("sec.users.created", { name: f.name }), "success");
      onDone();
    } catch (e) {
      if (!isCancelled(e)) toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  return (
    <Modal
      title={t("sec.users.addTitle")}
      size="wide"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" disabled={!ok || busy} onClick={submit}>
            {busy ? <span className="spinner" /> : <UserPlus size={13} />} {t("sec.users.create")}
          </button>
        </>
      }
    >
      <div className="sec-form-grid">
        <div className="field">
          <label>{t("sec.users.name")}</label>
          <input className={`input mono ${nameErr ? "invalid" : ""}`} autoFocus value={f.name} onChange={(e) => setF({ ...f, name: e.target.value.trim() })} placeholder="deploy" />
          <div className={nameErr ? "error" : "hint"}>{nameErr || t("sec.users.nameHint")}</div>
        </div>
        <div className="field">
          <label>{t("sec.users.fullName")}</label>
          <input className="input" maxLength={64} value={f.gecos} onChange={set("gecos")} />
        </div>
        <div className="field">
          <label>{t("sec.users.shell")}</label>
          <Select mono value={f.shell} onChange={(v) => setF({ ...f, shell: v })} options={shells} />
        </div>
        <div className="field sec-checks-col">
          <label className="check">
            <input type="checkbox" checked={f.createHome} onChange={(e) => setF({ ...f, createHome: e.target.checked })} /> {t("sec.users.createHome")}
          </label>
          <label className="check" title={adminGroup ? "" : t("sec.users.noAdminGroup")}>
            <input type="checkbox" checked={f.admin} disabled={!adminGroup} onChange={(e) => setF({ ...f, admin: e.target.checked })} /> {t("sec.users.makeAdmin", { group: adminGroup ?? "sudo" })}
          </label>
        </div>
        <div className="field">
          <label>{t("sec.users.initialPw")}</label>
          <input className="input" type="password" autoComplete="new-password" value={f.password} onChange={set("password")} />
          <div className="hint">{t("sec.users.initialPwHint")}</div>
        </div>
        <div className="field">
          <label>{t("sec.users.confirmPw")}</label>
          <input className={`input ${pwErr ? "invalid" : ""}`} type="password" autoComplete="new-password" value={f.password2} onChange={set("password2")} />
          {pwErr && <div className="error">{pwErr}</div>}
        </div>
      </div>
      <div className="field">
        <label>{t("sec.users.sshKey")}</label>
        <textarea className={`input mono sec-textarea ${keyErr ? "invalid" : ""}`} rows={3} placeholder="ssh-ed25519 AAAA… user@laptop" value={f.key} onChange={set("key")} />
        <div className={keyErr ? "error" : "hint"}>{keyErr || t("sec.users.sshKeyHint")}</div>
      </div>
    </Modal>
  );
}

function GroupsModal({ connId, user, data, onClose, onDone }: { connId: string; user: LinuxUser; data: UsersResult; onClose: () => void; onDone: () => void }) {
  const t = useT();
  const [sel, setSel] = useState<Set<string>>(() => new Set(user.groups ?? []));
  const [showSystem, setShowSystem] = useState(false);
  const [newGroup, setNewGroup] = useState("");
  const [busy, setBusy] = useState(false);
  const [groups, setGroups] = useState(data.groups ?? []);
  const admin = new Set(data.adminGroups ?? []);
  const visible = groups
    .filter((g) => g.name !== user.primaryGroup && (showSystem || !g.system || admin.has(g.name) || sel.has(g.name) || ["docker", "adm", "www-data", "systemd-journal"].includes(g.name)))
    .sort((a, b) => Number(admin.has(b.name)) - Number(admin.has(a.name)) || a.name.localeCompare(b.name));
  const cur = new Set(user.groups ?? []);
  const add = [...sel].filter((g) => !cur.has(g));
  const remove = [...cur].filter((g) => !sel.has(g));

  const save = async (force = false): Promise<void> => {
    setBusy(true);
    try {
      await sudo(connId, (pw) => SecurityService.SetGroups(connId, user.name, add, remove, force, pw));
      toast(t("sec.users.groupsSaved", { name: user.name }), "success");
      onDone();
    } catch (e) {
      if (isCancelled(e)) return;
      if (errCode(e) === "sec.users.selfAdmin" && !force) {
        const ok = await confirmOpts({ serverId: connId, title: t("sec.users.selfAdminTitle"), message: t("sec.users.selfAdminMsg"), confirmText: t("sec.users.removeAnyway"), typeWord: user.name });
        if (ok) return save(true);
        return;
      }
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  const createGroup = async () => {
    const name = newGroup.trim();
    if (await act(() => sudo(connId, (pw) => SecurityService.AddGroup(connId, name, pw)), t("sec.users.groupCreated", { name }))) {
      setGroups([...groups, { name, gid: 0, members: [], system: false }]);
      setSel(new Set(sel).add(name));
      setNewGroup("");
    }
  };

  return (
    <Modal
      title={t("sec.users.groupsTitle", { name: user.name })}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button className="btn primary" disabled={busy || (add.length === 0 && remove.length === 0)} onClick={() => save()}>
            {busy && <span className="spinner" />} {t("common.save")}
          </button>
        </>
      }
    >
      <div className="muted sec-small sec-mb">{t("sec.users.primaryGroup", { group: user.primaryGroup })}</div>
      <div className="sec-group-list">
        {visible.map((g) => (
          <label key={g.name} className="check">
            <input
              type="checkbox"
              checked={sel.has(g.name)}
              onChange={(e) => {
                const n = new Set(sel);
                if (e.target.checked) n.add(g.name);
                else n.delete(g.name);
                setSel(n);
              }}
            />
            <span className="mono">{g.name}</span>
            {admin.has(g.name) && <StateBadge tone="warn">{t("sec.users.adminGroup")}</StateBadge>}
          </label>
        ))}
      </div>
      <label className="check sec-mt">
        <input type="checkbox" checked={showSystem} onChange={(e) => setShowSystem(e.target.checked)} /> {t("sec.users.showSystemGroups")}
      </label>
      <div className="toolbar sec-mt">
        <input className="input input-sm mono" placeholder={t("sec.users.newGroup")} value={newGroup} onChange={(e) => setNewGroup(e.target.value.trim())} />
        <button className="btn sm" disabled={!reUser.test(newGroup) || groups.some((g) => g.name === newGroup) || busy} onClick={createGroup}>
          <Plus size={13} /> {t("sec.users.createGroup")}
        </button>
      </div>
    </Modal>
  );
}
