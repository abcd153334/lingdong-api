import { describe, expect, it } from "vitest";

import { noticeTime, typeMeta, unreadOf } from "@/lib/notice";
import type { PortalAnnouncementItem } from "@/types";

const item = (over: Partial<PortalAnnouncementItem> = {}): PortalAnnouncementItem => ({
  id: 1,
  content: "x",
  publishDate: "2026-10-20T09:00:00",
  type: "default",
  extra: "",
  read: false,
  ...over,
});

describe("unreadOf", () => {
  it("未读由条目 read 派生（接口不再下发 unread）", () => {
    expect(unreadOf([item(), item({ id: 2, read: true }), item({ id: 3 })])).toBe(2);
  });

  it("空 / undefined 视为 0", () => {
    expect(unreadOf([])).toBe(0);
    expect(unreadOf(undefined)).toBe(0);
  });
});

describe("typeMeta", () => {
  it("5 个枚举各自有文案", () => {
    expect(typeMeta("error").label).toBe("重要");
    expect(typeMeta("ongoing").label).toBe("进行中");
    expect(typeMeta("success").label).toBe("已完成");
    expect(typeMeta("warning").label).toBe("注意");
    expect(typeMeta("default").label).toBe("公告");
  });

  it("未知枚举回退「公告」，不留空白标签", () => {
    expect(typeMeta("notice").label).toBe("公告");
    expect(typeMeta("").label).toBe("公告");
  });
});

describe("noticeTime", () => {
  it("同年省略年份（弹层只有 384px）", () => {
    const sameYear = `${new Date().getFullYear()}-03-05T08:30:00`;
    expect(noticeTime(sameYear)).toBe("03-05 08:30");
  });

  it("跨年补全年份，避免去年的公告被当今年", () => {
    expect(noticeTime("2020-12-31T10:00:00")).toBe("2020-12-31");
  });

  it("非法时间返回空串，不渲染 Invalid Date", () => {
    expect(noticeTime("")).toBe("");
    expect(noticeTime("not-a-date")).toBe("");
  });
});
