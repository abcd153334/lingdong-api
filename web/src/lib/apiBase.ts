/**
 * 后端 API 基址的**唯一真相源**。
 *
 * 为什么必须收敛成一个模块：这里曾经有三份 `import.meta.env.VITE_API_BASE_URL ?? "/api"`
 * （apiClient / sse / docsClient 各一份），其中 docsClient 干脆硬编码了 `/api`。
 * 部署到「前后端不同域、API 挂在 `https://api.example.com/api`」时，其余两处正常、
 * 文档站的「复制 Markdown / 下载 .md」全部 404 —— 而它只在点按钮时失败，
 * 页面看起来完全正常。
 *
 * dev 下 `VITE_API_BASE_URL` 未设，走 vite proxy 把 `/api` 转发到 :8080。
 */
export const API_BASE_URL: string = (import.meta.env.VITE_API_BASE_URL ?? "/api").replace(/\/+$/, "");

/**
 * 拼出一个 API 地址。
 *
 * 路径统一以 `/` 开头：调用方既有 `"/docs/catalog"` 也有 `"docs/catalog"` 的写法，
 * 两种都接受，避免出现 `/apidocs/catalog` 这种拼错的地址（那种错只在某个调用点出现）。
 */
export function apiUrl(path: string): string {
  const p = path.startsWith("/") ? path : `/${path}`;
  return `${API_BASE_URL}${p}`;
}
