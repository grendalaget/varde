import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The app is embedded by Tauri (frontendDist: ui/dist): relative base so the
// bundle works under the tauri:// / http://tauri.localhost origins.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "./",
  server: {
    proxy: {
      "/v1": { target: "http://localhost:8090", changeOrigin: false },
    },
  },
  build: {
    // Keep scripts and fonts on the app origin, which the CSP allows.
    assetsInlineLimit: 0,
  },
});
