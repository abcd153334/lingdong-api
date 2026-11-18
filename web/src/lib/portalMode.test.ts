import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const get = vi.fn();
vi.mock("@/lib/apiClient", () => ({ apiClient: { get: (...args: unknown[]) => get(...args) } }));

import { __setPortalModeForTest, invalidatePortalMode, loadPortalMode } from "@/lib/portalMode";

/**
 * 运营模式读取的口径测试。
 *
 * 这个模块是「门户能不能进」的唯一前端判据，一旦它把「缺字段」当成关闭，
 * 整个站点会在一次后端回滚后整体变成登录页 —— 所以默认值必须被钉住。
 */
describe("portalMode", () => {
  beforeEach(() => {
    get.mockReset();
    __setPortalModeForTest(null);
    invalidatePortalMode();
  });
  afterEach(() => {
    __setPortalModeForTest(null);
  });

  it("缺字段 = 开启（与后端 conf.Bool 的默认值一致）", async () => {
    get.mockResolvedValue({ site: {} });
    await expect(loadPortalMode()).resolves.toBe(true);
  });

  it("明确 false 才关闭", async () => {
    get.mockResolvedValue({ portal_enabled: false });
    await expect(loadPortalMode()).resolves.toBe(false);
  });

  it("请求失败回落开启（不能把正常站点挡在门外）", async () => {
    get.mockRejectedValue(new Error("network"));
    await expect(loadPortalMode()).resolves.toBe(true);
  });

  it("TTL 内只用一次请求，force 穿透缓存", async () => {
    get.mockResolvedValue({ portal_enabled: false });
    await loadPortalMode();
    await loadPortalMode();
    expect(get).toHaveBeenCalledTimes(1);

    await loadPortalMode(true);
    expect(get).toHaveBeenCalledTimes(2);
  });

  it("invalidate 之后重新拉取（保存配置 / 命中 403 时）", async () => {
    get.mockResolvedValue({ portal_enabled: true });
    await loadPortalMode();
    invalidatePortalMode();
    get.mockResolvedValue({ portal_enabled: false });
    await expect(loadPortalMode()).resolves.toBe(false);
    expect(get).toHaveBeenCalledTimes(2);
  });

  it("测试钩子直接给答案，不发请求", async () => {
    __setPortalModeForTest(false);
    await expect(loadPortalMode()).resolves.toBe(false);
    expect(get).not.toHaveBeenCalled();
  });
});
