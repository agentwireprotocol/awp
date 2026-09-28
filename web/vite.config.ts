import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

const publicHost = process.env.VITE_PUBLIC_HOST

// https://vite.dev/config/
export default defineConfig({
  base: "/",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "./src"),
    },
  },
  server: {
    host: "127.0.0.1",
    port: Number(process.env.PORT) || 5173,
    // Behind an HTTPS proxy (a sprite's URL, say): VITE_PUBLIC_HOST names
    // the public host, so Vite accepts it and hot reload connects back
    // through the proxy.
    ...(publicHost && {
      allowedHosts: [publicHost],
      hmr: { host: publicHost, protocol: "wss", clientPort: 443 },
    }),
    proxy: {
      // `awp web --listen 127.0.0.1:7788`, or AWP_WEB=http://host:port
      "/api": { target: process.env.AWP_WEB || "http://127.0.0.1:7788", changeOrigin: true },
    },
  },
  build: {
    target: "es2022",
    chunkSizeWarningLimit: 400,
  },
})
