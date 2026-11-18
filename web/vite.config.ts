import { fileURLToPath, URL } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    // vitest 只跑 src 下的单测；e2e/ 由 Playwright 负责。
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
  },
  server: {
    // 固定 127.0.0.1:5178：避开 ::1 上他项目 dev server 与 Docker 端口转发对 5173 的占用；
    // strictPort 保证端口被占时直接报错，不会静默换端口导致 e2e 命中错误服务。
    host: "127.0.0.1",
    port: 5178,
    strictPort: true,
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8080",
        changeOrigin: true,
      },
      // /v1：API 文档页运行台（lib/apiDocs/client.ts 在 DEV 强制同源）直连后端网关
      "/v1": {
        target: "http://127.0.0.1:8080",
        changeOrigin: true,
      },
      // /f：上传文件的公网直链（`{api_base_url}/f/{id}/{name}`）。
      //
      // 必须转发到后端：图像编辑（/v1/images/edits）把 multipart 文件转成直链交给上游，
      // 上游会**从公网回取这个地址**。不转发时请求会落到前端拿到 index.html，
      // 上游解析失败只会回「任务失败（未返回原因）」——完全无法归因。
      // 用正则锚定 `/f/`，不能用前缀 "/f"：
      // 前缀匹配会把前端的 `/files` 页面也转给后端（实测整页返回 404 JSON），
      // 而这个"页面打不开"的表现完全不会指向代理配置。
      "^/f/": {
        target: "http://127.0.0.1:8080",
        changeOrigin: true,
      },
    },
  },
});
