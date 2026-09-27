import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { Pause, Play, RotateCcw } from "lucide-react";
import { TerminalService, errMsg } from "../../lib/api";
import { Modal } from "../Overlays";
import { useT } from "../../i18n";
import { Select } from "../../ui/Select";

type Ev = [number, string, string];

/** Replays an asciicast v2 recording (output events; long pauses skipped). */
export function RecordingPlayer({ file, onClose }: { file: string; onClose: () => void }) {
  const t = useT();
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const events = useRef<Ev[]>([]);
  const pos = useRef(0);
  const clock = useRef(0);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(2);
  const [progress, setProgress] = useState(0);
  const [duration, setDuration] = useState(0);
  const [error, setError] = useState("");

  useEffect(() => {
    const tm = new Terminal({ fontSize: 12, disableStdin: true, scrollback: 5000, convertEol: false, theme: { background: "#121214" }, fontFamily: "'SF Mono', Menlo, monospace" });
    tm.open(host.current!);
    term.current = tm;
    TerminalService.ReadRecording(file)
      .then((txt) => {
        const lines = txt.split("\n").filter(Boolean);
        const hdr = JSON.parse(lines[0]);
        if (hdr.width && hdr.height) tm.resize(Math.min(hdr.width, 240), Math.min(hdr.height, 80));
        const evs: Ev[] = [];
        let shift = 0;
        let last = 0;
        for (const l of lines.slice(1)) {
          try {
            const e = JSON.parse(l) as Ev;
            // Compress idle gaps longer than 2 s.
            if (e[0] - last > 2) shift += e[0] - last - 2;
            last = e[0];
            evs.push([e[0] - shift, e[1], e[2]]);
          } catch {
            /* skip */
          }
        }
        events.current = evs;
        setDuration(evs.length ? evs[evs.length - 1][0] : 0);
        setPlaying(true);
      })
      .catch((e) => setError(errMsg(e)));
    return () => tm.dispose();
  }, [file]);

  useEffect(() => {
    if (!playing) return;
    let raf = 0;
    let prev = performance.now();
    const tick = (now: number) => {
      clock.current += ((now - prev) / 1000) * speed;
      prev = now;
      const evs = events.current;
      let out = "";
      while (pos.current < evs.length && evs[pos.current][0] <= clock.current) {
        const e = evs[pos.current++];
        if (e[1] === "o") out += e[2];
        else if (e[1] === "r") {
          const [c, r] = e[2].split("x").map(Number);
          if (c && r) term.current?.resize(Math.min(c, 240), Math.min(r, 80));
        }
      }
      if (out) term.current?.write(out);
      setProgress(clock.current);
      if (pos.current >= evs.length) {
        setPlaying(false);
        return;
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [playing, speed]);

  const restart = () => {
    term.current?.reset();
    pos.current = 0;
    clock.current = 0;
    setProgress(0);
    setPlaying(true);
  };

  return (
    <Modal
      title={t("term.replayTitle")}
      size="xwide"
      onClose={onClose}
      footer={
        <>
          <div className="left" style={{ display: "flex", alignItems: "center", gap: 6 }}>
            <button className="icon-btn" onClick={() => setPlaying((p) => !p)} title={playing ? t("term.pause") : t("term.play")}>
              {playing ? <Pause size={15} /> : <Play size={15} />}
            </button>
            <button className="icon-btn" onClick={restart} title={t("term.restart")}>
              <RotateCcw size={14} />
            </button>
            <span className="muted mono" style={{ fontSize: 12 }}>
              {progress.toFixed(0)}s / {duration.toFixed(0)}s
            </span>
            <Select size="sm" value={speed} onChange={setSpeed} title={t("term.speed")} options={[1, 2, 4, 8, 16].map((s) => ({ value: s, label: `${s}×` }))} />
          </div>
          <button className="btn" onClick={onClose}>
            {t("common.close")}
          </button>
        </>
      }
    >
      {error ? <div className="err">{error}</div> : <div ref={host} className="job-log" style={{ height: "60vh", overflow: "auto" }} />}
      <div className="hint">{t("term.recordingHint")}</div>
    </Modal>
  );
}
