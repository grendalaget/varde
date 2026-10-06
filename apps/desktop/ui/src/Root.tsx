import { useEffect, useState } from "react";
import { FluentProvider } from "@fluentui/react-components";
import { vardeTheme } from "./theme";
import App from "./App";

const darkQuery = () => window.matchMedia("(prefers-color-scheme: dark)");

export default function Root() {
  // follows the OS theme; WebView2 updates the media query live
  const [dark, setDark] = useState(() => darkQuery().matches);
  useEffect(() => {
    const q = darkQuery();
    const onChange = (e: MediaQueryListEvent) => setDark(e.matches);
    q.addEventListener("change", onChange);
    return () => q.removeEventListener("change", onChange);
  }, []);
  return (
    <FluentProvider theme={vardeTheme(dark)} style={{ height: "100%" }}>
      <App />
    </FluentProvider>
  );
}
