import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // Dev-only: forward API calls to a local control-plane.
      // Keep the browser's Host so the control plane's same-origin
      // (CSRF) check sees the dev server's origin.
      "/v1": { target: "http://localhost:8080", changeOrigin: false },
      "/healthz": { target: "http://localhost:8080", changeOrigin: false },
    },
  },
});
