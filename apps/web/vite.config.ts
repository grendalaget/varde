import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // Dev-only: forward API calls to a local control-plane.
      "/v1": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
});
