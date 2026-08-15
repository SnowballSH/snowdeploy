import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [tailwindcss(), svelte()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // The dev identity mirrors what the fronting proxy asserts in
      // production, so the daemon's auth seam stays exercised in dev.
      "/api": {
        target: "http://127.0.0.1:8092",
        headers: { "Remote-User": "dev" },
      },
      "/healthz": {
        target: "http://127.0.0.1:8092",
        headers: { "Remote-User": "dev" },
      },
    },
  },
});
