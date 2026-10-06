import React from "react";
import { createRoot } from "react-dom/client";
import { FluentProvider } from "@fluentui/react-components";
import { vardeDark } from "./theme";
import App from "./App";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <FluentProvider theme={vardeDark} style={{ height: "100%" }}>
      <App />
    </FluentProvider>
  </React.StrictMode>,
);
