import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { ChannelForm } from "@/pages/admin/model-service/ChannelForm";
import type { AdapterInfo, ChannelItem, ChannelKeysResult } from "@/types";

/**
 * 渠道编辑抽屉的密钥区契约：
 *  1. 编辑态**没有**"填写将整体替换"的 textarea，密钥池直接以行列表平铺在抽屉里；
 *  2. 计数出自密钥池接口（不是渠道列表的快照），且只认当前渠道；
 *  3. 保存 PUT 的请求体**不含 keys** —— 带了后端 400，是"用旧前端配新后端"的唯一失败点；
 *  4. 新建态（还没有渠道 id）仍然只能逐行粘贴，textarea 必须留着。
 */
const h = vi.hoisted(() => ({
  updateCalls: [] as Record<string, unknown>[],
  keysOpened: [] as number[],
}));

const channel = (over: Partial<ChannelItem> = {}): ChannelItem =>
  ({
    id: 7,
    name: "多米",
    icon: "",
    icon_url: "",
    adapter_id: "openai",
    base_url: "https://api.example.com",
    multi_key_mode: "random",
    switch_key_retry: false,
    groups: ["default"],
    weight: 5,
    priority: 1,
    rpm_limit: 60,
    max_concurrency: 10,
    inflight: 0,
    inflight_limit: 10,
    status: 1,
    auto_ban: true,
    balance_cny: null,
    key_count: 3,
    key_enabled_count: 2,
    version: 4,
    health: {
      success_count: 0,
      fail_count: 0,
      fail_rate: 0,
      last_error: "",
      last_test_at: null,
      auto_disabled_at: null,
      disabled_reason: "",
    },
    adapter_config: {},
    adapter_options: null,
    bound_models: [],
    ...over,
  }) as ChannelItem;

const adapters: AdapterInfo[] = [
  {
    id: "openai",
    name: "OpenAI",
    label: "OpenAI",
    vendor: "openai",
    caps: [],
    status: 1,
    source: "builtin",
    editable: false,
    deletable: false,
    channel_count: 1,
    supports_declarative: true,
    supports_options: false,
    default_base_url: "https://api.openai.com",
    endpoints: [],
  },
];

vi.mock("@/lib/api", () => ({
  useCreateAdminChannelMutation: () => ({ isPending: false, mutate: () => {} }),
  useUpdateAdminChannelMutation: () => ({
    isPending: false,
    mutate: (vars: { id: number; body: Record<string, unknown> }) => {
      h.updateCalls.push(vars.body);
    },
  }),
  useAdminChannelKeysQuery: (id: number) => {
    h.keysOpened.push(id);
    const data: ChannelKeysResult = {
      version: 4,
      items: [
        {
          index: 0,
          key_id: "kid-1",
          fingerprint: "sk-aaa…1111",
          alias: "主账号",
          enabled: true,
          decrypt_ok: true,
          cooldown_reason: "",
          cooldown_remaining_sec: 0,
          last_error_status: 0,
          last_error_at: "",
        },
        {
          index: 1,
          key_id: "kid-2",
          fingerprint: "sk-bbb…2222",
          alias: "",
          enabled: true,
          decrypt_ok: true,
          cooldown_reason: "",
          cooldown_remaining_sec: 0,
          last_error_status: 0,
          last_error_at: "",
        },
      ],
      key_count: 2,
      broken_count: 0,
      key_enabled_count: 2,
    };
    return { data, isLoading: false, isError: false, error: null, refetch: () => {}, dataUpdatedAt: Date.now() };
  },
  usePatchAdminChannelKeysMutation: () => ({ isPending: false, mutate: () => {} }),
}));

const renderForm = (ch: ChannelItem | null = channel()) =>
  render(
    <ChannelForm
      open
      channel={ch}
      channels={ch ? [ch] : []}
      adapters={adapters}
      groups={["default"]}
      onClose={() => {}}
    />,
  );

beforeEach(() => {
  h.updateCalls = [];
  h.keysOpened = [];
});

describe("渠道编辑抽屉的密钥区", () => {
  it("编辑态不再有整体替换输入框，密钥行直接平铺在抽屉里", async () => {
    renderForm();
    const drawer = await screen.findByRole("dialog", { name: /编辑渠道/ });

    expect(drawer.querySelector("#chz-keys")).toBeNull();
    // 计数出自密钥池接口（key_count=2），不是渠道列表快照里的 3。
    expect(within(drawer).getByText(/已存 2 把 · 可用 2 把/)).toBeTruthy();
    expect(within(drawer).getByText("sk-aaa…1111")).toBeTruthy();
    expect(within(drawer).getByText("主账号")).toBeTruthy();
    // 只认当前渠道：不能拿到别人的池子（mock 不按 queryKey 去重，故只看调用参数）。
    expect(h.keysOpened.length).toBeGreaterThan(0);
    expect(h.keysOpened.every((id) => id === 7)).toBe(true);
  });

  it("保存 PUT 的请求体不含 keys", async () => {
    renderForm();
    const drawer = await screen.findByRole("dialog", { name: /编辑渠道/ });
    fireEvent.click(within(drawer).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(h.updateCalls).toHaveLength(1));
    // 带了后端 400：密钥池只有一条写路径（PATCH /keys）。
    expect("keys" in h.updateCalls[0]).toBe(false);
  });

  it("新建态没有渠道 id，仍保留逐行粘贴的输入框", async () => {
    renderForm(null);
    const drawer = await screen.findByRole("dialog", { name: "新建渠道" });

    expect(drawer.querySelector("#chz-keys")).toBeTruthy();
    expect(h.keysOpened).toEqual([]);
  });
});

describe("渠道编辑抽屉的官网区", () => {
  it("回填自己配置的官网，并在 Base URL 旁给出可跳转的外链", async () => {
    renderForm(channel({ website: "https://duomiapi.com", website_url: "https://duomiapi.com" }));
    const drawer = await screen.findByRole("dialog", { name: /编辑渠道/ });

    const input = drawer.querySelector("#chz-site") as HTMLInputElement;
    expect(input.value).toBe("https://duomiapi.com");

    const go = drawer.querySelector(".field-row__go") as HTMLAnchorElement;
    expect(go.getAttribute("href")).toBe("https://duomiapi.com");
    expect(go.getAttribute("target")).toBe("_blank");
    expect(go.getAttribute("rel")).toContain("noopener");
  });

  it("官网为空且适配器无默认时不渲染按钮；输入合法地址后才出现", async () => {
    renderForm(channel());
    const drawer = await screen.findByRole("dialog", { name: /编辑渠道/ });
    expect(drawer.querySelector(".field-row__go")).toBeNull();

    fireEvent.change(drawer.querySelector("#chz-site")!, { target: { value: "https://example.com" } });
    const go = drawer.querySelector(".field-row__go") as HTMLAnchorElement;
    expect(go.getAttribute("href")).toBe("https://example.com");
  });

  it("保存时 website 恒提交（清空 = 空串，不是省略）", async () => {
    renderForm(channel({ website: "https://old.example.com" }));
    const drawer = await screen.findByRole("dialog", { name: /编辑渠道/ });

    fireEvent.change(drawer.querySelector("#chz-site")!, { target: { value: "" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(h.updateCalls).toHaveLength(1));
    expect(h.updateCalls[0].website).toBe("");
  });
});
