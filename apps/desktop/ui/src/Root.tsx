import { useEffect, useRef, useState } from "react";
import App from "./App";
import Dashboard, { type FaceInfo } from "./Dashboard";
import { invoke, listen } from "./tauri";

declare global {
  interface Window {
    // set by the tray's initialization script (see app.rs): true when the
    // webview composites over a Mica/acrylic backdrop
    __VARDE_GLASS__?: boolean;
  }
}

export default function Root() {
  const [face, setFace] = useState<FaceInfo | null>(null);
  const currentFace = useRef<FaceInfo | null>(null);

  function updateFace(next: FaceInfo) {
    const previous = currentFace.current;
    const previousKey = previous
      ? `${previous.cp_url}|${previous.group_id}|${previous.node_id}`
      : null;
    const nextKey = `${next.cp_url}|${next.group_id}|${next.node_id}`;
    if (
      previous?.facet === "dashboard" &&
      next.facet === "dashboard" &&
      previousKey !== nextKey
    ) {
      window.history.replaceState(null, "", "/");
    }
    currentFace.current = next;
    setFace(next);
  }

  useEffect(() => {
    let active = true;
    let receivedFacet = false;
    let unlisten: (() => void) | undefined;
    void listen<FaceInfo>("facet", (event) => {
      receivedFacet = true;
      updateFace(event.payload);
    }).then((stop) => {
      if (active) unlisten = stop;
      else stop();
    });
    void invoke<FaceInfo>("get_face").then((current) => {
      if (active && !receivedFacet) updateFace(current);
    });
    return () => {
      active = false;
      unlisten?.();
    };
  }, []);

  useEffect(() => {
    document.body.style.background =
      face?.facet === "dashboard" || window.__VARDE_GLASS__ !== true
        ? "var(--background)"
        : "transparent";
  }, [face?.facet]);

  return face?.facet === "dashboard" ? (
    <Dashboard
      key={`${face.cp_url}|${face.group_id}|${face.node_id}`}
      face={face}
    />
  ) : (
    <App />
  );
}
