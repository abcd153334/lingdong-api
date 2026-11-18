/**
 * 价目表档位的**预填建议**（不是白名单）。
 *
 * 真源在后端 `billing.ImageTierKeys` / `billing.VideoTierKeys`，前端通过
 * `GET /admin/schema/catalog` 的 `spec_prices.tier_keys` 读取（见 lib/api/schema.ts）。
 * 这里的常量只在目录尚未返回、或请求失败时顶上。
 *
 * 为什么必须有兜底：价目表整块消失比"少一个档位"严重得多 —— 运营会以为模型
 * 没有按量/按秒计价，转而去找别的入口。目录是增强，不是前置依赖。
 *
 * 它们只决定「新建模型时预填哪几行」：档位由运营自由配置（8K / 2.5K / 1440p…），
 * 页面同时会把**已配的键**并进来，并允许现场新增任意档位。
 * 与后端 `billing.ImageTierKeys` / `VideoTierKeys` 保持逐字一致（`billing` 包的
 * TestFrontendTierKeysMatchBackend 会读本文件比对，漂移即红），避免预填顺序分叉。
 */
// 1.5K 是正式档位（官方：与 1K 同价、效果更优），不是 1K 的别名。
export const FALLBACK_IMAGE_TIERS = ["1K", "1.5K", "2K", "3K", "4K"];
export const FALLBACK_VIDEO_TIERS = ["480p", "720p", "1080p", "2k", "4k"];
