import { useCallback, useEffect, useRef, useState } from "react";

/**
 * Minimal fetch hook. `deps` re-run the loader; `refresh()` re-runs it on
 * demand (e.g. when a live event arrives). Keeps stale data while reloading.
 */
export function useLoad<T>(
  loader: () => Promise<{ data?: T; error?: unknown }>,
  deps: unknown[],
) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<unknown>();
  const [loading, setLoading] = useState(true);
  const loaderRef = useRef(loader);
  loaderRef.current = loader;
  const seq = useRef(0);

  const refresh = useCallback(async () => {
    const my = ++seq.current;
    setLoading(true);
    try {
      const r = await loaderRef.current();
      if (my !== seq.current) return;
      if (r.error) setError(r.error);
      else {
        setError(undefined);
        setData(r.data);
      }
    } catch (e) {
      if (my === seq.current) setError(e);
    } finally {
      if (my === seq.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { data, error, loading, refresh, setData };
}
