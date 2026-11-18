import { describe, expect, it } from "vitest";
import type { AdminConfigValues } from "@/types";
import {
  CONFIG_KEY_TAB,
  CONFIG_TAB_KEYS,
  configTabDirty,
  configTabPayload,
  mergeTabBaseline,
  tabOfMessage,
  unownedConfigKeys,
} from "./configTabs";

/** 全量配置夹具：类型受 AdminConfigValues 约束，后端改键时这里先红 */
function fixture(): AdminConfigValues {
  return {
    site: {
      site_name: "灵动 API",
      logo_image: "",
      favicon_image: "",
      site_url: "https://example.com",
      api_base_urls: ["https://api.example.com"],
      contact_email: "ops@example.com",
      footer_text: "© 2026",
      icp_beian: "",
      currency: "CNY",
      currency_symbol: "¥",
    },
    release_manifest_url: "",
    group_discount: { default: 1_000_000 },
    cny_per_usd: 7.2,
    balance_limit: 0,
    min_topup: 1_000_000,
    custom_topup_enabled: true,
    pay_channels: [],
    retry_times: 3,
    file_retention_days: 7,
    poll_deadline_min: 30,
    logs_retention_days: 30,
    archive_retention_days: 60,
    audit_retention_days: 90,
    rate_limit_defaults: { rpm: 600, concurrency: 60 },
    auth_captcha: {
      scenes: { login: "after_fails" },
    },
    auth_login: {
      fail_threshold: 5,
      backoff_seconds: [60, 300, 900],
      ip_fail_limit: 20,
      ip_fail_window_sec: 600,
      delay_max_ms: 800,
    },
    auth_session: { absolute_ttl_sec: 604800, idle_ttl_sec: 43200, max_active: 10, touch_interval_sec: 300 },
    auth_register: { mode: "open", contact_verify: "off", contact_channels: ["email"] },
    auth_mfa: { policy: "optional" },
    auth_event_retention_days: 90,
    storage: { active: "none", channels: {} },
    uploads: {
      enabled: true,
      daily_limit_per_key: 200,
      retention_hours: 24,
      user_total_mb: 2048,
      concurrency: 3,
      types: { png: "image", mp4: "video", txt: "text" },
      limits: { image: 20, audio: 100, video: 200, document: 30, text: 5 },
      compress: { enabled: true, quality: 82, format: "webp" },
    },
    email: {
      enabled: false,
      smtp_host: "",
      smtp_port: 465,
      encryption: "ssl",
      username: "",
      password: "",
      from_addr: "",
      sender_name: "",
      scenes: { task_done: true, low_balance: true, price_change: false },
      templates: { reset_password: { subject: "s", body: "b" } },
    },
    sms: { enabled: false, active: "aliyun", daily_limit: 100, channels: { aliyun: {}, tencent: {} } },
    aff: {
      enabled: true,
      inviter_rate: 100_000,
      invitee_amount: 5_000_000,
      max_rewards: 3,
      hold_hours: 48,
      min_transfer: 100_000,
      daily_invite_cap: 20,
    },
    monitor: {
      auto_disable: true,
      fail_threshold: 0.5,
      fail_window: 5,
      auto_enable: true,
      probe_interval_min: 5,
    },
    announcements: { enabled: true, items: [] },
    about_content: "关于",
    agreement_content: "协议",
    modality_defaults: {
      chat: "gpt-4o",
      image: "",
      audio: "",
      video: "",
      music: "",
      embedding: "",
      rerank: "",
      moderation: "",
    },
  };
}

describe("配置页签归属表", () => {
  it("每个页签都有键，且并集覆盖全部配置键（无孤儿键）", () => {
    const all = Object.keys(CONFIG_KEY_TAB);
    expect(unownedConfigKeys(Object.keys(fixture()))).toEqual([]);
    for (const keys of Object.values(CONFIG_TAB_KEYS)) expect(keys.length).toBeGreaterThan(0);
    // 派生表不重复计数：每个键只属一个页签
    const flat = Object.values(CONFIG_TAB_KEYS).flat();
    expect(flat.slice().sort()).toEqual(all.slice().sort());
  });

  it("提交包只含本页签的键", () => {
    const form = fixture();
    expect(Object.keys(configTabPayload("notify", form, form)).sort()).toEqual(["email", "sms"]);
    expect(Object.keys(configTabPayload("models", form, form)).sort()).toEqual([
      "modality_defaults",
      "monitor",
      "poll_deadline_min",
      "rate_limit_defaults",
      "retry_times",
    ]);
    // 支付渠道单独成页签：计费页签的保存不得顺带提交它
    expect(Object.keys(configTabPayload("pay", form, form))).toEqual(["pay_channels"]);
    expect(Object.keys(configTabPayload("billing", form, form))).not.toContain("pay_channels");
    // 产物保留天数必须与 storage 同页签：两者互相约束，分开放只能看见一半的值
    const storage = Object.keys(configTabPayload("storage", form, form));
    expect(storage).toContain("storage");
    expect(storage).toContain("file_retention_days");
  });

  it("site 跨页签：各页签只写自己那几个字段，其余回填已落库基线", () => {
    const baseline = fixture();
    const form = fixture();
    form.site.currency = "USD";
    form.site.site_name = "改名中（未保存）";

    const billing = configTabPayload("billing", form, baseline) as { site: Record<string, unknown> };
    expect(billing.site.currency).toBe("USD");
    // 站点名属通用页签，不能跟着计费页签的保存一起发布
    expect(billing.site.site_name).toBe("灵动 API");
    // 派生字段不写回：符号由币种在后端派生，写回去等于给已删除的键续命
    // （切到 USD 后库里还留着 ¥，下次读出就是「币种 USD 却显示 ¥」）
    expect(billing.site.currency_symbol).toBeUndefined();

    const general = configTabPayload("general", form, baseline) as { site: Record<string, unknown> };
    expect(general.site.site_name).toBe("改名中（未保存）");
    // 币种属计费页签，通用页签保存不得顺带改币种
    expect(general.site.currency).toBe("CNY");
  });

  it("脏判定只看本页签", () => {
    const baseline = fixture();
    const form = fixture();
    form.email.enabled = true;
    expect(configTabDirty("notify", form, baseline)).toBe(true);
    expect(configTabDirty("billing", form, baseline)).toBe(false);

    const form2 = fixture();
    form2.site.currency = "USD";
    expect(configTabDirty("billing", form2, baseline)).toBe(true);
    expect(configTabDirty("general", form2, baseline)).toBe(false);

    // 支付渠道与计费互不牵连：改渠道只脏支付页签
    const form3 = fixture();
    form3.pay_channels = [];
    form3.group_discount = { default: 900_000 };
    expect(configTabDirty("billing", form3, baseline)).toBe(true);
    expect(configTabDirty("pay", form3, baseline)).toBe(false);
  });

  it("记录型字段改键序不算改动", () => {
    const baseline = fixture();
    baseline.group_discount = { a: 1_000_000, b: 900_000 };
    const form = fixture();
    form.group_discount = { b: 900_000, a: 1_000_000 };
    expect(configTabDirty("billing", form, baseline)).toBe(false);
  });

  it("保存后基线只推进本页签", () => {
    const baseline = fixture();
    const form = fixture();
    form.email.enabled = true;
    form.site.site_name = "未保存的站点名";

    const next = mergeTabBaseline("notify", form, baseline);
    expect(next.email.enabled).toBe(true);
    // 通用页签的编辑仍然「未保存」
    expect(next.site.site_name).toBe("灵动 API");
    expect(configTabDirty("general", form, next)).toBe(true);
    expect(configTabDirty("notify", form, next)).toBe(false);
  });
});

describe("保存失败 → 定位页签", () => {
  it("键前缀由归属表派生（checkNum 会把键拼进文案）", () => {
    expect(tabOfMessage("retry_times 取值不合法")).toBe("models");
    expect(tabOfMessage("cny_per_usd 格式不合法")).toBe("billing");
    expect(tabOfMessage("modality_defaults 格式不合法")).toBe("models");
    expect(tabOfMessage("pay_channels 格式不合法")).toBe("pay");
  });

  it("中文标签走领域词表", () => {
    expect(tabOfMessage("余额上限必须是整数（单位：微，1 元 = 1000000 微）")).toBe("billing");
    expect(tabOfMessage("默认模型「audio」与模型模态不符")).toBe("models");
    expect(tabOfMessage("支付方式不能为空")).toBe("pay");
    expect(tabOfMessage("会话绝对有效期必须是正整数")).toBe("security");
  });

  it("命中不唯一或完全不命中都不跳", () => {
    expect(tabOfMessage("")).toBeNull();
    expect(tabOfMessage("未知错误")).toBeNull();
  });
});
