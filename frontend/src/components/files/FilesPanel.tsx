import { useContext, useState } from "react";
import type { PanelProps } from "../../ui/types";
import type { TermRequest } from "../terminal/TerminalPanel";
import { WsContext } from "../builtinPanels";
import { FileExplorer } from "./FileExplorer";
import { EditorArea } from "./EditorArea";

// Loaded on first use (it brings in the Monaco editor).
export default function FilesPanel(p: PanelProps) {
  const ws = useContext(WsContext);
  const [w, setW] = useState(320);
  const [dragging, setDragging] = useState(false);
  const startResize = (e: React.MouseEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = w;
    setDragging(true);
    const move = (ev: MouseEvent) => setW(Math.max(180, Math.min(window.innerWidth * 0.6, startW + ev.clientX - startX)));
    const up = () => {
      setDragging(false);
      window.removeEventListener("mousemove", move);
      window.removeEventListener("mouseup", up);
    };
    window.addEventListener("mousemove", move);
    window.addEventListener("mouseup", up);
  };
  return (
    <>
      <div className="explorer" style={{ width: w }}>
        {ws.root !== null && (
          <FileExplorer
            connId={p.connId}
            root={ws.root}
            onRootChange={ws.setRoot}
            home={ws.home}
            onOpenTerminal={(cwd) => p.navigate("terminal", { cwd, nonce: Date.now() } satisfies TermRequest)}
            connected={p.connected}
          />
        )}
      </div>
      <div className={`resizer ${dragging ? "dragging" : ""}`} onMouseDown={startResize} />
      <EditorArea connId={p.connId} visible={p.visible} />
    </>
  );
}

