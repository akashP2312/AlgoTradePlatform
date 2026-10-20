import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dev server proxies /ws to a running gateway: the dry run's 18091 by default,
// PMT_GATEWAY=127.0.0.1:8091 for the real stack. The production build lands in
// web/dist, which the gateway itself serves at /.
const gateway = process.env.PMT_GATEWAY ?? "127.0.0.1:18091";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy: { "/ws": { target: `ws://${gateway}`, ws: true, changeOrigin: true } },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: true, chunkSizeWarningLimit: 1024 },
});
