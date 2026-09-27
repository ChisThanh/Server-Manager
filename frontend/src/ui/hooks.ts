import { useCallback, useEffect, useRef, useState } from "react";
import { errMsg } from "../lib/api";

/** Calls fn every ms milliseconds (null = paused). Always uses the latest fn. */
export function useInterval(fn: () => void, ms: number | null) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    if (ms === null) return;
    const id = setInterval(() => ref.current(), ms);
    return () => clearInterval(id);
  }, [ms]);
}

export interface Remote<T> {
  data: T | undefined;
  error: string;
  loading: boolean;
  /** Reloads; resolves when done. Errors are stored in `error`, not thrown. */
  reload: () => Promise<void>;
  setData: (v: T | undefined | ((prev: T | undefined) => T | undefined)) => void;
}

/**
 * Loads remote data when `enabled` (default true) and whenever deps change,
 * optionally polling. Out-of-order responses are discarded; data is kept on
 * error so a failing refresh doesn't blank the page.
 */
export function useRemote<T>(load: () => Promise<T>, deps: unknown[], opts: { enabled?: boolean; poll?: number } = {}): Remote<T> {
  const enabled = opts.enabled ?? true;
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const seq = useRef(0);
  const loadRef = useRef(load);
  loadRef.current = load;

  const reload = useCallback(async () => {
    const my = ++seq.current;
    setLoading(true);
    try {
      const v = await loadRef.current();
      if (my !== seq.current) return;
      setData(v);
      setError("");
    } catch (e) {
      if (my !== seq.current) return;
      setError(errMsg(e));
    } finally {
      if (my === seq.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (enabled) reload();
  }, [enabled, ...deps]);

  useInterval(() => {
    if (enabled && !document.hidden) reload();
  }, enabled && opts.poll ? opts.poll : null);

  return { data, error, loading, reload, setData };
}

/** Runs an action with a busy flag, reporting errors as a toast. */
export function useBusy() {
  const [busy, setBusy] = useState<string | null>(null);
  const run = useCallback(async <R,>(key: string, fn: () => Promise<R>): Promise<R | undefined> => {
    setBusy(key);
    try {
      return await fn();
    } finally {
      setBusy(null);
    }
  }, []);
  return { busy, run };
}

/**
 * Returns value while `active`, and the last active value otherwise, so a
 * hidden panel doesn't re-render for data it isn't showing.
 */
export function useFrozen<T>(value: T, active: boolean): T {
  const last = useRef(value);
  if (active) last.current = value;
  return last.current;
}
