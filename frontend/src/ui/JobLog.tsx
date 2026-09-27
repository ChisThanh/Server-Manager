import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Events } from "@wailsio/runtime";
import { AppService } from "../../bindings/server-manager/services";
import type { JobInfo } from "../../bindings/server-manager/internal/core";

const enc = new TextEncoder();
const dec = new TextDecoder();

/**
 * Live output of a backend job (deploy, compose up, backup…), rendered with
 * xterm so ANSI colors and progress bars look right. Output written before
 * the component mounted is fetched first; events are deduplicated by byte
 * offset so nothing is shown twice or lost.
 */
export function JobLog({ jobId, height = 320, onDone }: { jobId: string; height?: number | string; onDone?: (info: JobInfo) => void }) {
  const host = useRef<HTMLDivElement>(null);
  const onDoneRef = useRef(onDone);
  onDoneRef.current = onDone;

  useEffect(() => {
    const term = new Terminal({
      convertEol: true,
      disableStdin: true,
      cursorBlink: false,
      cursorStyle: "bar",
      cursorInactiveStyle: "none",
      fontFamily: "'SF Mono', Menlo, Monaco, Consolas, monospace",
      fontSize: 12,
      scrollback: 20000,
      theme: { background: "#121214", foreground: "#d4d4d8", cursor: "#121214" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current!);
    try {
      fit.fit();
    } catch {
      /* hidden */
    }
    const ro = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        /* hidden */
      }
    });
    ro.observe(host.current!);

    let have = -1; // bytes shown; -1 until the snapshot arrives
    const early: { offset: number; data: string }[] = [];
    let finished = false;
    const show = (offset: number, data: string) => {
      const bytes = enc.encode(data);
      const end = offset + bytes.length;
      if (end <= have) return;
      term.write(offset >= have ? data : dec.decode(bytes.slice(have - offset)));
      have = end;
    };
    const offOut = Events.On("job:output", (ev) => {
      if (ev.data.id !== jobId) return;
      if (have < 0) early.push({ offset: ev.data.offset, data: ev.data.data });
      else show(ev.data.offset, ev.data.data);
    });
    const offDone = Events.On("job:done", (ev) => {
      if (ev.data.id !== jobId || finished) return;
      finished = true;
      // Fetch once more so the tail is complete, then report.
      AppService.Job(jobId).then(([info]) => {
        if (info && have >= 0 && info.written > have) {
          const tail = enc.encode(info.log);
          const missing = info.written - have;
          term.write(dec.decode(tail.slice(Math.max(0, tail.length - missing))));
          have = info.written;
        }
        onDoneRef.current?.(info ?? ev.data);
      });
    });
    AppService.Job(jobId).then(([info, ok]) => {
      if (!ok) {
        term.write("\x1b[90m(job not found)\x1b[0m\n");
        return;
      }
      term.write(info.log);
      have = info.written;
      for (const e of early) show(e.offset, e.data);
      early.length = 0;
      if (info.state !== "running" && !finished) {
        finished = true;
        onDoneRef.current?.(info);
      }
    });
    return () => {
      offOut();
      offDone();
      ro.disconnect();
      term.dispose();
    };
  }, [jobId]);

  return <div className="job-log" style={{ height }} ref={host} />;
}
