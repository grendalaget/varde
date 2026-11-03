import { createContext, useContext, type ReactNode } from "react";
import type { Me, Node } from "../api/client";
import { usePlatform } from "./platform";

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

const queryNode = (() => {
  const key = "varde.this-node";
  const node = new URLSearchParams(window.location.search).get("node");
  try {
    if (node) sessionStorage.setItem(key, node);
    return node ?? sessionStorage.getItem(key);
  } catch {
    return node;
  }
})();

export function useThisNode(): string | null {
  const { thisNodeId } = usePlatform();
  return thisNodeId ?? queryNode;
}

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
