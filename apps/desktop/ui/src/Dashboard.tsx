import { useEffect, useMemo, useState } from "react";
import App from "@varde/dashboard/App";
import {
  PlatformProvider,
  type DashboardPlatform,
} from "@varde/dashboard/lib/platform";
import { navigate, RouterProvider } from "@varde/dashboard/lib/router";
import { installCpTransport } from "./cp";
import { invoke, listen } from "./tauri";

export type FaceInfo = {
  facet: "setup" | "dashboard";
  cp_url: string;
  group_id: string;
  node_id: string;
};

installCpTransport();

export default function Dashboard({ face }: { face: FaceInfo }) {
  useState(() => {
    if (window.location.pathname.endsWith("/index.html")) {
      window.history.replaceState(null, "", "/" + window.location.search);
    }
    return undefined;
  });
  const [sessionError, setSessionError] = useState<string | null>(null);
  const platform = useMemo<DashboardPlatform>(
    () => ({
      kind: "desktop",
      cpOrigin: face.cp_url,
      thisNodeId: face.node_id || null,
      thisGroupId: face.group_id || null,
      signInWithVarde() {
        void invoke("begin_varde_signin").catch((error: unknown) => {
          setSessionError(String(error));
        });
      },
      openExternal(url) {
        void invoke("open_url", { url }).catch((error: unknown) => {
          setSessionError(String(error));
        });
      },
      moveThisPc(groupId) {
        void invoke("request_move_here", { groupId }).catch(
          (error: unknown) => setSessionError(String(error)),
        );
      },
      linkSteam: null,
    }),
    [face.cp_url, face.group_id, face.node_id, setSessionError],
  );

  useEffect(() => {
    document.body.style.background = "var(--background)";
    let active = true;
    const unlisten: (() => void)[] = [];
    void listen("cp-session", () => {
      setSessionError(null);
      window.dispatchEvent(new Event("varde:session-changed"));
    }).then((stop) => {
      if (active) unlisten.push(stop);
      else stop();
    });
    void listen<string>("cp-session-error", (event) => {
      setSessionError(event.payload);
    }).then((stop) => {
      if (active) unlisten.push(stop);
      else stop();
    });
    void listen<{ path: string }>("dashboard-navigate", (event) => {
      navigate(event.payload.path);
    }).then((stop) => {
      if (active) unlisten.push(stop);
      else stop();
    });
    return () => {
      active = false;
      unlisten.forEach((stop) => stop());
    };
  }, []);

  return (
    <PlatformProvider platform={platform}>
      <RouterProvider>
        <div className="min-h-screen bg-[var(--background)] text-foreground">
          {sessionError && (
            <div
              className="border-b border-danger/30 bg-danger/10 px-5 py-3 text-sm text-danger"
              role="alert"
            >
              {sessionError}
            </div>
          )}
          <App />
        </div>
      </RouterProvider>
    </PlatformProvider>
  );
}
