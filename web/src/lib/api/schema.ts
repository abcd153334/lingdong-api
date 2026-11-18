import { useQuery } from "@tanstack/react-query";
import { apiClient } from "@/lib/apiClient";

/**
 * 平台 schema 目录（`GET /admin/schema/catalog`）。
 *
 * 为什么前端要读它：档位白名单（图像 1K/2K/4K、视频 480p/720p/1080p/2k）
 * 的真源在**后端** `billing.ImageTierKeys` / `billing.VideoTierKeys`。
 * 前端曾各自写死一份，加一档只改一侧的表现是「能填却不能保存」或
 * 「后端支持但界面上没有这一行」，两种都很难被发现。
 *
 * 这里只取界面真正要用的部分（档位键），其余字段留给按需扩展：
 * 全量目录很大，无脑铺到前端状态里只是噪声。
 */

export interface SchemaCatalog {
  version: number;
  spec_prices?: {
    /** 档位白名单：image 必须大写 K，video 必须小写 p/k */
    tier_keys?: { image?: string[]; video?: string[] };
    kinds?: Record<string, string>;
  };
  limits?: {
    pricing?: { max_seconds?: number };
  };
}

export const schemaKeys = {
  catalog: ["admin", "schema", "catalog"] as const,
};

/**
 * 读 schema 目录。
 *
 * `staleTime` 给到 5 分钟：目录随版本号变，但它不是热数据，
 * 每开一次模型对话框就拉一遍没有收益，反而会在慢网下让价目表闪一下。
 * 失败时**不抛给页面**（调用方用本地常量兜底）：目录拉不到不该挡住运营改价。
 */
export function useSchemaCatalogQuery() {
  return useQuery({
    queryKey: schemaKeys.catalog,
    queryFn: ({ signal }) => apiClient.get<SchemaCatalog>("/admin/schema/catalog", { signal }),
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
}
