import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The server serves the build at /app/. In development, the API is the
// server at PAI_URL, default http://127.0.0.1:8080.
const api = process.env.PAI_URL ?? "http://127.0.0.1:8080";
const routes = ["/events", "/graph", "/catalog", "/memory", "/sessions", "/workers", "/providers", "/join", "/keys", "/settings"];

export default defineConfig({
  base: "/app/",
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true, chunkSizeWarningLimit: 900 },
  server: { proxy: Object.fromEntries(routes.map((r) => [r, { target: api, changeOrigin: true }])) },
});
