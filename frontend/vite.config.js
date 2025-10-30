import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In prod we serve frontend via `vite preview` (or any static server).
// Proxy API during dev to backend on :8080.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
});
