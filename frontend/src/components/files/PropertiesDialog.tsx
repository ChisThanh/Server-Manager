import { useState } from "react";
import { Modal } from "../Overlays";
import { FileService, errMsg, type FileEntry } from "../../lib/api";
import { formatBytes, formatDate, octal } from "../../lib/format";
import { toast } from "../../store/ui";
import { withSudo, withSudoFallback } from "../../store/sudo";
import { useT } from "../../i18n";

const WHO = ["props.whoOwner", "props.whoGroup", "props.whoOther"] as const;
const BITS = [
  ["props.read", 4],
  ["props.write", 2],
  ["props.exec", 1],
] as const;

export function PropertiesDialog(props: { connId: string; entry: FileEntry; onClose: () => void; onDone: () => void }) {
  const t = useT();
  const { entry } = props;
  const [mode, setMode] = useState(entry.perm & 0o7777);
  const [text, setText] = useState(octal(entry.perm));
  const [recursive, setRecursive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [owner, setOwner] = useState(entry.owner);
  const [group, setGroup] = useState(entry.group);
  const ownerChanged = owner.trim() !== entry.owner || group.trim() !== entry.group;

  const setBits = (m: number) => {
    setMode(m);
    setText(octal(m));
  };
  const onText = (v: string) => {
    setText(v);
    if (/^[0-7]{3,4}$/.test(v)) setMode(parseInt(v, 8));
  };
  const has = (shift: number, bit: number) => (mode >> shift) & bit;
  const toggle = (shift: number, bit: number) => setBits(mode ^ (bit << shift));
  const modeChanged = mode !== (entry.perm & 0o7777) || recursive;
  const changed = modeChanged || ownerChanged;

  const apply = async () => {
    setBusy(true);
    try {
      if (ownerChanged) {
        const o = owner.trim() !== entry.owner ? owner.trim() : "";
        const g = group.trim() !== entry.group ? group.trim() : "";
        await withSudo(props.connId, (pw) => FileService.Chown(props.connId, entry.path, o, g, recursive, pw));
        toast(t("app.props.ownerChanged", { spec: `${owner.trim()}:${group.trim()}` }), "success");
      }
      if (modeChanged) {
        const ok = await withSudoFallback(
          props.connId,
          () => FileService.Chmod(props.connId, entry.path, mode, recursive),
          (pw) => FileService.SudoOp(props.connId, "chmod", [entry.path], "", mode, recursive, pw),
        );
        if (!ok) return;
        toast(t("props.changed", { mode: octal(mode) }), "success");
      }
      props.onDone();
      props.onClose();
    } catch (e) {
      toast(errMsg(e), "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      title={t("props.title", { name: entry.name })}
      onClose={props.onClose}
      footer={
        <>
          <button className="btn" onClick={props.onClose}>
            {t("common.close")}
          </button>
          <button className="btn primary" onClick={apply} disabled={!changed || busy || !/^[0-7]{3,4}$/.test(text)}>
            {t("props.apply")}
          </button>
        </>
      }
    >
      <div className="kv">
        <span className="k">{t("props.path")}</span>
        <span className="v">{entry.path}</span>
        <span className="k">{t("props.type")}</span>
        <span className="v">{entry.isLink ? t("props.symlink", { target: entry.linkTarget }) : entry.isDir ? t("props.dir") : t("props.file")}</span>
        {!entry.isDir && (
          <>
            <span className="k">{t("props.size")}</span>
            <span className="v">
              {formatBytes(entry.size)} ({t("props.bytes", { n: entry.size.toLocaleString() })})
            </span>
          </>
        )}
        <span className="k">{t("props.modified")}</span>
        <span className="v">{formatDate(entry.modTime)}</span>
        <span className="k">{t("props.owner")}</span>
        <span className="v">
          {entry.owner}:{entry.group}
        </span>
        <span className="k">{t("props.perm")}</span>
        <span className="v">{entry.mode}</span>
      </div>

      <div className="perm-grid">
        <span />
        {BITS.map(([label]) => (
          <span key={label} className="h">
            {t(label)}
          </span>
        ))}
        {WHO.map((who, i) => {
          const shift = (2 - i) * 3;
          return (
            <FragmentRow key={who} label={t(who)}>
              {BITS.map(([label, bit]) => (
                <input key={label} type="checkbox" checked={!!has(shift, bit)} onChange={() => toggle(shift, bit)} />
              ))}
            </FragmentRow>
          );
        })}
      </div>
      <div className="row">
        <div className="field">
          <label>{t("app.props.owner")}</label>
          <input className="input mono" value={owner} onChange={(e) => setOwner(e.target.value)} spellCheck={false} />
        </div>
        <div className="field">
          <label>{t("app.props.group")}</label>
          <input className="input mono" value={group} onChange={(e) => setGroup(e.target.value)} spellCheck={false} />
        </div>
      </div>
      <div className="row" style={{ alignItems: "center" }}>
        <div className="field" style={{ maxWidth: 120, marginBottom: 0 }}>
          <label>{t("props.octal")}</label>
          <input className={`input mono ${/^[0-7]{3,4}$/.test(text) ? "" : "invalid"}`} value={text} onChange={(e) => onText(e.target.value)} maxLength={4} />
        </div>
        {entry.isDir && (
          <label className="check" style={{ marginTop: 18 }}>
            <input type="checkbox" checked={recursive} onChange={(e) => setRecursive(e.target.checked)} />
            {t("props.recursive")}
          </label>
        )}
      </div>
    </Modal>
  );
}

function FragmentRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <span className="r">{label}</span>
      {children}
    </>
  );
}
