import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

// The build output is embedded by internal/web/assets.go via go:embed ui/dist.
// We keep relative asset paths so the SPA works when served from "/".
export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
});
