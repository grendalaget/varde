import { useEffect, useState } from "react";
import { FluentProvider } from "@fluentui/react-components";
import { vardeTheme } from "./theme";
import App from "./App";

declare global {
  interface Window {
    // set by the tray's initialization script (see app.rs)
    __VARDE_GLASS__?: boolean;
    // the user's OS accent color (#rrggbb) or "" when unavailable
    __VARDE_ACCENT__?: string;
  }
}

const darkQuery = () => window.matchMedia("(prefers-color-scheme: dark)");

// The OS accent color. Preferred source is the tray's init script (HKCU
// DWM\AccentColor); fall back to the AccentColor CSS system color where a
// platform exposes it (WebView2 currently doesn't).
function osAccent(): string | null {
  if (window.__VARDE_ACCENT__) return window.__VARDE_ACCENT__;
  const probe = document.createElement("span");
  probe.style.color = "AccentColor";
  if (probe.style.color === "") return null;
  document.body.appendChild(probe);
  const rgb = getComputedStyle(probe).color;
  probe.remove();
  return rgb || null;
}

export default function Root() {
  const [dark, setDark] = useState(() => darkQuery().matches);
  const [accent, setAccent] = useState<string | null>(() => osAccent());
  useEffect(() => {
    const q = darkQuery();
    const onChange = (e: MediaQueryListEvent) => {
      setDark(e.matches);
      setAccent(osAccent()); // the scheme change can carry an accent change
    };
    q.addEventListener("change", onChange);
    return () => q.removeEventListener("change", onChange);
  }, []);

  const glass = window.__VARDE_GLASS__ === true;
  useEffect(() => {
    // let the backdrop show through instead of the body fallback color
    if (glass) document.body.style.background = "transparent";
  }, [glass]);

  return (
    <FluentProvider
      theme={vardeTheme(dark, accent, glass)}
      style={{ height: "100%" }}
    >
      <App />
    </FluentProvider>
  );
}
