import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ADMIN_NAV_COLLAPSED_KEY,
  adminNav,
  adminNavFlat,
  adminSectionIdOf,
  adminTitleOf,
  readCollapsedSections,
  writeCollapsedSections,
} from "./nav";

/**
 * 导航数据结构不变量。
 *
 * 这组断言替代了「模块加载时 throw 校验」：静态常量由单测 100% 覆盖就够了，
 * 顶层 throw 会让任何 import 它的入口（dev server、全部 e2e）一起炸掉，
 * 处罚力度与「展示层数据写错」完全不匹配。
 */
describe("adminNav 结构", () => {
  it("7 个分区 / 15 项，路径无重复且与路由一一对应", () => {
    // 模型服务合页：原来的「模型管理 / 渠道管理 / 适配器 / AI 接入助手」四项
    // 收敛成**顶级项**「模型服务」一项（助手保留深链，但不再占侧栏）⇒ 项数 18→15，
    // 可折叠分组 5→4（models 变成顶级项，分组数不变）。
    expect(adminNav).toHaveLength(7);
    expect(adminNavFlat).toHaveLength(15);
    expect(new Set(adminNavFlat.map((i) => i.path)).size).toBe(15);
  });

  it("分区 id 非空且唯一（折叠状态与 aria-controls 都拿它当键）", () => {
    const ids = adminNav.map((s) => s.id);
    expect(ids.every((id) => id.length > 0)).toBe(true);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("顶级项各 1 项且没有组标题，有 title 的分区 ≥2 项", () => {
    for (const s of adminNav) {
      if (s.title === undefined) expect(s.items, `${s.id} 是顶级项`).toHaveLength(1);
      else expect(s.items.length, `${s.title} 不足 2 项就该做成顶级项`).toBeGreaterThanOrEqual(2);
    }
  });

  it("三个最常用的入口是顶级项且排在最前（仪表盘 / 系统配置 / 模型服务）", () => {
    expect(adminNav.slice(0, 3).map((s) => s.title)).toEqual([undefined, undefined, undefined]);
    expect(adminNav[0].items[0].path).toBe("/admin/dashboard");
    expect(adminNav[1].items[0].path).toBe("/admin/config");
    expect(adminNav[2].items[0].path).toBe("/admin/model-service");
  });

  it("4 个分组的标题与顺序冻结（信息架构改动必须同步这条断言）", () => {
    expect(adminNav.filter((s) => s.title).map((s) => s.title)).toEqual([
      "用户与应用",
      "交易与资金",
      "运行与审计",
      "文档与接口",
    ]);
  });

  it("分区归属与面包屑同源：adminSectionIdOf / adminTitleOf 对同一 path 一致", () => {
    expect(adminSectionIdOf("/admin/config")).toBe("config");
    expect(adminSectionIdOf("/admin/model-service")).toBe("models");
    // 旧地址已删除：这里顺带钉死"没有第二个模型入口"
    expect(adminSectionIdOf("/admin/models")).toBeUndefined();
    expect(adminSectionIdOf("/admin/channels")).toBeUndefined();
    expect(adminSectionIdOf("/admin/logs")).toBe("runtime");
    expect(adminSectionIdOf("/admin/not-exist")).toBeUndefined();
    expect(adminTitleOf("/admin/logs")).toBe("调用日志");
    // 助手保留深链但不在侧栏：面包屑仍要有标题（off-nav 表兜底）
    expect(adminTitleOf("/admin/agent")).toBe("AI 接入助手");
  });
});

describe("折叠状态存储", () => {
  afterEach(() => {
    localStorage.removeItem(ADMIN_NAV_COLLAPSED_KEY);
    vi.restoreAllMocks();
  });

  it("脏数据一律降级为全展开", () => {
    for (const raw of ["{oops", '{"a":1}', '[1,null,""]', "null", '"models"']) {
      localStorage.setItem(ADMIN_NAV_COLLAPSED_KEY, raw);
      expect(readCollapsedSections(), raw).toEqual([]);
    }
  });

  it("写读往返；空数组删键而不是写 []", () => {
    writeCollapsedSections(["models", "docs"]);
    expect(JSON.parse(localStorage.getItem(ADMIN_NAV_COLLAPSED_KEY) ?? "null")).toEqual([
      "models",
      "docs",
    ]);
    expect(readCollapsedSections()).toEqual(["models", "docs"]);

    writeCollapsedSections([]);
    expect(localStorage.getItem(ADMIN_NAV_COLLAPSED_KEY)).toBeNull();
    expect(readCollapsedSections()).toEqual([]);
  });

  it("存储被禁用（隐私模式 / 配额满）时静默降级，不把异常抛给渲染层", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    expect(() => writeCollapsedSections(["models"])).not.toThrow();
  });
});
