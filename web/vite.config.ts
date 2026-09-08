import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The API is a separate process in development, so the dev server proxies to it rather
// than the app knowing an absolute origin. In production the two are served together.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: true },
      "/paid": { target: "http://localhost:8080", changeOrigin: true },
      "/healthz": { target: "http://localhost:8080", changeOrigin: true },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
  },
});
