import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { apiClient } from "@/lib/apiClient";
import { docsV7Keys } from "@/lib/api/docsv7";
import type { DocV7Endpoint, DocV7Node } from "@/types/docsv7";
import type {
  DocSamples,
  DocsAdminEndpoint,
  DocsAdminEndpointData,
  DocsAdminNode,
  DocsAdminNodeData,
  DocsAdminSection,
  DocsAdminSectionData,
  DocsAdminTree,
  DocsAdminTransport,
  DocKV,
} from "@/types/docsadmin";

/**
 * 文档编辑页的数据层。
 *
 * 三条约定（改之前先读）：
 *  1. **写成功后同时失效公开站的缓存**：文档站有 30s staleTime，只失效管理台的话，
 *     运营保存完去 `/docs` 看到的还是旧内容，会以为没生效 —— 这就是「前后台不同步」
 *     的观感来源，哪怕数据其实已经写进去了。
 *  2. **不做乐观更新**：文档是直接对外承诺的产物，「先显示成功、失败再回滚」会让
 *     运营在那 200ms 里以为已经生效。宁可慢一次，也不要一次假成功。
 *  3. 预览是**查询**（key 里带草稿指纹），不是 mutation：同一份草稿反复切页签
 *     不该重复请求，而乱序响应会落在旧 key 上，不会覆盖新画布。
 */

export const docsAdminKeys = {
  tree: ["admin", "docs", "tree"] as const,
  node: (key: string) => ["admin", "docs", "node", key] as const,
  ep: (node: string, ep: string) => ["admin", "docs", "ep", node, ep] as const,
  transports: ["admin", "docs", "transports"] as const,
  preview: (node: string, ep: string, hash: string) =>
    ["admin", "docs", "preview", node, ep, hash] as const,
  nodePreview: (key: string, hash: string) =>
    ["admin", "docs", "node-preview", key, hash] as const,
};

const enc = encodeURIComponent;

function useInvalidateDocs() {
  const qc = useQueryClient();
  return (node?: string, ep?: string) => {
    void qc.invalidateQueries({ queryKey: docsAdminKeys.tree });
    if (node) void qc.invalidateQueries({ queryKey: docsAdminKeys.node(node) });
    if (node && ep) void qc.invalidateQueries({ queryKey: docsAdminKeys.ep(node, ep) });
    // 公开站（同一份数据、另一套投影）。
    void qc.invalidateQueries({ queryKey: docsV7Keys.tree });
    if (node) void qc.invalidateQueries({ queryKey: docsV7Keys.node(node) });
    if (node && ep) void qc.invalidateQueries({ queryKey: docsV7Keys.endpoint(node, ep) });
  };
}

export function useDocsAdminTree() {
  return useQuery({
    queryKey: docsAdminKeys.tree,
    queryFn: () => apiClient.get<DocsAdminTree>("/admin/docs/tree"),
    staleTime: 10_000,
  });
}

export function useDocsAdminNode(key: string) {
  return useQuery({
    queryKey: docsAdminKeys.node(key),
    queryFn: () => apiClient.get<DocsAdminNode>(`/admin/docs/nodes/${enc(key)}`),
    enabled: !!key,
  });
}

export function useDocsAdminEndpoint(node: string, ep: string) {
  return useQuery({
    queryKey: docsAdminKeys.ep(node, ep),
    queryFn: () =>
      apiClient.get<DocsAdminEndpoint>(`/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}`),
    enabled: !!node && !!ep,
  });
}

export function useDocsAdminTransports() {
  return useQuery({
    queryKey: docsAdminKeys.transports,
    queryFn: () => apiClient.get<DocsAdminTransport[]>("/admin/docs/transports"),
    // 契约只在发版时变；保存绑定后靠 invalidate 刷新占用情况。
    staleTime: 5 * 60_000,
  });
}

/* ------------------------------ 写 ------------------------------ */

export function useSaveSection(key: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (body: DocsAdminSectionData) =>
      apiClient.put<DocsAdminSection>(`/admin/docs/sections/${enc(key)}`, body),
    onSuccess: () => invalidate(),
  });
}

export function useSaveNode(key: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (body: DocsAdminNodeData) =>
      apiClient.put<DocsAdminNode>(`/admin/docs/nodes/${enc(key)}`, body),
    onSuccess: () => invalidate(key),
  });
}

export function useCreateNode() {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (body: {
      key: string;
      kind: string;
      title: string;
      section: string;
      summary?: string;
    }) => apiClient.post<DocsAdminNode>("/admin/docs/nodes", body),
    onSuccess: (d) => invalidate(d.key),
  });
}

export function useDeleteNode() {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (key: string) => apiClient.delete(`/admin/docs/nodes/${enc(key)}`),
    onSuccess: () => invalidate(),
  });
}

export function useNodeAction(node: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (action: "publish" | "unpublish") =>
      apiClient.post<DocsAdminNode>(`/admin/docs/nodes/${enc(node)}/${action}`),
    onSuccess: () => invalidate(node),
  });
}

export function useCreateEndpoint(node: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (body: { key: string } & DocsAdminEndpointData) =>
      apiClient.post<DocsAdminEndpoint>(`/admin/docs/nodes/${enc(node)}/endpoints`, body),
    onSuccess: (d) => invalidate(node, d.key),
  });
}

export function useSaveEndpoint(node: string, ep: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (body: DocsAdminEndpointData) =>
      apiClient.put<DocsAdminEndpoint>(
        `/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}`,
        body,
      ),
    onSuccess: () => invalidate(node, ep),
  });
}

export function useDeleteEndpoint(node: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (ep: string) =>
      apiClient.delete(`/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}`),
    onSuccess: () => invalidate(node),
  });
}

/**
 * 发布 / 转草稿：**一个动作上线一整页**。
 *
 * 返回 `{endpoint, node}`：发布可能连带改节点状态，只重置接口的 base 会让节点
 * 立刻显示"未保存"（它的 version 已经变了）。
 */
export function useEndpointAction(node: string, ep: string) {
  const invalidate = useInvalidateDocs();
  return useMutation({
    mutationFn: (action: "publish" | "unpublish") =>
      apiClient.post<{ endpoint: DocsAdminEndpoint; node: DocsAdminNode }>(
        `/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}/${action}`,
      ),
    onSuccess: () => invalidate(node, ep),
  });
}

/* ------------------------------ 生成 / 预览 ------------------------------ */

/** 按当前草稿的字段示例值重新生成四段示例代码（不落库）。 */
export function useGenerateSamples(node: string, ep: string) {
  return useMutation({
    mutationFn: (body: DocsAdminEndpointData) =>
      apiClient.post<{ samples: DocSamples | null }>(
        `/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}/samples`,
        body,
      ),
  });
}

/** 按某个调用方式的 body 生成它的示例代码（不落库）。 */
export function useGeneratePresetSamples(node: string, ep: string) {
  return useMutation({
    mutationFn: (body: DocKV[]) =>
      apiClient.post<{ samples: DocSamples | null }>(
        `/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}/presets/samples`,
        { body },
      ),
  });
}

/**
 * 草稿指纹：把「会影响渲染的那几列」序列化成一个稳定短串。
 *
 * 只取内容列（不含 version/sort 这类不影响画面的值），否则改一次顺序就换 key、
 * 预览重新请求 —— 而画面上什么都没变。
 */
export function draftHash(v: unknown): string {
  const s = JSON.stringify(v);
  // FNV-1a：够短、够稳，且不引入依赖。
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0).toString(36) + "-" + s.length.toString(36);
}

export function useEndpointPreview(
  node: string,
  ep: string,
  data: DocsAdminEndpointData | null,
  enabled: boolean,
) {
  const hash = data ? draftHash(data) : "";
  return useQuery({
    queryKey: docsAdminKeys.preview(node, ep, hash),
    queryFn: () =>
      apiClient.post<DocV7Endpoint>(
        `/admin/docs/nodes/${enc(node)}/endpoints/${enc(ep)}/preview`,
        data,
      ),
    enabled: enabled && !!data,
    retry: false,
    // 切回编辑再切回预览时先显示上一份，不闪空。
    placeholderData: (prev) => prev,
  });
}

export function useNodePreview(
  key: string,
  data: DocsAdminNodeData | null,
  enabled: boolean,
) {
  const hash = data ? draftHash(data) : "";
  return useQuery({
    queryKey: docsAdminKeys.nodePreview(key, hash),
    queryFn: () => apiClient.post<DocV7Node>(`/admin/docs/nodes/${enc(key)}/preview`, data),
    enabled: enabled && !!data,
    retry: false,
    placeholderData: (prev) => prev,
  });
}
