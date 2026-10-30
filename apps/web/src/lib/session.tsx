import { createContext, useContext, type ReactNode } from "react";
import type { Me, Node } from "../api/client";

export type MeGroup = Me["groups"][number];

export type Session = {
  me: Me;
  group: MeGroup;
  refreshMe: () => Promise<void>;
  selectGroup: (id: string) => void;
  nodes: Node[];
  /** false until this group's first nodes fetch lands */
  nodesLoaded: boolean;
  refreshNodes: () => Promise<void>;
};

const SessionCtx = createContext<Session | null>(null);

export function SessionProvider({
  value,
  children,
}: {
  value: Session;
  children: ReactNode;
}) {
  return <SessionCtx.Provider value={value}>{children}</SessionCtx.Provider>;
}

export function useSession(): Session {
  const s = useContext(SessionCtx);
  if (!s) throw new Error("useSession outside SessionProvider");
  return s;
}

/** This PC's node id when the dashboard runs in the desktop app (?node=).
 * Read once at load: the app reloads the page when it changes. Kept in tab
 * storage so a reload of that window still knows. */
export const thisNode: string | null = (() => {
  const key = "varde.this-node";
  const n = new URLSearchParams(window.location.search).get("node");
  try {
    if (n) sessionStorage.setItem(key, n);
    return n ?? sessionStorage.getItem(key);
  } catch {
    return n;
  }
})();

export function canAdmin(role: string) {
  return role === "owner" || role === "admin";
}

/** node_id → display name, for rendering ids anywhere in the UI. */
export function useNodeName() {
  const { nodes } = useSession();
  return (id?: string | null) => {
    if (!id) return "-";
    return nodes.find((n) => n.id === id)?.name ?? id;
  };
}
