import { useQuery } from "@tanstack/react-query";

import { apiClient } from "@/lib/apiClient";
import type { DocV7Catalog, DocV7Endpoint, DocV7Node } from "@/types/docsv7";

/**
 * 文档站 v7 的数据访问层。
 *
 * 三级各一个接口：目录（索引）→ 节点（成员 + 接口清单）→ 接口（全文）。
 * 页面一次只取「树 + 当前接口」，不为了渲染一篇文档把全站字段都拉下来。
 */

export const docsV7Keys = {
  tree: ["docs", "v7", "tree"] as const,
  node: (key: string) => ["docs", "v7", "node", key] as const,
  endpoint: (node: string, ep: string) => ["docs", "v7", "ep", node, ep] as const,
};

// 30 秒：后台发布后读者不该等 5 分钟才看到新内容。
// ETag/304 会把重复请求的代价压到一次往返，这个值可以很小。
const STALE = 30_000;

export function useDocsTree(enabled = true) {
  return useQuery({
    queryKey: docsV7Keys.tree,
    queryFn: () => apiClient.get<DocV7Catalog>("/docs/tree"),
    staleTime: STALE,
    refetchOnWindowFocus: true,
    // 管理台预览自带 base_url（树接口会给），不必再拉一次公开目录：
    // 那一次请求在文档站未开通时还会 404，把预览拖成错误态。
    enabled,
  });
}

export function useDocNode(key: string, enabled = true) {
  return useQuery({
    queryKey: docsV7Keys.node(key),
    queryFn: () => apiClient.get<DocV7Node>(`/docs/nodes/${encodeURIComponent(key)}`),
    staleTime: STALE,
    refetchOnWindowFocus: true,
    enabled: enabled && !!key,
  });
}

export function useDocEndpoint(node: string, ep: string, enabled = true) {
  return useQuery({
    queryKey: docsV7Keys.endpoint(node, ep),
    queryFn: () =>
      apiClient.get<DocV7Endpoint>(
        `/docs/nodes/${encodeURIComponent(node)}/endpoints/${encodeURIComponent(ep)}`,
      ),
    staleTime: STALE,
    refetchOnWindowFocus: true,
    enabled: enabled && !!node && !!ep,
  });
}
