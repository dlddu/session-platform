import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In dev, the SPA runs on Vite's server and proxies /api to the control plane
// so the same relative API calls work in dev and in the embedded prod build.
// In prod, `vite build` emits to dist/, which the control plane embeds & serves.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.CONTROL_PLANE_URL ?? "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
  },
});
