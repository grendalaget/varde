import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Embedded by Tauri (frontendDist: ui/dist): relative base so the bundle
// works under the tauri:// / http://tauri.localhost origins.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "./",
  build: {
    assetsInlineLimit: 0,
  },
});
