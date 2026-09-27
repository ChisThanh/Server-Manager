import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import {
  AlertCircle,
  Archive,
  ArrowUp,
  ChevronDown,
  ChevronRight,
  ChevronsDownUp,
  Clipboard,
  Copy,
  Download,
  Eye,
  EyeOff,
  FilePlus,
  FolderInput,
  FolderPlus,
  Home,
  Info,
  Lock,
  PackageOpen,
  Pencil,
  RefreshCw,
  Scissors,
  Search,
  ShieldAlert,
  SquareTerminal,
  Trash2,
  Upload,
} from "lucide-react";
import { FileService, errCode, errMsg, type FileEntry } from "../../lib/api";
import { basename, dirname, formatBytes, formatDate, isArchive, joinPath, validName } from "../../lib/format";
import { useT } from "../../i18n";
import { useApp } from "../../store/app";
import { forgetPath, openFile, renamePath } from "../../store/editor";
import { withSudo, withSudoFallback } from "../../store/sudo";
import { confirmDialog, promptDialog, toast, useUI, type MenuItem } from "../../store/ui";
import { FileIcon } from "./icons";
import { PropertiesDialog } from "./PropertiesDialog";
import { SearchPanel } from "./SearchPanel";

const ROW_H = 24;
const DND_TYPE = "application/x-sm-paths";

interface DirState {
  entries?: FileEntry[];
  loading?: boolean;
  error?: string;
  errorCode?: string;
  /** Listed through sudo; children are listed through sudo too. */
  sudo?: boolean;
}

type Row =
  | { kind: "entry"; entry: FileEntry; depth: number }
  | { kind: "loading"; depth: number; key: string }
  | { kind: "empty"; depth: number; key: string }
  | { kind: "error"; depth: number; key: string; dir: string; text: string; denied: boolean };

function loadPref(k: string, d: boolean) {
  try {
    const v = localStorage.getItem(k);
    return v === null ? d : v === "1";
  } catch {
    return d;
  }
}

export function FileExplorer(props: {
  connId: string;
  root: string;
  onRootChange: (p: string) => void;
  home: string;
  onOpenTerminal: (cwd: string) => void;
  connected: boolean;
}) {
  const t = useT();
  const { connId, root } = props;
  const [dirs, setDirs] = useState<Record<string, DirState>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [focused, setFocused] = useState<string | null>(null);
  const [anchor, setAnchor] = useState<string | null>(null);
  const [showHidden, setShowHidden] = useState(() => loadPref("sm.showHidden", true));
  const [clip, setClip] = useState<{ mode: "copy" | "cut"; paths: string[] } | null>(null);
  const [mode, setMode] = useState<"tree" | "search">("tree");
  const [pathInput, setPathInput] = useState(root);
  const [propsEntry, setPropsEntry] = useState<FileEntry | null>(null);
  const [dropOver, setDropOver] = useState<string | null>(null);
  const treeRef = useRef<HTMLDivElement>(null);
  const [scroll, setScroll] = useState({ top: 0, height: 600 });
  const dirsRef = useRef(dirs);
  dirsRef.current = dirs;
  const showMenu = useUI((s) => s.showMenu);

  useEffect(() => setPathInput(root), [root]);
  useEffect(() => {
    try {
      localStorage.setItem("sm.showHidden", showHidden ? "1" : "0");
    } catch {
      /* ignore */
    }
  }, [showHidden]);

  /**
   * Lists a directory. Folders under a sudo-listed folder are listed with
   * sudo as well, so browsing a root-only tree keeps working.
   */
  const load = useCallback(
    async (dir: string, sudo?: boolean) => {
      const useSudo = sudo ?? (!!dirsRef.current[dir]?.sudo || (dir !== "/" && !!dirsRef.current[dirname(dir)]?.sudo));
      setDirs((d) => ({ ...d, [dir]: { ...d[dir], loading: true, error: undefined, errorCode: undefined } }));
      try {
        const entries = useSudo
          ? ((await withSudo(connId, (pw) => FileService.ListDirSudo(connId, dir, pw))) ?? [])
          : ((await FileService.ListDir(connId, dir)) ?? []);
        setDirs((d) => ({ ...d, [dir]: { entries, sudo: useSudo } }));
      } catch (e) {
        setDirs((d) => ({ ...d, [dir]: { error: errMsg(e), errorCode: errCode(e), sudo: useSudo } }));
      }
    },
    [connId],
  );

  const refresh = useCallback(
    (dir?: string) => {
      if (dir) {
        if (dirsRef.current[dir]) load(dir);
        return;
      }
      for (const d of Object.keys(dirsRef.current)) {
        if (d === root || expanded.has(d)) load(d);
      }
    },
    [load, root, expanded],
  );

  useEffect(() => {
    if (!props.connected) return;
    setSelected(new Set());
    setFocused(null);
    load(root);
  }, [root, props.connected, load]);

  // Reload a directory once an upload into it finishes.
  useEffect(
    () =>
      useApp.subscribe((s, prev) => {
        for (const tr of Object.values(s.transfers)) {
          if (tr.connId === connId && tr.kind === "upload" && tr.state === "done" && prev.transfers[tr.id]?.state !== "done") {
            refresh(tr.target);
          }
        }
      }),
    [connId, refresh],
  );

  const rows = useMemo(() => {
    const out: Row[] = [];
    const walk = (dir: string, depth: number) => {
      const st = dirs[dir];
      if (!st || (st.loading && !st.entries)) {
        out.push({ kind: "loading", depth, key: dir + ":loading" });
        return;
      }
      if (st.error) {
        out.push({ kind: "error", depth, key: dir + ":err", dir, text: st.error, denied: st.errorCode === "fs.permission" && !st.sudo });
        return;
      }
      const list = (st.entries ?? []).filter((e) => showHidden || !e.name.startsWith("."));
      if (list.length === 0) out.push({ kind: "empty", depth, key: dir + ":empty" });
      for (const e of list) {
        out.push({ kind: "entry", entry: e, depth });
        if (e.isDir && expanded.has(e.path)) walk(e.path, depth + 1);
      }
    };
    walk(root, 0);
    return out;
  }, [dirs, expanded, root, showHidden]);

  const entryRows = useMemo(() => rows.filter((r): r is Extract<Row, { kind: "entry" }> => r.kind === "entry"), [rows]);
  const byPath = useMemo(() => new Map(entryRows.map((r) => [r.entry.path, r.entry])), [entryRows]);

  useLayoutEffect(() => {
    const el = treeRef.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setScroll((s) => ({ ...s, height: el.clientHeight })));
    ro.observe(el);
    return () => ro.disconnect();
  }, [mode]);

  const toggle = (dir: string, force?: boolean) => {
    const open = force ?? !expanded.has(dir);
    setExpanded((s) => {
      const n = new Set(s);
      if (open) n.add(dir);
      else n.delete(dir);
      return n;
    });
    const st = dirsRef.current[dir];
    if (open && !st?.entries && !st?.loading) load(dir);
  };

  const selectedEntries = (): FileEntry[] =>
    Array.from(selected)
      .map((p) => byPath.get(p))
      .filter((e): e is FileEntry => !!e);

  const targetDir = (e?: FileEntry | null): string => {
    const x = e ?? (focused ? byPath.get(focused) : undefined);
    if (!x) return root;
    return x.isDir ? x.path : dirname(x.path);
  };

  const refreshParents = (paths: string[]) => {
    new Set(paths.map(dirname)).forEach((d) => refresh(d));
  };

  /** Runs a file operation, offering sudo when it fails for lack of permission. */
  const run = async (op: () => Promise<unknown>, sudoOp: (pw: string) => Promise<unknown>) => {
    try {
      return await withSudoFallback(connId, op, sudoOp);
    } catch (e) {
      toast(errMsg(e), "error");
      return false;
    }
  };

  // ---------- actions ----------
  const newFile = async (dir: string) => {
    const name = await promptDialog(t("fx.newFile"), t("fx.createIn", { dir }), "", { validate: validName, placeholder: "index.js" });
    if (!name) return;
    const p = joinPath(dir, name.trim());
    const ok = await run(
      () => FileService.CreateFile(connId, p),
      (pw) => FileService.SudoOp(connId, "touch", [p], "", 0, false, pw),
    );
    if (!ok) return;
    if (dir !== root) toggle(dir, true);
    await load(dir);
    setSelected(new Set([p]));
    setFocused(p);
    openFile(connId, p);
  };

  const newFolder = async (dir: string) => {
    const name = await promptDialog(t("fx.newFolder"), t("fx.createIn", { dir }), "", { validate: validName });
    if (!name) return;
    const p = joinPath(dir, name.trim());
    const ok = await run(
      () => FileService.CreateDir(connId, p),
      (pw) => FileService.SudoOp(connId, "mkdir", [p], "", 0, false, pw),
    );
    if (!ok) return;
    if (dir !== root) toggle(dir, true);
    await load(dir);
    setSelected(new Set([p]));
    setFocused(p);
  };

  const rename = async (e: FileEntry) => {
    const name = await promptDialog(t("fx.rename"), t("fx.newName"), e.name, { validate: validName, confirmText: t("fx.rename") });
    if (!name || name === e.name) return;
    const to = joinPath(dirname(e.path), name.trim());
    const ok = await run(
      () => FileService.Rename(connId, e.path, to),
      (pw) => FileService.SudoOp(connId, "rename", [e.path], to, 0, false, pw),
    );
    if (!ok) return;
    renamePath(connId, e.path, to);
    if (expanded.has(e.path)) {
      setExpanded((s) => {
        const n = new Set(s);
        n.delete(e.path);
        n.add(to);
        return n;
      });
    }
    await load(dirname(e.path));
    setSelected(new Set([to]));
    setFocused(to);
  };

  const remove = async (entries: FileEntry[]) => {
    if (entries.length === 0) return;
    const hasDir = entries.some((e) => e.isDir && !e.isLink);
    const msg =
      entries.length === 1
        ? t(hasDir ? "fx.deleteOneDir" : "fx.deleteOne", { path: entries[0].path })
        : t(hasDir ? "fx.deleteManyDir" : "fx.deleteMany", { n: entries.length });
    if (!(await confirmDialog(t("common.delete"), `${msg}\n\n${t("fx.irreversible")}`, t("common.delete"), true))) return;
    const paths = entries.map((e) => e.path);
    const ok = await run(
      () => FileService.Delete(connId, paths),
      (pw) => FileService.SudoOp(connId, "delete", paths, "", 0, false, pw),
    );
    refreshParents(paths);
    if (!ok) return;
    paths.forEach((p) => forgetPath(connId, p));
    setSelected(new Set());
    toast(t("fx.deleted", { n: entries.length }), "success");
  };

  const transferInto = async (paths: string[], dir: string, copy: boolean) => {
    const ok = await run(
      () => (copy ? FileService.Copy(connId, paths, dir) : FileService.Move(connId, paths, dir)),
      (pw) => FileService.SudoOp(connId, copy ? "copy" : "move", paths, dir, 0, false, pw),
    );
    if (!ok) return false;
    if (!copy) {
      paths.forEach((p) => renamePath(connId, p, joinPath(dir, basename(p))));
      refreshParents(paths);
    }
    if (dir !== root) toggle(dir, true);
    await load(dir);
    return true;
  };

  const paste = async (dir: string) => {
    if (!clip) return;
    if ((await transferInto(clip.paths, dir, clip.mode === "copy")) && clip.mode === "cut") setClip(null);
  };

  const moveInto = (paths: string[], dir: string, copy: boolean) => {
    const movable = paths.filter((p) => dirname(p) !== dir || copy);
    if (movable.length > 0) transferInto(movable, dir, copy);
  };

  const upload = async (dir: string, folders: boolean) => {
    try {
      await FileService.PickAndUpload(connId, dir, folders, t("fx.pickUpload"));
    } catch (err) {
      toast(errMsg(err), "error");
    }
  };

  const download = async (e: FileEntry) => {
    try {
      await FileService.PickAndDownload(connId, e.path, e.isDir, t("fx.pickSaveDir"));
    } catch (err) {
      toast(errMsg(err), "error");
    }
  };

  const compress = async (entries: FileEntry[]) => {
    if (entries.length === 0) return;
    const dir = dirname(entries[0].path);
    if (entries.some((e) => dirname(e.path) !== dir)) {
      toast(t("fx.onlySameDir"), "error");
      return;
    }
    const def = (entries.length === 1 ? entries[0].name : basename(dir) || "archive") + ".tar.gz";
    const name = await promptDialog(t("fx.compress"), t("fx.archiveName"), def, { validate: validName });
    if (!name) return;
    try {
      toast(t("fx.compressing"));
      await FileService.Compress(connId, dir, entries.map((e) => e.name), name.trim());
      await load(dir);
      toast(t("fx.created", { name }), "success");
    } catch (err) {
      toast(errMsg(err), "error");
    }
  };

  const extract = async (e: FileEntry) => {
    try {
      toast(t("fx.extracting"));
      await FileService.Extract(connId, e.path);
      await load(dirname(e.path));
      toast(t("fx.extracted", { name: e.name }), "success");
    } catch (err) {
      toast(errMsg(err), "error");
    }
  };

  const copyText = (s: string) => {
    navigator.clipboard.writeText(s);
    toast(t("fx.copiedPath"));
  };

  const activate = (e: FileEntry) => {
    if (e.isDir) toggle(e.path);
    else openFile(connId, e.path);
  };

  // ---------- selection ----------
  const onRowClick = (ev: React.MouseEvent, e: FileEntry) => {
    const multi = ev.metaKey || ev.ctrlKey;
    if (ev.shiftKey && anchor) {
      const a = entryRows.findIndex((r) => r.entry.path === anchor);
      const b = entryRows.findIndex((r) => r.entry.path === e.path);
      if (a >= 0 && b >= 0) {
        const [lo, hi] = a < b ? [a, b] : [b, a];
        setSelected(new Set(entryRows.slice(lo, hi + 1).map((r) => r.entry.path)));
        setFocused(e.path);
        return;
      }
    }
    if (multi) {
      setSelected((s) => {
        const n = new Set(s);
        if (n.has(e.path)) n.delete(e.path);
        else n.add(e.path);
        return n;
      });
    } else {
      setSelected(new Set([e.path]));
      activate(e);
    }
    setFocused(e.path);
    setAnchor(e.path);
  };

  const rowMenu = (ev: React.MouseEvent, e: FileEntry | null) => {
    ev.preventDefault();
    ev.stopPropagation();
    let sel = selectedEntries();
    if (e && !selected.has(e.path)) {
      setSelected(new Set([e.path]));
      sel = [e];
    }
    if (e) setFocused(e.path);
    const dir = e ? (e.isDir ? e.path : dirname(e.path)) : root;
    const many = sel.length > 1;
    const items: MenuItem[] = [];
    if (e && !many) {
      if (!e.isDir) items.push({ label: t("fx.open"), icon: <Pencil />, onClick: () => openFile(connId, e.path) });
      if (e.isDir) items.push({ label: t("fx.openAsRoot"), icon: <FolderInput />, onClick: () => props.onRootChange(e.path) });
      items.push({ label: t("fx.openTerminal"), icon: <SquareTerminal />, onClick: () => props.onOpenTerminal(dir) });
      items.push({ separator: true });
    }
    items.push(
      { label: t("fx.newFileMenu"), icon: <FilePlus />, onClick: () => newFile(dir) },
      { label: t("fx.newFolderMenu"), icon: <FolderPlus />, onClick: () => newFolder(dir) },
      { label: t("fx.uploadFiles"), icon: <Upload />, onClick: () => upload(dir, false) },
      { label: t("fx.uploadFolder"), icon: <Upload />, onClick: () => upload(dir, true) },
    );
    if (e) {
      items.push({ separator: true });
      if (!many) items.push({ label: t("fx.download"), icon: <Download />, onClick: () => download(e) });
      items.push(
        { label: t("fx.copy"), icon: <Copy />, shortcut: "⌘C", onClick: () => setClip({ mode: "copy", paths: sel.map((x) => x.path) }) },
        { label: t("fx.cut"), icon: <Scissors />, shortcut: "⌘X", onClick: () => setClip({ mode: "cut", paths: sel.map((x) => x.path) }) },
      );
    }
    items.push({
      label: clip ? t("fx.pasteN", { n: clip.paths.length }) : t("fx.paste"),
      icon: <Clipboard />,
      shortcut: "⌘V",
      disabled: !clip,
      onClick: () => paste(dir),
    });
    if (e) {
      items.push({ separator: true });
      if (!many) items.push({ label: t("fx.renameMenu"), icon: <Pencil />, shortcut: "F2", onClick: () => rename(e) });
      items.push({ label: t("fx.compressMenu"), icon: <Archive />, onClick: () => compress(sel) });
      if (!many && !e.isDir && isArchive(e.name)) items.push({ label: t("fx.extractHere"), icon: <PackageOpen />, onClick: () => extract(e) });
      if (!many) {
        items.push({ label: t("fx.copyPath"), icon: <Copy />, onClick: () => copyText(e.path) });
        items.push({ label: t("fx.properties"), icon: <Info />, onClick: () => setPropsEntry(e) });
      }
      items.push({ separator: true });
      items.push({ label: many ? t("fx.deleteN", { n: sel.length }) : t("common.delete"), icon: <Trash2 />, danger: true, shortcut: "⌫", onClick: () => remove(sel) });
    } else {
      items.push({ separator: true }, { label: t("common.refresh"), icon: <RefreshCw />, onClick: () => refresh() });
    }
    showMenu(ev.clientX, ev.clientY, items);
  };

  const onKeyDown = (ev: React.KeyboardEvent) => {
    const idx = focused ? entryRows.findIndex((r) => r.entry.path === focused) : -1;
    const cur = idx >= 0 ? entryRows[idx].entry : undefined;
    const mod = ev.metaKey || ev.ctrlKey;
    const moveTo = (i: number) => {
      const r = entryRows[Math.max(0, Math.min(entryRows.length - 1, i))];
      if (!r) return;
      setFocused(r.entry.path);
      setAnchor(r.entry.path);
      setSelected(new Set([r.entry.path]));
      const el = treeRef.current;
      const rowIdx = rows.indexOf(r);
      if (el) {
        const top = rowIdx * ROW_H;
        if (top < el.scrollTop) el.scrollTop = top;
        else if (top + ROW_H > el.scrollTop + el.clientHeight) el.scrollTop = top + ROW_H - el.clientHeight;
      }
    };
    switch (ev.key) {
      case "ArrowDown":
        moveTo(idx + 1);
        break;
      case "ArrowUp":
        moveTo(idx - 1);
        break;
      case "ArrowRight":
        if (cur?.isDir) toggle(cur.path, true);
        break;
      case "ArrowLeft":
        if (cur?.isDir && expanded.has(cur.path)) toggle(cur.path, false);
        else if (cur) {
          const parent = dirname(cur.path);
          if (parent !== root && byPath.has(parent)) moveTo(entryRows.findIndex((r) => r.entry.path === parent));
        }
        break;
      case "Enter":
        if (cur) activate(cur);
        break;
      case "F2":
        if (cur) rename(cur);
        break;
      case "Delete":
      case "Backspace":
        if (ev.key === "Delete" || mod) remove(selectedEntries());
        break;
      case "c":
        if (mod && selected.size) setClip({ mode: "copy", paths: Array.from(selected) });
        break;
      case "x":
        if (mod && selected.size) setClip({ mode: "cut", paths: Array.from(selected) });
        break;
      case "v":
        if (mod) paste(targetDir());
        break;
      case "a":
        if (mod) setSelected(new Set(entryRows.map((r) => r.entry.path)));
        break;
      default:
        return;
    }
    ev.preventDefault();
  };

  // ---------- internal drag & drop (move; hold ⌥/Ctrl to copy) ----------
  const onDragStart = (ev: React.DragEvent, e: FileEntry) => {
    const paths = selected.has(e.path) ? Array.from(selected) : [e.path];
    ev.dataTransfer.setData(DND_TYPE, JSON.stringify(paths));
    ev.dataTransfer.effectAllowed = "copyMove";
  };
  const onDragOver = (ev: React.DragEvent, dir: string) => {
    if (!ev.dataTransfer.types.includes(DND_TYPE)) return;
    ev.preventDefault();
    ev.stopPropagation();
    ev.dataTransfer.dropEffect = ev.altKey || ev.ctrlKey ? "copy" : "move";
    setDropOver(dir);
  };
  const onDrop = (ev: React.DragEvent, dir: string) => {
    const raw = ev.dataTransfer.getData(DND_TYPE);
    setDropOver(null);
    if (!raw) return;
    ev.preventDefault();
    ev.stopPropagation();
    const paths: string[] = JSON.parse(raw);
    if (paths.some((p) => dir === p || dir.startsWith(p + "/"))) return;
    moveInto(paths, dir, ev.altKey || ev.ctrlKey);
  };

  // ---------- render ----------
  const start = Math.max(0, Math.floor(scroll.top / ROW_H) - 10);
  const end = Math.min(rows.length, Math.ceil((scroll.top + scroll.height) / ROW_H) + 10);
  const focusEntry = focused ? byPath.get(focused) : undefined;
  const cutSet = clip?.mode === "cut" ? new Set(clip.paths) : null;

  const goPath = async (p: string) => {
    const target = p.trim() || "/";
    try {
      const st = await FileService.Stat(connId, target);
      if (st.isDir) props.onRootChange(st.path);
      else {
        props.onRootChange(dirname(st.path));
        openFile(connId, st.path);
      }
    } catch (e) {
      if (errCode(e) === "fs.permission") {
        props.onRootChange(target);
        return;
      }
      toast(errMsg(e), "error");
      setPathInput(root);
    }
  };

  const msgStyle = (top: number, depth: number) => ({ position: "absolute" as const, top, left: 0, right: 0, height: ROW_H, paddingLeft: 10 + depth * 14 });

  return (
    <div className="explorer-inner" style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0 }}>
      <div className="explorer-head">
        <span className="title" title={root}>
          {mode === "search" ? t("fx.searchTitle") : basename(root) || "/"}
        </span>
        {mode === "tree" && (
          <>
            <button className="icon-btn" title={t("fx.newFile")} onClick={() => newFile(targetDir())}>
              <FilePlus size={15} />
            </button>
            <button className="icon-btn" title={t("fx.newFolder")} onClick={() => newFolder(targetDir())}>
              <FolderPlus size={15} />
            </button>
            <button
              className="icon-btn"
              title={t("fx.upload")}
              onClick={(e) =>
                showMenu(e.clientX, e.clientY, [
                  { label: t("fx.uploadFiles"), icon: <Upload />, onClick: () => upload(targetDir(), false) },
                  { label: t("fx.uploadFolder"), icon: <Upload />, onClick: () => upload(targetDir(), true) },
                ])
              }
            >
              <Upload size={15} />
            </button>
            <button className="icon-btn" title={t("common.refresh")} onClick={() => refresh()}>
              <RefreshCw size={14} />
            </button>
            <button className="icon-btn" title={t("fx.collapseAll")} onClick={() => setExpanded(new Set())}>
              <ChevronsDownUp size={15} />
            </button>
            <button className={`icon-btn ${showHidden ? "active" : ""}`} title={showHidden ? t("fx.hideHidden") : t("fx.showHidden")} onClick={() => setShowHidden((v) => !v)}>
              {showHidden ? <Eye size={15} /> : <EyeOff size={15} />}
            </button>
          </>
        )}
        <button className={`icon-btn ${mode === "search" ? "active" : ""}`} title={t("fx.search")} onClick={() => setMode((m) => (m === "tree" ? "search" : "tree"))}>
          <Search size={15} />
        </button>
      </div>

      <div className="pathbar">
        <button className="icon-btn" title={t("fx.parent")} disabled={root === "/"} onClick={() => props.onRootChange(dirname(root))}>
          <ArrowUp size={15} />
        </button>
        <button className="icon-btn" title={t("fx.home")} onClick={() => props.onRootChange(props.home)}>
          <Home size={14} />
        </button>
        <input
          className="input"
          value={pathInput}
          onChange={(e) => setPathInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") goPath(pathInput);
            if (e.key === "Escape") setPathInput(root);
          }}
          spellCheck={false}
          title={t("fx.pathTip")}
        />
      </div>

      {mode === "search" ? (
        <SearchPanel connId={connId} root={root} />
      ) : (
        <div
          ref={treeRef}
          className={`tree ${dropOver === root ? "file-drop-target-active" : ""}`}
          tabIndex={0}
          onKeyDown={onKeyDown}
          onScroll={(e) => setScroll({ top: e.currentTarget.scrollTop, height: e.currentTarget.clientHeight })}
          onContextMenu={(e) => rowMenu(e, null)}
          onClick={(e) => {
            if (e.target === e.currentTarget) {
              setSelected(new Set());
              setFocused(null);
            }
          }}
          onDragOver={(e) => onDragOver(e, root)}
          onDragLeave={() => setDropOver(null)}
          onDrop={(e) => onDrop(e, root)}
          data-file-drop-target
          data-remote-dir={root}
          data-conn-id={connId}
        >
          <div style={{ height: rows.length * ROW_H, position: "relative" }}>
            {rows.slice(start, end).map((r, i) => {
              const top = (start + i) * ROW_H;
              if (r.kind === "loading" || r.kind === "empty") {
                return (
                  <div key={r.key} className="tree-msg" style={msgStyle(top, r.depth)}>
                    {r.kind === "loading" ? <span className="spinner" style={{ width: 11, height: 11 }} /> : null}
                    <span className="txt">{r.kind === "loading" ? t("common.loading") : t("fx.empty")}</span>
                  </div>
                );
              }
              if (r.kind === "error") {
                return (
                  <div key={r.key} className="tree-msg err" style={msgStyle(top, r.depth)} title={r.text}>
                    {r.denied ? <Lock size={12} /> : <AlertCircle size={12} />}
                    <span className="txt">{r.denied ? t("fx.permDenied") : r.text}</span>
                    {r.denied ? (
                      <button className="link-btn" onClick={() => load(r.dir, true)}>
                        {t("fx.openSudo")}
                      </button>
                    ) : (
                      <button className="link-btn" onClick={() => load(r.dir)}>
                        {t("common.retry")}
                      </button>
                    )}
                  </div>
                );
              }
              const e = r.entry;
              const isOpen = e.isDir && expanded.has(e.path);
              const dropDir = e.isDir ? e.path : dirname(e.path);
              const dst = e.isDir ? dirs[e.path] : undefined;
              return (
                <div
                  key={e.path}
                  className={`tree-row ${selected.has(e.path) ? "selected" : ""} ${focused === e.path ? "focused" : ""} ${dropOver === e.path && e.isDir ? "drop-over" : ""} ${cutSet?.has(e.path) ? "cut" : ""}`}
                  style={{ position: "absolute", top, left: 0, right: 0, paddingLeft: 6 + r.depth * 14 }}
                  onClick={(ev) => onRowClick(ev, e)}
                  onDoubleClick={() => !e.isDir && openFile(connId, e.path)}
                  onContextMenu={(ev) => rowMenu(ev, e)}
                  draggable
                  onDragStart={(ev) => onDragStart(ev, e)}
                  onDragOver={(ev) => onDragOver(ev, dropDir)}
                  onDrop={(ev) => onDrop(ev, dropDir)}
                  data-file-drop-target
                  data-remote-dir={dropDir}
                  data-conn-id={connId}
                  title={`${e.path}\n${e.mode}  ${e.owner}:${e.group}${e.isDir ? "" : "  " + formatBytes(e.size)}\n${formatDate(e.modTime)}${e.isLink ? "\n→ " + e.linkTarget : ""}`}
                >
                  <span
                    className="chev"
                    onClick={(ev) => {
                      if (e.isDir) {
                        ev.stopPropagation();
                        toggle(e.path);
                      }
                    }}
                  >
                    {e.isDir && (isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />)}
                  </span>
                  <span className="ficon">
                    <FileIcon name={e.name} isDir={e.isDir} open={isOpen} />
                  </span>
                  <span className={`fname ${e.name.startsWith(".") ? "hidden-file" : ""}`}>{e.name}</span>
                  {e.isLink && <span className="link">↗</span>}
                  {dst?.sudo && dst.entries && (
                    <span className="sudo-mark" title={t("fx.sudoBadge")}>
                      <ShieldAlert size={12} />
                    </span>
                  )}
                  {dst?.loading && <span className="spinner spin" style={{ width: 11, height: 11 }} />}
                </div>
              );
            })}
          </div>
        </div>
      )}

      <div className="explorer-foot">
        {selected.size > 1
          ? t("fx.selectedN", { n: selected.size })
          : focusEntry
            ? `${focusEntry.mode} · ${focusEntry.owner}:${focusEntry.group}${focusEntry.isDir ? "" : " · " + formatBytes(focusEntry.size)} · ${formatDate(focusEntry.modTime)}`
            : clip
              ? t(clip.mode === "copy" ? "fx.clipCopied" : "fx.clipCut", { n: clip.paths.length })
              : t("fx.items", { n: dirs[root]?.entries?.length ?? 0 })}
      </div>

      {propsEntry && <PropertiesDialog connId={connId} entry={propsEntry} onClose={() => setPropsEntry(null)} onDone={() => refresh(dirname(propsEntry.path))} />}
    </div>
  );
}
