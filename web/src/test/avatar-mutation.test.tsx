import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useAvatarUploadMutation } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";

/**
 * 头像上传必须**同时**做两件事：
 *   1. `invalidateQueries(me)` —— 用户中心 / 账户弹窗这些读 query 的地方要刷新；
 *   2. 写回 `useAuthStore` —— 顶栏读的是 store 快照，`loadSession` 命中 loaded 后
 *      不会再探测，只失效 query 的话「头像换了、右上角还是旧的」。
 *
 * 第 2 条最容易在「给 mutate 传了 per-call onSuccess」之后悄悄失效（hook 级回调被
 * 覆盖时不会有任何报错），所以这里刻意用与 AvatarPicker 完全相同的调用形态。
 */
function wrapper(client: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

function stubFetch(avatarUrl: string) {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({ code: 0, message: "", error_code: "", data: { avatar_url: avatarUrl } }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    ),
  );
}

/** 持久化的 QueryClient：`gcTime:0` 不排 GC 定时器，否则 vitest 跑完会被挂起的计时器拖住不退。 */
let client: QueryClient | null = null;

afterEach(() => {
  vi.unstubAllGlobals();
  client?.clear();
  client = null;
  useAuthStore.getState().clear();
});

describe("头像上传", () => {
  it("成功后写回会话 store（带 per-call 回调也照样生效）", async () => {
    const url = "data:image/webp;base64,AVATAR";
    useAuthStore.setState({
      user: { id: 1, username: "alice", role: 1, avatar_url: "" },
      loaded: true,
    });
    stubFetch(url);

    client = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { gcTime: 0 } },
    });
    const { result } = renderHook(() => useAvatarUploadMutation(), { wrapper: wrapper(client) });

    act(() => {
      result.current.mutate(new File(["x"], "a.png", { type: "image/png" }), {
        onSuccess: () => undefined,
      });
    });

    await waitFor(() => expect(useAuthStore.getState().user?.avatar_url).toBe(url));
    // 其它字段不能被这次合并冲掉
    expect(useAuthStore.getState().user?.username).toBe("alice");
  });
});
