import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 3000,
    strictPort: true,
    // Dev proxy: keep the browser same-origin (no CORS / no Keycloak redirect-URI
    // changes) while the v2 UI talks to the real gateway + Keycloak. /kc strips
    // its prefix; /v1 forwards verbatim. See src/v2/backend.ts.
    proxy: {
      "/v1": { target: "http://localhost:8080", changeOrigin: true },
      "/kc": {
        target: "http://localhost:8081",
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/kc/, ""),
      },
    },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["src/test-setup.ts"],
  },
});
