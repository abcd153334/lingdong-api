import { useCallback, useState } from "react";

import { toastErr } from "@/components/common/Toaster";
import { useKeysQuery, useRevealKeyMutation } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";

/**
 * 「用我自己的密钥」—— 两个调试面板（`/docs` 在线运行、`/admin/api-docs` 试运行）共用这一份。
 *
 * 三条事实决定了它只能这么写：
 *  1. **未登录时不能发** `/portal/keys`：401 会触发全站的会话失效处理，公开页上一个
 *     匿名访客就能把整页判死（`useKeysQuery` 的 `enabled` 就是为此存在）。
 *  2. 只列 `kind === "user" && status === 1`：托管密钥取不到明文（后端 409）、
 *     停用的密钥调不通 —— 列出来只会让读者点两次才发现不行。
 *  3. **点了才取明文**：`/plaintext` 会写一条密钥审计，渲染时预取等于每次开面板都留痕。
 */
export function useMyKey(onKey: (plaintext: string) => void, enabled = true) {
  const user = useAuthStore((s) => s.user);
  const q = useKeysQuery({ enabled: enabled && !!user });
  const reveal = useRevealKeyMutation();
  const [pickId, setPickId] = useState<number | null>(null);

  const keys = (q.data?.items ?? []).filter((k) => k.kind === "user" && k.status === 1);
  // 没手动挑过就默认第一把：面板上不该出现「先选一个」这种多余动作。
  const current = pickId ?? keys[0]?.id ?? null;

  const take = useCallback(async () => {
    if (!current) return;
    try {
      const r = await reveal.mutateAsync(current);
      if (!r.plaintext) {
        toastErr("这把密钥不支持查看明文，请新建一把或手动粘贴");
        return;
      }
      onKey(r.plaintext);
    } catch {
      // 会话过期由全站统一处理，这里只说"还能手填"。
      toastErr("暂时取不到密钥，请手动粘贴");
    }
  }, [current, onKey, reveal]);

  return { keys, current, setPickId, take, taking: reveal.isPending };
}
