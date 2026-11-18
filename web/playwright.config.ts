import { defineConfig, devices } from '@playwright/test'

// E2E 基线：门户/管理台 chrome、主题、响应式。
// 固定 127.0.0.1:5178（见 vite.config.ts server 注释），避免 localhost 解析到 ::1 命中他项目服务。
export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  // 串行执行（方案 P1-7）：全量并行下 vite dev 代理存在秒级抖动，真实联调用例
  // （real-backend.spec）会因异步边界超时而随机失败；实测两次全量跑出现 129/2 与 131/131 漂移，
  // 失败集漂移不等于产品缺陷，故门禁固定单 worker 以消除资源竞争假失败。
  fullyParallel: false,
  workers: 1,
  use: { baseURL: 'http://127.0.0.1:5178' },
  webServer: {
    command: 'pnpm dev',
    url: 'http://127.0.0.1:5178/login',
    reuseExistingServer: true,
    timeout: 60_000,
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
