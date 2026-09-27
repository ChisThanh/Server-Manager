import { useState } from "react";
import { AlertTriangle, KeyRound, Plus, RefreshCw, Trash2 } from "lucide-react";
import { Empty, ErrorBox, Loading } from "../../ui/Page";
import { StateBadge } from "../../ui/Status";
import { useRemote } from "../../ui/hooks";
import { Modal } from "../Overlays";
import { toast } from "../../store/ui";
import { useT } from "../../i18n";
import { SecurityService, act, confirmOpts, errCode, errMsg, isCancelled, sudo, type AuthorizedKey, type SecViewProps } from "./common";
import { Select } from "../../ui/Select";

export function KeysView({ connId, active, canSec, user, setUser }: SecViewProps & { user: string; setUser: (u: string) => void }) {
  const t = useT();
  const [adding, setAdding] = useState(false);
  const keys = useRemote(() => sudo(connId, (pw) => SecurityService.ListKeys(connId, user, pw)), [connId, user], { enabled: active });
  const users = useRemote(() => sudo(connId, (pw) => SecurityService.Users(connId, pw)), [connId], { enabled: active });
  const d = keys.data;
  const choices = (users.data?.users ?? []).filter((u) => u.canLogin && (!u.system || u.uid === 0));

  const remove = async (k: AuthorizedKey) => {
    const warnInUse = k.inUse === "maybe" ? t("sec.keys.maybeInUse") : "";
    const ok = await confirmOpts({
      serverId: connId,
      title: t("sec.keys.removeQ"),
      message: (
        <div>
          <div className="mono sec-small">{k.fingerprint}</div>
          <div>{k.comment}</div>
          {warnInUse && <div className="hint-box sec-warnbox sec-mt">{warnInUse}</div>}
        </div>
      ),
      confirmText: t("common.delete"),
    });
    if (!ok) return;
    try {
      await sudo(connId, (pw) => SecurityService.RemoveKey(connId, user, k.fingerprint, false, pw));
    } catch (e) {
      if (isCancelled(e)) return;
      if (errCode(e) !== "sec.keys.lastKey") {
        toast(errMsg(e), "error");
        return;
      }
      const again = await confirmOpts({ serverId: connId, title: t("sec.keys.lastKeyTitle"), message: t("sec.keys.lastKeyMsg", { user: d?.user ?? user }), confirmText: t("sec.keys.removeAnyway"), typeWord: d?.user ?? user });
      if (!again) return;
      if (!(await act(() => sudo(connId, (pw) => SecurityService.RemoveKey(connId, user, k.fingerprint, true, pw))))) return;
    }
    toast(t("sec.keys.removed"), "success");
    keys.reload();
  };

  return (
    <div className="sec-keys">
      <div className="toolbar">
        <label className="muted">{t("sec.keys.user")}</label>
        <Select
          size="sm"
          className="sec-user-select"
          value={user}
          onChange={setUser}
          options={[
            { value: "", label: t("sec.keys.currentUser", { user: d?.isLogin ? d.user : (users.data?.loginUser ?? "") }) },
            ...choices.filter((u) => u.name !== users.data?.loginUser).map((u) => u.name),
            ...(user && !choices.some((u) => u.name === user) ? [user] : []),
          ]}
        />
        {d && <span className="mono muted sec-small">{d.path}</span>}
        <div className="grow" />
        <button className="icon-btn" onClick={keys.reload} title={t("common.refresh")} disabled={keys.loading}>
          <RefreshCw size={14} className={keys.loading ? "spin-icon" : ""} />
        </button>
        {canSec && (
          <button className="btn primary sm" onClick={() => setAdding(true)} disabled={!d}>
            <Plus size={13} /> {t("sec.keys.add")}
          </button>
        )}
      </div>

      {keys.error && !d ? (
        <ErrorBox error={keys.error} onRetry={keys.reload} />
      ) : !d ? (
        <Loading />
      ) : (
        <>
          {(d.problems ?? []).length > 0 && (
            <div className="hint-box sec-warnbox">
              <AlertTriangle size={13} /> {t("sec.keys.permProblems", { list: (d.problems ?? []).join(", ") })}
            </div>
          )}
          {d.passwordAuth === "no" && d.keys!.filter((k) => !k.invalid).length <= 1 && <div className="hint-box">{t("sec.keys.pwOffHint")}</div>}
          {(d.keys ?? []).length === 0 ? (
            <Empty icon={<KeyRound size={28} />} title={t("sec.keys.none")} text={d.exists ? t("sec.keys.noneHint") : t("sec.keys.noFile")} />
          ) : (
            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th>{t("sec.keys.type")}</th>
                    <th>{t("sec.keys.fingerprint")}</th>
                    <th>{t("sec.keys.comment")}</th>
                    <th>{t("sec.keys.options")}</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {(d.keys ?? []).map((k) => (
                    <tr key={k.line}>
                      {k.invalid ? (
                        <>
                          <td>
                            <StateBadge tone="err">{t("sec.keys.invalidLine", { line: k.line })}</StateBadge>
                          </td>
                          <td colSpan={3} className="mono muted sec-small">
                            {k.raw}
                          </td>
                        </>
                      ) : (
                        <>
                          <td className="mono">
                            {k.type.replace(/^ssh-/, "")}
                            {k.bits ? <span className="muted"> · {k.bits}</span> : null}
                          </td>
                          <td className="mono sec-small">
                            {k.fingerprint}
                            {k.inUse === "yes" && (
                              <StateBadge tone="info">{t("sec.keys.inUse")}</StateBadge>
                            )}
                            {k.inUse === "maybe" && <StateBadge tone="muted">{t("sec.keys.maybe")}</StateBadge>}
                          </td>
                          <td>{k.comment || <span className="muted">—</span>}</td>
                          <td>
                            <div className="chips">
                              {(k.options ?? []).map((o) => (
                                <span className="chip mono sec-small" key={o}>
                                  {o}
                                </span>
                              ))}
                            </div>
                          </td>
                        </>
                      )}
                      <td style={{ width: 40 }}>
                        {canSec && !k.invalid && (
                          <button className="icon-btn" title={k.inUse === "yes" ? t("sec.keys.inUseHint") : t("common.delete")} disabled={k.inUse === "yes"} onClick={() => remove(k)}>
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
        </>
      )}
      {adding && d && (
        <AddKeyModal
          user={d.user}
          onClose={() => setAdding(false)}
          onAdd={async (key) => {
            const ok = await act(() => sudo(connId, (pw) => SecurityService.AddKey(connId, user, key, pw)), t("sec.keys.added"));
            if (ok) {
              setAdding(false);
              keys.reload();
            }
          }}
        />
      )}
    </div>
  );
}

function AddKeyModal({ user, onClose, onAdd }: { user: string; onClose: () => void; onAdd: (key: string) => Promise<void> }) {
  const t = useT();
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const trimmed = key.trim();
  const looksValid = /^(?:\S+\s+)?(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp\d+|sk-\S+)\s+[A-Za-z0-9+/=]{40,}(\s.*)?$/.test(trimmed) && !trimmed.includes("\n");
  return (
    <Modal
      title={t("sec.keys.addTitle", { user })}
      size="wide"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            {t("common.cancel")}
          </button>
          <button
            className="btn primary"
            disabled={!looksValid || busy}
            onClick={async () => {
              setBusy(true);
              await onAdd(trimmed);
              setBusy(false);
            }}
          >
            {busy ? <span className="spinner" /> : <Plus size={13} />} {t("sec.keys.add")}
          </button>
        </>
      }
    >
      <div className="field">
        <label>{t("sec.keys.paste")}</label>
        <textarea className="input mono sec-textarea" rows={5} autoFocus placeholder="ssh-ed25519 AAAAC3Nza… user@laptop" value={key} onChange={(e) => setKey(e.target.value)} />
        <div className={trimmed && !looksValid ? "error" : "hint"}>{trimmed && !looksValid ? t("sec.keys.badFormat") : t("sec.keys.pasteHint")}</div>
      </div>
    </Modal>
  );
}
