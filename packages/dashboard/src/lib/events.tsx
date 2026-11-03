import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import type { VardeEvent } from "../api/client";

type Listener = (e: VardeEvent) => void;
type Ctx = {
  subscribe: (fn: Listener) => () => void;
  connected: boolean;
  recent: VardeEvent[];
};

const EventsCtx = createContext<Ctx>({
  subscribe: () => () => {},
  connected: false,
  recent: [],
});

/** One SSE connection per group; components subscribe for live updates. */
export function GroupEventsProvider({
  groupId,
  children,
}: {
  groupId: string;
  children: ReactNode;
}) {
  const listeners = useRef(new Set<Listener>());
  const [connected, setConnected] = useState(false);
  const [recent, setRecent] = useState<VardeEvent[]>([]);

  useEffect(() => {
    setRecent([]);
    const es = new EventSource(
      `/v1/groups/${encodeURIComponent(groupId)}/events/stream`,
    );
    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);
    es.onmessage = (m) => {
      let ev: VardeEvent;
      try {
        ev = JSON.parse(m.data) as VardeEvent;
      } catch {
        return;
      }
      setRecent((r) => [ev, ...r].slice(0, 200));
      listeners.current.forEach((fn) => fn(ev));
    };
    return () => es.close();
  }, [groupId]);

  const subscribe = useCallback((fn: Listener) => {
    listeners.current.add(fn);
    return () => {
      listeners.current.delete(fn);
    };
  }, []);

  return (
    <EventsCtx.Provider value={{ subscribe, connected, recent }}>
      {children}
    </EventsCtx.Provider>
  );
}

export function useGroupEvents() {
  return useContext(EventsCtx);
}

/** Calls `fn` (debounced) whenever a matching live event arrives. */
export function useOnEvent(filter: (e: VardeEvent) => boolean, fn: () => void) {
  const { subscribe } = useGroupEvents();
  const fnRef = useRef(fn);
  const filterRef = useRef(filter);
  fnRef.current = fn;
  filterRef.current = filter;
  useEffect(() => {
    let t: ReturnType<typeof setTimeout> | undefined;
    const off = subscribe((e) => {
      if (!filterRef.current(e)) return;
      clearTimeout(t);
      t = setTimeout(() => fnRef.current(), 150);
    });
    return () => {
      clearTimeout(t);
      off();
    };
  }, [subscribe]);
}
