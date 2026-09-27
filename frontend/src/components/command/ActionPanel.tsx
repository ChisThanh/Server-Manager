import { useEffect, useMemo, useState } from "react";
import { BookmarkPlus, Code2, FileCode, ListChecks, Pencil, Play, Plus, Trash2 } from "lucide-react";
import { CommandService } from "../../../bindings/server-manager/services/command";
import type { Preset, PresetParam, Snippet } from "../../../bindings/server-manager/services/command";
import { CodeEditor } from "../../ui/CodeEditor";
import { useRemote } from "../../ui/hooks";
import { Modal } from "../Overlays";
import { confirmDialog, toast } from "../../store/ui";
import { errMsg } from "../../lib/api";
import { hasKey, t, useT } from "../../i18n";
import { useCmd, type ActionKind } from "./store";
import { DangerList } from "./util";
import { Select } from "../../ui/Select";

const UNIT_RE = /^[A-Za-z0-9][A-Za-z0-9@._:+-]{0,127}$/;

export function presetLabel(id: string): string {
  const key = `cmd.preset.${id}`;
  return hasKey(key) ? t(key) : id;
}

function paramLabel(name: string): string {
  const key = `cmd.param.${name}`;
  return hasKey(key) ? t(key) : name;
}

function optionLabel(param: string, v: string): string {
  const key = `cmd.param.${param}.${v}`;
  return hasKey(key) ? t(key) : v;
}

function paramError(p: PresetParam, v: string): string | null {
  const val = v.trim() || p.default;
  if (!val) return p.required ? t("cmd.paramRequired") : null;
  if (p.kind === "unit" && !UNIT_RE.test(val)) return t("cmd.invalidUnit");
  if (p.kind === "int") {
    const n = Number(val);
    if (!Number.isInteger(n) || n < p.min || n > p.max) return t("cmd.rangeError", { min: p.min, max: p.max });
  }
  return null;
}

/** Keeps the live analysis (script to run + danger reasons) up to date. */
function useAnalysis(presets: Preset[]) {
  const kind = useCmd((s) => s.kind);
  const command = useCmd((s) => s.command);
  const preset = useCmd((s) => s.preset);
  const params = useCmd((s) => s.params);
  useEffect(() => {
    let stale = false;
    const timer = setTimeout(async () => {
      try {
        if (kind === "preset") {
          const p = presets.find((x) => x.id === preset);
          if (!p) return;
          const errs = (p.params ?? []).map((x) => paramError(x, params[x.name] ?? "")).filter(Boolean);
          if (errs.length) {
            if (!stale) useCmd.getState().set({ analysis: { script: "", dangers: [], presetDanger: p.danger, presetSudo: p.sudo, error: errs[0]! } });
            return;
          }
          const r = await CommandService.Render(preset, params);
          const dangers = (await CommandService.Check(r.command)) ?? [];
          if (!stale) useCmd.getState().set({ analysis: { script: r.command, dangers, presetDanger: r.danger, presetSudo: r.sudo, error: "" } });
        } else {
          const dangers = command.trim() ? ((await CommandService.Check(command)) ?? []) : [];
          if (!stale) useCmd.getState().set({ analysis: { script: command, dangers, presetDanger: false, presetSudo: false, error: "" } });
        }
      } catch (e) {
        if (!stale) useCmd.getState().set({ analysis: { script: "", dangers: [], presetDanger: false, presetSudo: false, error: errMsg(e) } });
      }
    }, 250);
    return () => {
      stale = true;
      clearTimeout(timer);
    };
  }, [kind, command, preset, params, presets]);
}

export function ActionPanel({ onRun }: { onRun: () => void }) {
  const tr = useT();
  const kind = useCmd((s) => s.kind);
  const set = useCmd((s) => s.set);
  const presets = useRemote(async () => (await CommandService.Presets()) ?? [], []);
  const list = presets.data ?? [];
  useAnalysis(list);
  const analysis = useCmd((s) => s.analysis);

  const tabs: { id: ActionKind; label: string; icon: React.ReactNode }[] = [
    { id: "command", label: tr("cmd.kind.command"), icon: <Code2 size={13} /> },
    { id: "preset", label: tr("cmd.kind.preset"), icon: <ListChecks size={13} /> },
    { id: "snippets", label: tr("cmd.kind.snippets"), icon: <FileCode size={13} /> },
  ];

  return (
    <div className="panel-box cmd-action">
      <div className="box-title">
        <span>{tr("cmd.action")}</span>
        <div className="grow" />
        <div className="segmented cmd-kind">
          {tabs.map((x) => (
            <button key={x.id} className={kind === x.id ? "on" : ""} onClick={() => set({ kind: x.id })}>
              {x.icon} {x.label}
            </button>
          ))}
        </div>
      </div>
      {kind === "command" && <CommandEditor onRun={onRun} />}
      {kind === "preset" && <PresetForm presets={list} error={presets.error} />}
      {kind === "snippets" && <Snippets />}
      {kind !== "snippets" && analysis.error && <div className="cmd-hint err">{analysis.error}</div>}
      {kind !== "snippets" && <DangerList dangers={analysis.dangers} />}
    </div>
  );
}

function CommandEditor({ onRun }: { onRun: () => void }) {
  const tr = useT();
  const command = useCmd((s) => s.command);
  const snippet = useCmd((s) => s.snippet);
  const sudo = useCmd((s) => s.sudo);
  const set = useCmd((s) => s.set);
  const [saving, setSaving] = useState<Snippet | null>(null);
  return (
    <>
      <CodeEditor value={command} language="shell" height={170} onChange={(v) => set({ command: v })} onSubmit={onRun} />
      <div className="cmd-editor-foot">
        <span className="muted">{snippet ? tr("cmd.fromSnippet", { name: snippet }) : tr("cmd.editorHint")}</span>
        <div className="grow" />
        <button
          className="btn sm ghost"
          disabled={!command.trim()}
          onClick={() => setSaving({ id: "", name: snippet, command, sudo, description: "", created: 0, updated: 0 })}
        >
          <BookmarkPlus size={13} /> {tr("cmd.saveAsSnippet")}
        </button>
      </div>
      {saving && <SnippetDialog snippet={saving} onClose={() => setSaving(null)} />}
    </>
  );
}

function PresetForm({ presets, error }: { presets: Preset[]; error: string }) {
  const tr = useT();
  const presetId = useCmd((s) => s.preset);
  const params = useCmd((s) => s.params);
  const analysis = useCmd((s) => s.analysis);
  const set = useCmd((s) => s.set);
  const preset = presets.find((p) => p.id === presetId);
  const categories = useMemo(() => {
    const map = new Map<string, Preset[]>();
    for (const p of presets) map.set(p.category, [...(map.get(p.category) ?? []), p]);
    return Array.from(map.entries());
  }, [presets]);

  if (error) return <div className="cmd-hint err">{error}</div>;
  const setParam = (name: string, v: string) => set({ params: { ...params, [name]: v } });
  return (
    <div className="cmd-preset">
      <div className="form-grid">
        <div className="field">
          <label>{tr("cmd.preset")}</label>
          <Select
            value={presetId}
            onChange={(v) => set({ preset: v, params: {} })}
            options={categories.flatMap(([cat, list]) => {
              const group = hasKey(`cmd.cat.${cat}`) ? t(`cmd.cat.${cat}` as "cmd.cat.info") : cat;
              return list.map((p) => ({ value: p.id, label: presetLabel(p.id), group, hint: p.danger ? "⚠" : undefined }));
            })}
          />
        </div>
        {(preset?.params ?? []).map((p) => {
          const v = params[p.name] ?? "";
          const err = paramError(p, v);
          if (p.kind === "bool") {
            return (
              <div className="field" key={p.name}>
                <label>&nbsp;</label>
                <label className="check">
                  <input type="checkbox" checked={(v || p.default) === "true"} onChange={(e) => setParam(p.name, e.target.checked ? "true" : "false")} />
                  {paramLabel(p.name)}
                </label>
              </div>
            );
          }
          if (p.kind === "enum") {
            return (
              <div className="field" key={p.name}>
                <label>{paramLabel(p.name)}</label>
                <Select value={v || p.default} onChange={(o) => setParam(p.name, o)} options={(p.options ?? []).map((o) => ({ value: o, label: optionLabel(p.name, o) }))} />
              </div>
            );
          }
          return (
            <div className="field" key={p.name}>
              <label>
                {paramLabel(p.name)}
                {p.required ? " *" : ""}
              </label>
              <input
                className={`input ${v && err ? "invalid" : ""}`}
                type={p.kind === "int" ? "number" : "text"}
                min={p.kind === "int" ? p.min : undefined}
                max={p.kind === "int" ? p.max : undefined}
                placeholder={p.kind === "unit" ? "nginx" : p.default}
                value={v}
                spellCheck={false}
                autoCapitalize="off"
                onChange={(e) => setParam(p.name, e.target.value)}
              />
              {v && err && <span className="error">{err}</span>}
            </div>
          );
        })}
      </div>
      {preset && (
        <div className="cmd-preset-desc muted">
          {hasKey(`cmd.presetDesc.${preset.id}`) ? t(`cmd.presetDesc.${preset.id}` as "cmd.presetDesc.disk") : ""}
          {preset.sudo && <span className="badge warn">{tr("cmd.needsRoot")}</span>}
          {preset.danger && <span className="badge err">{tr("cmd.dangerous")}</span>}
        </div>
      )}
      <div className="field">
        <label>{tr("cmd.finalCommand")}</label>
        <pre className="log-view cmd-rendered">{analysis.script || "…"}</pre>
      </div>
    </div>
  );
}

function Snippets() {
  const tr = useT();
  const set = useCmd((s) => s.set);
  const version = useCmd((s) => s.historyVersion);
  const [editing, setEditing] = useState<Snippet | null>(null);
  const [q, setQ] = useState("");
  const list = useRemote(async () => (await CommandService.Snippets()) ?? [], [version]);
  const items = (list.data ?? []).filter((s) => !q.trim() || [s.name, s.description, s.command].some((v) => v.toLowerCase().includes(q.trim().toLowerCase())));

  const use = (s: Snippet) => {
    set({ kind: "command", command: s.command, snippet: s.name, sudo: s.sudo });
    toast(tr("cmd.snippetLoaded", { name: s.name }));
  };
  const remove = async (s: Snippet) => {
    if (!(await confirmDialog(tr("cmd.snippetDeleteTitle"), tr("cmd.snippetDeleteMsg", { name: s.name }), tr("common.delete"), true))) return;
    try {
      await CommandService.DeleteSnippet(s.id);
      list.reload();
    } catch (e) {
      toast(errMsg(e), "error");
    }
  };

  return (
    <div className="cmd-snippets">
      <div className="toolbar">
        <input className="input input-sm" placeholder={tr("cmd.snippetSearch")} value={q} onChange={(e) => setQ(e.target.value)} />
        <div className="grow" />
        <button className="btn sm" onClick={() => setEditing({ id: "", name: "", command: "", sudo: false, description: "", created: 0, updated: 0 })}>
          <Plus size={13} /> {tr("cmd.snippetNew")}
        </button>
      </div>
      {list.error && <div className="cmd-hint err">{list.error}</div>}
      {list.loading && !list.data && <div className="muted">{tr("common.loading")}</div>}
      {list.data && items.length === 0 && <div className="muted cmd-empty">{list.data.length === 0 ? tr("cmd.snippetEmpty") : tr("cmd.noMatch")}</div>}
      <div className="list-rows cmd-snippet-list">
        {items.map((s) => (
          <div key={s.id} className="list-row clickable" onDoubleClick={() => use(s)}>
            <div className="grow">
              <div className="cmd-snippet-name">
                {s.name} {s.sudo && <span className="badge warn">sudo</span>}
              </div>
              <div className="cmd-snippet-cmd mono">{s.description || s.command.split("\n")[0]}</div>
            </div>
            <button className="btn sm" onClick={() => use(s)} title={tr("cmd.snippetUse")}>
              <Play size={12} /> {tr("cmd.snippetUse")}
            </button>
            <button className="icon-btn" onClick={() => setEditing(s)} title={tr("common.edit")}>
              <Pencil size={13} />
            </button>
            <button className="icon-btn" onClick={() => remove(s)} title={tr("common.delete")}>
              <Trash2 size={13} />
            </button>
          </div>
        ))}
      </div>
      {editing && (
        <SnippetDialog
          snippet={editing}
          onClose={(saved) => {
            setEditing(null);
            if (saved) list.reload();
          }}
        />
      )}
    </div>
  );
}

function SnippetDialog({ snippet, onClose }: { snippet: Snippet; onClose: (saved?: boolean) => void }) {
  const tr = useT();
  const [v, setV] = useState(snippet);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const save = async () => {
    if (!v.name.trim()) return setError(tr("err.cmd.snippetName"));
    if (!v.command.trim()) return setError(tr("err.cmd.empty"));
    setBusy(true);
    try {
      const saved = await CommandService.SaveSnippet(v);
      toast(tr("cmd.snippetSaved", { name: saved.name }), "success");
      useCmd.getState().set({ historyVersion: useCmd.getState().historyVersion + 1 });
      onClose(true);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal
      title={snippet.id ? tr("cmd.snippetEdit") : tr("cmd.snippetNew")}
      size="wide"
      onClose={() => onClose()}
      footer={
        <>
          <button className="btn" onClick={() => onClose()}>
            {tr("common.cancel")}
          </button>
          <button className="btn primary" disabled={busy} onClick={save}>
            {tr("common.save")}
          </button>
        </>
      }
    >
      <div className="field">
        <label>{tr("cmd.snippetName")}</label>
        <input className="input" autoFocus value={v.name} maxLength={100} onChange={(e) => setV({ ...v, name: e.target.value })} />
      </div>
      <div className="field">
        <label>{tr("cmd.snippetDesc")}</label>
        <input className="input" value={v.description} maxLength={1000} onChange={(e) => setV({ ...v, description: e.target.value })} />
      </div>
      <div className="field">
        <label>{tr("cmd.kind.command")}</label>
        <CodeEditor value={v.command} language="shell" height={200} onChange={(c) => setV((x) => ({ ...x, command: c }))} onSubmit={save} />
      </div>
      <label className="check">
        <input type="checkbox" checked={v.sudo} onChange={(e) => setV({ ...v, sudo: e.target.checked })} />
        {tr("cmd.opt.sudo")}
      </label>
      {error && <div className="cmd-hint err">{error}</div>}
    </Modal>
  );
}
