import { useCallback, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from "react";

/**
 * Renders only the rows inside (or near) the viewport of a scroll container.
 * Row heights may vary (e.g. wrapped text): unmeasured rows use `estimate`,
 * rendered rows are measured after layout and cached by key, so the total
 * height converges as the user scrolls. Everything before/after the window is
 * replaced by two spacer divs, so rows stay in normal flow (CSS unchanged).
 */
export function VirtualRows<T>(props: {
  items: T[];
  itemKey: (item: T, index: number) => string | number;
  render: (item: T, index: number) => ReactNode;
  /** The scrolling element that contains this component. */
  scrollRef: RefObject<HTMLElement | null>;
  /** Estimated row height in px. */
  estimate: number;
  overscan?: number;
  /** Changing this discards measured heights (e.g. when wrap mode toggles). */
  layoutKey?: unknown;
  /** Use "tr" when the rows are table rows (inside <tbody>). */
  spacer?: "div" | "tr";
  /** Column count for "tr" spacers. */
  colSpan?: number;
}) {
  const { items, itemKey, scrollRef, estimate } = props;
  const overscan = props.overscan ?? 10;
  const heights = useRef(new Map<string | number, number>());
  const [version, setVersion] = useState(0);
  const [view, setView] = useState({ top: 0, height: 800 });
  const topRef = useRef<HTMLElement | null>(null);
  const lastLayout = useRef(props.layoutKey);
  if (lastLayout.current !== props.layoutKey) {
    lastLayout.current = props.layoutKey;
    heights.current.clear();
  }

  // Prefix offsets (cheap even for tens of thousands of rows).
  const offsets = useMemo(() => {
    const o = new Float64Array(items.length + 1);
    for (let i = 0; i < items.length; i++) {
      o[i + 1] = o[i] + (heights.current.get(itemKey(items[i], i)) ?? estimate);
    }
    return o;
  }, [items, version, estimate]);

  // Track the container's scroll position and size (rAF-throttled). A
  // child's layout effect runs before the parent's ref is attached, so the
  // container is looked up again on the next frame if it isn't there yet.
  useLayoutEffect(() => {
    let el: HTMLElement | null = null;
    let raf = 0;
    let ro: ResizeObserver | null = null;
    let disposed = false;
    const read = () => {
      raf = 0;
      if (!el) return;
      const base = topRef.current ? offsetWithin(topRef.current, el) : 0;
      setView((v) => {
        const top = Math.max(0, el!.scrollTop - base);
        const height = el!.clientHeight;
        return v.top === top && v.height === height ? v : { top, height };
      });
    };
    const onScroll = () => {
      if (!raf) raf = requestAnimationFrame(read);
    };
    const attach = () => {
      if (disposed) return;
      el = scrollRef.current;
      if (!el) {
        raf = requestAnimationFrame(attach);
        return;
      }
      read();
      el.addEventListener("scroll", onScroll, { passive: true });
      ro = new ResizeObserver(onScroll);
      ro.observe(el);
    };
    attach();
    return () => {
      disposed = true;
      el?.removeEventListener("scroll", onScroll);
      ro?.disconnect();
      if (raf) cancelAnimationFrame(raf);
    };
  }, [scrollRef]);

  const find = useCallback(
    (y: number) => {
      let lo = 0;
      let hi = items.length;
      while (lo < hi) {
        const mid = (lo + hi) >> 1;
        if (offsets[mid + 1] <= y) lo = mid + 1;
        else hi = mid;
      }
      return lo;
    },
    [offsets, items.length],
  );

  const start = Math.max(0, find(view.top) - overscan);
  const end = Math.min(items.length, find(view.top + view.height) + overscan + 1);

  // Measure rendered rows; re-render once if any height changed.
  // Rendered rows are the siblings between the two spacers.
  useLayoutEffect(() => {
    let node = topRef.current?.nextElementSibling as HTMLElement | null;
    let changed = false;
    for (let idx = start; idx < end && node && !node.dataset.vspacer; idx++) {
      const k = itemKey(items[idx], idx);
      const h = node.getBoundingClientRect().height;
      if (h > 0 && Math.abs((heights.current.get(k) ?? -1) - h) > 0.5) {
        heights.current.set(k, h);
        changed = true;
      }
      node = node.nextElementSibling as HTMLElement | null;
    }
    if (changed) setVersion((v) => v + 1);
  });

  const rows: ReactNode[] = [];
  for (let i = start; i < end; i++) rows.push(props.render(items[i], i));
  const top = offsets[start];
  const bottom = offsets[items.length] - offsets[end];
  const setTop = (el: HTMLElement | null) => {
    topRef.current = el;
  };
  if (props.spacer === "tr") {
    const cell = (h: number) => <td colSpan={props.colSpan ?? 1} style={{ height: h, padding: 0, border: 0 }} />;
    return (
      <>
        <tr ref={setTop} data-vspacer="1" aria-hidden>
          {cell(top)}
        </tr>
        {rows}
        <tr data-vspacer="1" aria-hidden>
          {cell(bottom)}
        </tr>
      </>
    );
  }
  return (
    <>
      <div ref={setTop} data-vspacer="1" style={{ height: top }} aria-hidden />
      {rows}
      <div data-vspacer="1" style={{ height: bottom }} aria-hidden />
    </>
  );
}

/** Distance from the top of the scroll container's content to el. */
function offsetWithin(el: HTMLElement, container: HTMLElement): number {
  return el.getBoundingClientRect().top - container.getBoundingClientRect().top + container.scrollTop;
}
