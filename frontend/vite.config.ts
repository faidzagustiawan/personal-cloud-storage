import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The SPA and the API share an origin in production (spec §7.4), so the dev
// server proxies rather than enabling CORS. That keeps the httpOnly session
// cookie working and keeps the server's Origin check meaningful in development
// — both of which would be papered over by a permissive CORS config.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      "^/api/": { target: "http://127.0.0.1:8080", changeOrigin: false },
      // Share links are /s/:token. Anchored as a regex on purpose: a plain
      // "/s" key matches by prefix, which quietly swallows /src/* and every
      // other path that happens to start with an s.
      "^/s/": { target: "http://127.0.0.1:8080", changeOrigin: false },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: true,
  },
});
