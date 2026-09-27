import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

export interface ChartSeries {
  label: string;
  color: string;
  /** Fill the area under the line. */
  fill?: boolean;
  /** Dashed line (e.g. a "max" series next to an average). */
  dash?: boolean;
  width?: number;
}

export interface TimeChartProps {
  /** [timestamps (unix seconds), ...one array per series]; null = gap. */
  data: [number[], ...(number | null)[][]];
  series: ChartSeries[];
  height?: number;
  /** Fixed y range; otherwise auto from 0. */
  yMin?: number;
  yMax?: number;
  /** Formats y values (axis + legend). */
  fmt?: (v: number) => string;
  /** Called when the user drag-selects a time range (seconds). */
  onZoom?: (min: number, max: number) => void;
  /** Charts with the same sync key share the crosshair. */
  syncKey?: string;
  /** Fixed x range (seconds), e.g. the selected window. */
  xRange?: [number, number];
}

const axisColor = "#8b8b94";
const gridColor = "rgba(255,255,255,0.06)";

/**
 * Time-series line chart (uPlot). Drag across the plot to zoom in; the
 * parent can refetch finer data via onZoom. Double-click resets the zoom.
 */
export function TimeChart(props: TimeChartProps) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const propsRef = useRef(props);
  propsRef.current = props;
  const shape = props.series.map((s) => s.label + s.color).join("|") + (props.yMax ?? "") + (props.syncKey ?? "");

  useEffect(() => {
    const el = host.current!;
    const fmt = (v: number | null) => (v === null || v === undefined ? "–" : (propsRef.current.fmt ?? defaultFmt)(v));
    const opts: uPlot.Options = {
      width: el.clientWidth || 400,
      height: props.height ?? 160,
      padding: [8, 8, 0, 0],
      cursor: {
        drag: { x: true, y: false, setScale: true },
        sync: props.syncKey ? { key: props.syncKey } : undefined,
        points: { size: 6 },
      },
      legend: { live: true },
      scales: {
        x: { time: true, range: (_u, min, max) => propsRef.current.xRange ?? [min, max] },
        y: {
          range: (_u, _min, max) => {
            const p = propsRef.current;
            const hi = p.yMax ?? (max > 0 ? max * 1.1 : 1);
            return [p.yMin ?? 0, hi];
          },
        },
      },
      axes: [
        { stroke: axisColor, grid: { stroke: gridColor, width: 1 }, ticks: { stroke: gridColor }, font: "11px -apple-system, sans-serif" },
        {
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          ticks: { stroke: gridColor },
          font: "11px -apple-system, sans-serif",
          // Wide enough for the longest label ("123.4 MB/s").
          size: (_u, vals) => (vals && vals.length ? Math.max(44, Math.max(...vals.map((v) => String(v).length)) * 6.6 + 14) : 50),
          values: (_u, vals) => vals.map((v) => fmt(v)),
        },
      ],
      series: [
        { value: (_u, v) => (v ? new Date(v * 1000).toLocaleString() : "–") },
        ...props.series.map((s) => ({
          label: s.label,
          stroke: s.color,
          width: s.width ?? 1.5,
          dash: s.dash ? [4, 4] : undefined,
          fill: s.fill ? hexAlpha(s.color, 0.12) : undefined,
          points: { show: false },
          spanGaps: false,
          value: (_u: uPlot, v: number | null) => fmt(v),
        })),
      ],
      hooks: {
        setSelect: [
          (u) => {
            if (u.select.width > 2 && propsRef.current.onZoom) {
              const min = u.posToVal(u.select.left, "x");
              const max = u.posToVal(u.select.left + u.select.width, "x");
              propsRef.current.onZoom(Math.floor(min), Math.ceil(max));
            }
          },
        ],
      },
    };
    plot.current = new uPlot(opts, props.data as uPlot.AlignedData, el);
    const ro = new ResizeObserver(() => {
      if (el.clientWidth > 0) plot.current?.setSize({ width: el.clientWidth, height: propsRef.current.height ?? 160 });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot.current?.destroy();
      plot.current = null;
    };
  }, [shape, props.height]);

  useEffect(() => {
    plot.current?.setData(props.data as uPlot.AlignedData, true);
  }, [props.data, props.xRange?.[0], props.xRange?.[1]]);

  return <div className="time-chart" ref={host} />;
}

function defaultFmt(v: number) {
  if (Math.abs(v) >= 100) return v.toFixed(0);
  if (Math.abs(v) >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

function hexAlpha(color: string, a: number) {
  const m = color.match(/^#([0-9a-f]{6})$/i);
  if (!m) return color;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
}
