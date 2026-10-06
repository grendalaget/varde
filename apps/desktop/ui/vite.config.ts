import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The app is embedded by Tauri (frontendDist: ui/dist): relative base so the
// bundle works under the tauri:// / http://tauri.localhost origins.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    // emit every asset as a file: the Tauri CSP allows 'self' only
    assetsInlineLimit: 0,
  },
});
