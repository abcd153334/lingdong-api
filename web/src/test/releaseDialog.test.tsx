import { render, screen, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ReleaseDialog } from "@/components/admin/ReleaseDialog";
import type { ReleaseInfo } from "@/types";

/**
 * 版本与更新弹窗的四类状态。
 *
 * 守的是"失败绝不能显示成已是最新"：清单抓不到、地址没配、清单字段非法时，
 * 界面必须显示原因。这条如果退化，管理员会以为"已经最新"而错过一次必修的升级。
 */

function envelope(data: unknown) {
  return new Response(JSON.stringify({ code: 0, message: "success", error_code: "", data }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function base(over: Partial<ReleaseInfo> = {}): ReleaseInfo {
  return {
    current: { version: "v2.1.0", node: "n1", mode: "full" },
    agent: { online: true, host: "prod-1", current_tag: "v2.1.0", seen_ago_sec: 3 },
    manifest: {
      configured: true,
      url: "https://example.com/release.json",
      error_kind: "",
      checked_at: "2026-11-18T10:00:00Z",
      latest: {
        version: "v2.2.0",
        tag: "v2.2.0",
        published_at: "2026-11-18",
        notes: "修复一处计费口径",
        commit: "",
        url: "",
      },
      update_available: true,
      comparable: true,
    },
    update: {
      can_update: true,
      blocked_reason: "",
      active_job: null,
      last_job: null,
    },
    running_tasks: 0,
    ...over,
  };
}

function stub(info: ReleaseInfo) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/admin/system/release/update/current")) {
        return envelope({ active: null, last: null, server_time: "2026-11-18T10:00:00Z" });
      }
      return envelope(info);
    }),
  );
}

let client: QueryClient | null = null;

function renderDialog(info: ReleaseInfo) {
  stub(info);
  client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <ReleaseDialog open onClose={() => {}} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client?.clear();
  client = null;
});

describe("ReleaseDialog", () => {
  it("未配置清单时说明该去哪里配，而不是显示已是最新", async () => {
    renderDialog(
      base({
        manifest: {
          configured: false,
          url: "",
          error_kind: "not_configured",
          update_available: false,
          comparable: true,
        },
      }),
    );
    expect(await screen.findByText(/还没有配置发布清单地址/)).toBeTruthy();
    expect(screen.queryByText("已是最新")).toBeNull();
  });

  it("检查失败时显示原因，绝不显示已是最新", async () => {
    renderDialog(
      base({
        manifest: {
          configured: true,
          url: "https://example.com/r.json",
          error_kind: "timeout",
          checked_at: "2026-11-18T10:00:00Z",
          update_available: false,
          comparable: true,
        },
        update: { can_update: false, blocked_reason: "manifest_error", active_job: null, last_job: null },
      }),
    );
    expect(await screen.findByText(/无法确认最新版本/)).toBeTruthy();
    expect(screen.getByText(/抓取超时/)).toBeTruthy();
    expect(screen.queryByText("已是最新")).toBeNull();
  });

  it("有更新时显示版本与说明", async () => {
    renderDialog(base());
    expect(await screen.findByText("有新版本")).toBeTruthy();
    expect(screen.getByText("v2.2.0")).toBeTruthy();
    expect(screen.getByText(/修复一处计费口径/)).toBeTruthy();
  });

  it("已是最新时不显示更新按钮", async () => {
    renderDialog(
      base({
        manifest: {
          configured: true,
          url: "https://example.com/r.json",
          error_kind: "",
          checked_at: "2026-11-18T10:00:00Z",
          latest: { version: "v2.1.0", tag: "v2.1.0", published_at: "", notes: "", commit: "", url: "" },
          update_available: false,
          comparable: true,
        },
        update: { can_update: false, blocked_reason: "no_update", active_job: null, last_job: null },
      }),
    );
    expect(await screen.findByText("已是最新")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "立即更新" })).toBeNull();
  });

  it("更新程序离线时按钮禁用并说明怎么装", async () => {
    renderDialog(
      base({
        agent: { online: false },
        update: { can_update: false, blocked_reason: "agent_offline", active_job: null, last_job: null },
      }),
    );
    const btn = (await screen.findByRole("button", { name: "立即更新" })) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(screen.getByText(/ld\.sh updater/)).toBeTruthy();
  });

  // 回归：线上真实故障。`last` 是"最近一条终态任务、没有时间窗口"，
  // 曾经一次成功更新就让弹窗永久停在"刚完成"分支，把更新按钮与重新检查全部吞掉。
  it("上次更新成功但已经是旧版本时，仍然给出立即更新与重新检查", async () => {
    renderDialog(
      base({
        update: {
          can_update: true,
          blocked_reason: "",
          active_job: null,
          last_job: {
            id: 9,
            target_tag: "v2.1.0",
            status: "success",
            step: "done",
            progress: 100,
            message: "已更新到 v2.1.0",
            attempt: 1,
            requested_at: "2026-11-10T10:00:00Z",
            finished_at: "2026-11-10T10:05:00Z",
          },
        },
      }),
    );
    expect(await screen.findByRole("button", { name: "立即更新" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "重新检查" })).toBeTruthy();
  });

  it("刚更新完成时主按钮是刷新页面，重新检查仍在", async () => {
    const now = new Date().toISOString();
    renderDialog(
      base({
        manifest: {
          configured: true,
          url: "https://example.com/r.json",
          error_kind: "",
          checked_at: now,
          latest: { version: "v2.1.0", tag: "v2.1.0", published_at: "", notes: "", commit: "", url: "" },
          update_available: false,
          comparable: true,
        },
        update: {
          can_update: false,
          blocked_reason: "no_update",
          active_job: null,
          last_job: {
            id: 10,
            target_tag: "v2.1.0",
            status: "success",
            step: "done",
            progress: 100,
            message: "已更新到 v2.1.0",
            attempt: 1,
            requested_at: now,
            finished_at: now,
          },
        },
      }),
    );
    expect(await screen.findByRole("button", { name: "刷新页面" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "重新检查" })).toBeTruthy();
  });

  // 数据迁移型终态是"完成"，不是"失败"：容器已经是新版本，只是不能只回退镜像。
  it("数据迁移型终态显示为已完成，并给出刷新页面", async () => {
    const now = new Date().toISOString();
    renderDialog(
      base({
        manifest: {
          configured: true,
          url: "https://example.com/r.json",
          error_kind: "",
          checked_at: now,
          latest: { version: "v2.1.0", tag: "v2.1.0", published_at: "", notes: "", commit: "", url: "" },
          update_available: false,
          comparable: true,
        },
        update: {
          can_update: false,
          blocked_reason: "no_update",
          active_job: null,
          last_job: {
            id: 11,
            target_tag: "v2.1.0",
            status: "manual_required",
            step: "done",
            progress: 100,
            message: "更新已完成，但期间执行了数据迁移",
            failure_code: "data_epoch_changed",
            attempt: 1,
            requested_at: now,
            finished_at: now,
          },
        },
      }),
    );
    expect(await screen.findByText("已完成·待确认")).toBeTruthy();
    expect(screen.getByRole("button", { name: "刷新页面" })).toBeTruthy();
    // 不能再出现红色失败块
    expect(document.querySelector(".rel-err")).toBeNull();
  });

  it("构建中显示步骤与进度，且不再显示更新按钮", async () => {
    renderDialog(
      base({
        update: {
          can_update: false,
          blocked_reason: "already_running",
          active_job: {
            id: 3,
            target_tag: "v2.2.0",
            status: "building",
            step: "building",
            progress: 35,
            message: "构建镜像",
            attempt: 1,
            requested_at: "2026-11-18T10:00:00Z",
          },
          last_job: null,
        },
      }),
    );
    expect(await screen.findByText("构建中")).toBeTruthy();
    expect(screen.getByText("35%")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "立即更新" })).toBeNull();
  });
});
