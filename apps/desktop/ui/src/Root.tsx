import { useEffect } from "react";
import App from "./App";

declare global {
  interface Window {
    // set by the tray's initialization script (see app.rs): true when the
    // webview composites over a Mica/acrylic backdrop
    __VARDE_GLASS__?: boolean;
  }
}

export default function Root() {
  useEffect(() => {
    // let the backdrop show through instead of the body fallback color
    if (window.__VARDE_GLASS__ === true)
      document.body.style.background = "transparent";
  }, []);
  return <App />;
}
