/**
 * 系统配置：页签 ↔ 配置键 的唯一归属表。
 *
 * 为什么需要它：保存必须按页签分片。整包保存时，任何一个页签里半填的草稿
 * 都会让**整次**保存被后端拒绝 —— 运营改一个开关，却被另一页的「域名还没填」
 * 拦下来，只能在「先保存」和「继续填」之间二选一。
 *
 * 单一真相源：归属只在 `CONFIG_KEY_TAB` / `SITE_FIELD_TAB` 定义一次，
 * 页签键集、脏判定、提交包、保存后基线全部从这里派生。
 * 新增配置键时**必须**在这里登记，否则它永远保存不出去（`unownedConfigKeys` 兜底告警）。
 */
import type { AdminConfigValues, SiteInfo } from "@/types";

export type ConfigTab =
  | "general"
  | "billing"
  | "pay"
  | "storage"
  | "notify"
  | "models"
  | "security";

/**
 * 顶层配置键 → 页签。
 *
 * 归属按**读者与操作节奏**切，不按后端键名切：
 *   - 通用：对外可见的品牌与内容 + 谁能注册；
 *   - 计费 / 支付渠道：钱怎么算 与 钱怎么收，是两条独立的操作链
 *     （改折扣不该被「渠道密钥还没填」拦住，反之亦然）；
 *   - 存储：写到哪里 + 上传配额；
 *   - 通知：邮件 / 短信 / 模板；
 *   - 模型与调用：默认模型 + 路由限流 + 渠道熔断，都在「调用怎么走」这条线上；
 *   - 安全与数据：登录防护 / 会话 / 保留期。
 *
 * `site` 是唯一跨页签的对象键（币种属计费、品牌属通用），它在顶层按「通用」登记，
 * 字段级拆分见 `SITE_FIELD_TAB`。
 *
 * `satisfies Record<keyof AdminConfigValues, ConfigTab>` 是编译期完整性检查：
 * 后端新增一个配置键、类型里加了一个字段却忘了在这里登记时 **tsc 直接红**，
 * 而不是等到运营发现「这一项怎么改都存不进去」。
 */
const KEY_TAB_SOURCE = {
  // 通用：运营模式 / 站点品牌 / 公告 / 内容 / 注册与准入
  // 运营模式必须在"通用"：它和「谁能进来」（注册模式、二次验证）是同一条决策链。
  portal_enabled: "general",
  site: "general",
  announcements: "general",
  about_content: "general",
  agreement_content: "general",
  // 发布清单地址与站点同页签：它决定"这套部署怎么知道自己该升级到哪一版"，
  // 属于部署面配置，不是计费/存储/安全策略。
  release_manifest_url: "general",
  // 注册与二次验证放在「通用」，因为它们决定"谁能进来"，与站点一起第一次配置；
  // 登录防护属于"进来之后怎么防"，留在安全页签。
  auth_register: "general",
  auth_mfa: "general",
  // 计费
  group_discount: "billing",
  cny_per_usd: "billing",
  balance_limit: "billing",
  min_topup: "billing",
  custom_topup_enabled: "billing",
  aff: "billing",
  // 支付渠道单独成页签：它是唯一带「密钥 / 排序 / 启停」的独立操作链
  pay_channels: "pay",
  // 存储
  storage: "storage",
  uploads: "storage",
  // 产物保留天数与存储渠道必须同页签：转存目标（storage.active）与保留上限
  // （file_retention_days）互相约束，分开放只能看见一半的值。
  file_retention_days: "storage",
  // 通知服务
  email: "notify",
  sms: "notify",
  // 模型与调用
  modality_defaults: "models",
  retry_times: "models",
  poll_deadline_min: "models",
  rate_limit_defaults: "models",
  monitor: "models",
  // 安全与保留
  auth_captcha: "security",
  auth_login: "security",
  auth_session: "security",
  auth_event_retention_days: "security",
  logs_retention_days: "security",
  archive_retention_days: "security",
  audit_retention_days: "security",
} satisfies Record<keyof AdminConfigValues, ConfigTab>;

export const CONFIG_KEY_TAB: Readonly<Record<string, ConfigTab>> = KEY_TAB_SOURCE;

/** `site` 内部的字段级归属：币种在计费页签（它决定全站金额口径），其余在通用页签。 */
export const SITE_FIELD_TAB = {
  currency: "billing",
  // 符号由币种派生、不可自定义；跟着币种留在计费页签，避免通用页签误写
  currency_symbol: "billing",
  site_name: "general",
  logo_image: "general",
  favicon_image: "general",
  site_url: "general",
  api_base_urls: "general",
  contact_email: "general",
  footer_text: "general",
  icp_beian: "general",
} satisfies Record<keyof SiteInfo, ConfigTab>;

/** 每个页签拥有的顶层键（派生，不手写） */
export const CONFIG_TAB_KEYS: Readonly<Record<ConfigTab, readonly string[]>> = (() => {
  const out: Record<ConfigTab, string[]> = {
    general: [],
    billing: [],
    pay: [],
    storage: [],
    notify: [],
    models: [],
    security: [],
  };
  for (const [key, tab] of Object.entries(CONFIG_KEY_TAB)) out[tab].push(key);
  return out;
})();

/** 没被任何页签认领的键：这类键既不会出现在提交包里，也不会被脏判定看见 —— 必须零。 */
export function unownedConfigKeys(allKeys: readonly string[]): string[] {
  return allKeys.filter((k) => CONFIG_KEY_TAB[k] === undefined);
}

/** 每个页签拥有的 `site` 字段（派生） */
const OWNED_SITE_FIELDS: Readonly<Record<ConfigTab, readonly string[]>> = (() => {
  const out: Record<ConfigTab, string[]> = {
    general: [],
    billing: [],
    pay: [],
    storage: [],
    notify: [],
    models: [],
    security: [],
  };
  for (const [field, tab] of Object.entries(SITE_FIELD_TAB)) out[tab].push(field);
  return out;
})();

/**
 * `site` 里**由后端派生、不参与写入**的字段。
 *
 * 后端 `siteCfg` 已经没有这个字段（符号由 `site.currency` 派生，见 `conf/currency`），
 * 而 `Update` 是「按提交的原始 JSON 落库」。写回去等于给一个已删除的配置键续命：
 * 切币种后库里仍留着旧符号，下次读出就是「币种 USD 却显示 ¥」。
 */
const SITE_DERIVED_FIELDS: readonly string[] = ["currency_symbol"];

/**
 * 本页签的提交包。
 *
 * `site` 只写本页签拥有的字段，其余字段一律回填**已落库基线**：
 * 否则在计费页签保存币种时，会把站点页签里尚未保存的站点名一起写进去
 * （运营以为只改了币种，实际上把别人半填的品牌信息也发布了）。
 * 后端 `systemconfig.Update` 的语义是「未出现的键保持原值」，所以分片提交是安全的。
 */
export function configTabPayload(
  tab: ConfigTab,
  form: AdminConfigValues,
  baseline: AdminConfigValues,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, owner] of Object.entries(CONFIG_KEY_TAB)) {
    if (key === "site") {
      // 跨页签：只要本页签拥有 site 的**任一**字段就要带上这个键
      // （计费页签只拥有 currency，但它必须把 site.currency 提交出去）。
      const fields = OWNED_SITE_FIELDS[tab];
      if (fields.length === 0) continue;
      const merged: Record<string, unknown> = { ...(baseline.site as unknown as Record<string, unknown>) };
      for (const field of fields) {
        merged[field] = (form.site as unknown as Record<string, unknown>)[field];
      }
      for (const field of SITE_DERIVED_FIELDS) delete merged[field];
      out.site = merged;
      continue;
    }
    if (owner !== tab) continue;
    out[key] = (form as unknown as Record<string, unknown>)[key];
  }
  return out;
}

/** 本页签是否有未保存改动（只比较本页签拥有的那部分）。 */
export function configTabDirty(
  tab: ConfigTab,
  form: AdminConfigValues,
  baseline: AdminConfigValues,
): boolean {
  return stableStringify(configTabPayload(tab, form, baseline)) !==
    stableStringify(configTabPayload(tab, baseline, baseline));
}

/**
 * 把 `source` 里本页签拥有的那一片写到 `target` 上（其余键原样保留）。
 *
 * 两个用途共用一份实现，避免「推进基线」和「还原本页」在 `site` 这种跨页签键上
 * 各写一套字段判定、迟早对不上：
 *   - 保存成功后 `applyTabSlice(tab, baseline, form)` 推进基线；
 *   - 「还原本页」`applyTabSlice(tab, form, baseline)` 丢弃本页编辑。
 */
export function applyTabSlice(
  tab: ConfigTab,
  target: AdminConfigValues,
  source: AdminConfigValues,
): AdminConfigValues {
  const next = { ...target } as unknown as Record<string, unknown>;
  for (const [key, owner] of Object.entries(CONFIG_KEY_TAB)) {
    if (key === "site") {
      const fields = OWNED_SITE_FIELDS[tab];
      if (fields.length === 0) continue;
      const site = { ...(target.site as unknown as Record<string, unknown>) };
      for (const field of fields) site[field] = (source.site as unknown as Record<string, unknown>)[field];
      next.site = site;
      continue;
    }
    if (owner !== tab) continue;
    next[key] = structuredClone((source as unknown as Record<string, unknown>)[key]);
  }
  return next as unknown as AdminConfigValues;
}

/**
 * 保存成功后推进基线：**只**把本页签那一片同步为已保存值。
 *
 * 整份替换会把其它页签里未保存的编辑一起标记成「已保存」——
 * 切回去看着已生效、实际没落库，下一次「还原本页」也回不到真正的服务端状态。
 */
export function mergeTabBaseline(
  tab: ConfigTab,
  form: AdminConfigValues,
  baseline: AdminConfigValues,
): AdminConfigValues {
  return applyTabSlice(tab, baseline, form);
}

/**
 * 保存失败的文案 → 出错页签。
 *
 * 后端 `systemconfig.validate` 只下发中文 message（`ApiError` 没有字段路径），
 * 所以只能两级匹配：
 *   1) 键前缀 —— `checkNum` 会把 JSON key 拼进文案（"retry_times 取值不合法"），
 *      这一步完全由 `CONFIG_KEY_TAB` 派生，不另维护一份键表；
 *   2) 领域文案 —— `checkMicro` 用的是中文标签，键名不在文案里，只能按词匹配。
 * 多处命中或都不命中 → null（跳错页签比不跳更糟）。
 */
const MSG_TAB: ReadonlyArray<readonly [RegExp, ConfigTab]> = [
  [
    /余额上限|最低充值|分组折扣|分组「|结算币种|邀请返利|奖励/,
    "billing",
  ],
  [/支付渠道|支付方式|虎皮椒|易支付|渠道「/, "pay"],
  [/云存储|文件上传|产物保留|存储渠道/, "storage"],
  [/邮件|短信|SMTP/, "notify"],
  [/默认模型|限流|重试|查询截止|渠道监控|探活/, "models"],
  [/公告|站点内容|关于页|用户协议|站点名称|Logo|站点图标|API 调用地址|注册模式|联系方式/, "general"],
  [/退避|失败计数|阶梯|会话|保留天数|保留（天）/, "security"],
];

export function tabOfMessage(msg: string): ConfigTab | null {
  if (!msg) return null;
  for (const [key, tab] of Object.entries(CONFIG_KEY_TAB)) {
    if (msg === key || msg.startsWith(`${key} `) || msg.startsWith(`${key}「`)) return tab;
  }
  const hits = MSG_TAB.filter(([re]) => re.test(msg));
  return hits.length === 1 ? hits[0][1] : null;
}

/** 键序无关的 JSON 序列化：记录型字段（折扣表 / 渠道参数）改键序不算改动。 */
function stableStringify(value: unknown): string {
  return JSON.stringify(sortValue(value));
}

function sortValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortValue);
  if (value && typeof value === "object") {
    const src = value as Record<string, unknown>;
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(src).sort()) out[k] = sortValue(src[k]);
    return out;
  }
  return value;
}
