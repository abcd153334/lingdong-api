import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";

import { AppCard } from "@/pages/admin/apps/AppCard";
import {
  brandFormOf,
  hasError,
  isBrandDirty,
  toISO,
  validateBrand,
  validateNotice,
  validateRelease,
} from "@/pages/admin/apps/validation";
import type { AdminAppBrand, AdminAppItem } from "@/types";

const base: AdminAppItem = {
  id: 2,
  slug: "web-portal",
  name: "官网门户",
  description: "面向 C 端用户的站点",
  status: 1,
  allow_self_issue: true,
  access_mode: "self_service",
  display_name: "官网门户",
  tagline: "面向 C 端用户的站点",
  logo_url: "",
  maintenance_mode: false,
  maintenance_message: "",
  created_at: "2026-10-20 10:00:00",
  key_count: 5,
  active_key_count: 4,
  user_count: 3,
  used: 12_345_678,
  paid_cent: 21_600,
  paid_count: 3,
  calls_7d: 1880,
  notice_count: 2,
  latest_release: { version: "2.1.0", published_at: "2026-10-21 09:10:00", force_update: false },
};

describe("AppCard", () => {
  it("展示密钥(启用/全部)、用户数、近 7 日调用与版本", () => {
    render(<AppCard app={base} onManage={vi.fn()} onKeys={vi.fn()} />);
    expect(screen.getByText("web-portal")).toBeInTheDocument();
    expect(screen.getByText("4/5")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("1,880")).toBeInTheDocument();
    expect(screen.getByText(/v2\.1\.0/)).toBeInTheDocument();
  });

  it("停用 / 维护中 / 关闭自助签发 并列显示，互不覆盖", () => {
    render(
      <AppCard
        app={{ ...base, status: 2, maintenance_mode: true, allow_self_issue: false }}
        onManage={vi.fn()}
        onKeys={vi.fn()}
      />,
    );
    expect(screen.getByText("停用")).toBeInTheDocument();
    expect(screen.getByText("维护中")).toBeInTheDocument();
    expect(screen.getByText("关闭自助签发")).toBeInTheDocument();
  });

  it("从未发布版本时给出明确文案，而不是空白", () => {
    render(<AppCard app={{ ...base, latest_release: null }} onManage={vi.fn()} onKeys={vi.fn()} />);
    expect(screen.getByText(/未发布版本/)).toBeInTheDocument();
  });

  it("启停不在卡片上（低频高风险，放在弹窗概览）", () => {
    render(<AppCard app={base} onManage={vi.fn()} onKeys={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /停用|启用/ })).toBeNull();
  });
});

describe("brand validation", () => {
  const ok = {
    displayName: "灵动画影",
    tagline: "",
    intro: "",
    logoIcon: "",
    contactName: "",
    contactEmail: "",
    supportURL: "",
    privacyURL: "",
    termsURL: "",
  };

  it("展示名称必填", () => {
    expect(validateBrand({ ...ok, displayName: "  " }).displayName).toBe("展示名称必填");
  });

  it("只接受 http(s) 绝对地址", () => {
    expect(validateBrand({ ...ok, supportURL: "/help" }).supportURL).toBeTruthy();
    expect(validateBrand({ ...ok, supportURL: "javascript:alert(1)" }).supportURL).toBeTruthy();
    expect(hasError(validateBrand({ ...ok, supportURL: "https://x.com/help" }))).toBe(false);
  });

  it("邮箱格式", () => {
    expect(validateBrand({ ...ok, contactEmail: "bad" }).contactEmail).toBeTruthy();
  });
});

/**
 * 脏值判定（页签「未保存」圆点 + 切页签保活的前提）。
 *
 * 判据必须与"还原按钮恢复的那份值"同源：两处各写一遍九个字段，
 * 加字段时漏一处就表现为"还原后仍然显示未保存"。
 */
describe("brand dirty tracking", () => {
  const server: AdminAppBrand = {
    display_name: "官网门户",
    tagline: "面向 C 端",
    intro: "",
    logo_icon: "",
    logo_url: "",
    contact_name: "",
    contact_email: "ops@example.com",
    support_url: "",
    privacy_url: "",
    terms_url: "",
  };

  it("刚加载时不是脏的", () => {
    expect(isBrandDirty(brandFormOf(server), server)).toBe(false);
  });

  it("任一字段改动（含图标令牌）都算脏", () => {
    const base = brandFormOf(server);
    expect(isBrandDirty({ ...base, tagline: "改过的标语" }, server)).toBe(true);
    expect(isBrandDirty({ ...base, logoIcon: "upload:01HXYZ" }, server)).toBe(true);
  });

  it("还原回服务器值后重新变净", () => {
    const edited = { ...brandFormOf(server), intro: "临时写的简介" };
    expect(isBrandDirty(edited, server)).toBe(true);
    expect(isBrandDirty(brandFormOf(server), server)).toBe(false);
  });
});

describe("notice validation", () => {
  const ok = {
    title: "维护通知",
    content: "今晚升级",
    level: "info" as const,
    enabled: true,
    startsAt: "",
    endsAt: "",
  };

  it("结束时间必须晚于开始时间", () => {
    const e = validateNotice({ ...ok, startsAt: "2026-11-12T10:00", endsAt: "2026-11-12T09:00" });
    expect(e.endsAt).toBe("结束时间必须晚于开始时间");
  });

  it("时间由前端补时区转成 RFC3339", () => {
    const iso = toISO("2026-11-12T10:00");
    expect(iso).toMatch(/Z$/);
    expect(new Date(iso as string).getTime()).toBe(new Date("2026-11-12T10:00").getTime());
  });

  it("留空表示不限", () => {
    expect(toISO("")).toBeNull();
  });
});

describe("release validation", () => {
  const ok = {
    version: "2.4.0",
    releaseNotes: "修复若干问题",
    downloadURL: "https://dl.example.com/a.exe",
    forceUpdate: false,
  };

  it("版本号必须是 semver 形态", () => {
    expect(validateRelease({ ...ok, version: "2.4" }).version).toBeTruthy();
    expect(validateRelease({ ...ok, version: "v2.4.0" }).version).toBeTruthy();
    expect(hasError(validateRelease(ok))).toBe(false);
  });

  it("允许带预发布后缀", () => {
    expect(hasError(validateRelease({ ...ok, version: "2.4.0-beta.1" }))).toBe(false);
  });

  it("下载地址必须是 http(s)", () => {
    expect(validateRelease({ ...ok, downloadURL: "ftp://x/a.exe" }).downloadURL).toBeTruthy();
  });
});
