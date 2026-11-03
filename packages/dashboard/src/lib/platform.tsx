import { createContext, useContext, type ReactNode } from "react";

export type DashboardPlatform = {
  kind: "web" | "desktop";
  cpOrigin: string;
  thisNodeId: string | null;
  thisGroupId: string | null;
  signInWithVarde(): void;
  openExternal(url: string): void;
  moveThisPc(groupId: string): void;
  linkSteam: ((redirectUrl: string) => void) | null;
};

export const FED_RETURN_KEY = "varde.fed-return";
export const SIGNED_OUT_KEY = "varde.signed-out";
export const FED_RETURN_TTL_MS = 15 * 60 * 1000;

const VARDE_SITE = "https://varde.games";

export const webPlatform: DashboardPlatform = {
  kind: "web",
  cpOrigin: window.location.origin,
  thisNodeId: null,
  thisGroupId: null,
  signInWithVarde() {
    sessionStorage.removeItem(SIGNED_OUT_KEY);
    sessionStorage.setItem(
      FED_RETURN_KEY,
      `${window.location.pathname + window.location.search}|${Date.now()}`,
    );
    window.location.assign(`${VARDE_SITE}/federate/cp?next=/`);
  },
  openExternal(url) {
    window.open(url, "_blank", "noopener");
  },
  moveThisPc(groupId) {
    window.location.assign(
      `varde://relink?group=${encodeURIComponent(groupId)}`,
    );
  },
  linkSteam(redirectUrl) {
    window.location.href = redirectUrl;
  },
};

const PlatformContext = createContext<DashboardPlatform>(webPlatform);

export function PlatformProvider({
  platform,
  children,
}: {
  platform: DashboardPlatform;
  children: ReactNode;
}) {
  return (
    <PlatformContext.Provider value={platform}>
      {children}
    </PlatformContext.Provider>
  );
}

export function usePlatform(): DashboardPlatform {
  return useContext(PlatformContext);
}
