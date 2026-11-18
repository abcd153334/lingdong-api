/**
 * 应用后台三个表单的纯函数校验。
 *
 * 为什么不用 zod/RHF：管理台现有 CRUD 全部是「受控 state + validate() + 按钮
 * disabled / Field hint」，只有登录注册用了 zod。这里跟齐管理台，避免同一页面
 * 出现两套表单范式。
 *
 * 返回 `Record<字段, 文案>` 而不是单个字符串：错误要显示在对应输入框下面，
 * toast 一句"参数不合法"运营不知道该改哪。
 */

import type { AdminAppBrand } from "@/types";

export type FormErrors = Record<string, string>;

export interface BrandForm {
  displayName: string;
  tagline: string;
  intro: string;
  logoIcon: string;
  contactName: string;
  contactEmail: string;
  supportURL: string;
  privacyURL: string;
  termsURL: string;
}

export interface NoticeForm {
  title: string;
  content: string;
  level: "info" | "warn" | "critical";
  enabled: boolean;
  /** datetime-local 的原始值（本地时间，不带时区） */
  startsAt: string;
  endsAt: string;
}

export interface ReleaseForm {
  version: string;
  releaseNotes: string;
  downloadURL: string;
  forceUpdate: boolean;
}

export interface AppForm {
  slug: string;
  name: string;
  description: string;
}

/** 应用标识的唯一判据，与后端 `appSlugPattern` 逐字一致。 */
export const APP_SLUG_RE = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;

/** 与后端同一条判据：只接受 http(s) 绝对地址。 */
function isHTTPUrl(v: string): boolean {
  if (v === "") return true;
  try {
    const u = new URL(v);
    return u.protocol === "http:" || u.protocol === "https:";
  } catch {
    return false;
  }
}

/** 按字符数（不是 UTF-16 长度）计：与后端 utf8.RuneCount 对齐。 */
export function runeLen(v: string): number {
  return Array.from(v).length;
}

const SEMVER = /^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$/;

/**
 * 新建应用。
 *
 * 标识为什么必须在这里挡：它**创建后不可改**（已写进接入方的部署配置），
 * 错了只能新建一个应用再迁移全部密钥。让一个大小写错误走到后端再报 400，
 * 是把一次可预防的输入错误换成一次迁移成本。
 */
export function validateApp(v: AppForm): FormErrors {
  const e: FormErrors = {};
  const slug = v.slug.trim();
  if (!slug) e.slug = "应用标识必填";
  else if (!APP_SLUG_RE.test(slug)) e.slug = "只能是小写字母、数字与连字符，1-32 位，且不以连字符开头/结尾";
  return { ...e, ...validateAppMeta(v) };
}

/**
 * 名称与说明（创建与编辑共用）。
 *
 * 单独拆出来是因为**改基本信息**时没有标识字段：把 slug 塞进编辑表单
 * 会让人以为能改，而它创建后不可改。
 */
export function validateAppMeta(v: { name: string; description: string }): FormErrors {
  const e: FormErrors = {};
  const name = v.name.trim();
  if (!name) e.name = "名称必填";
  else if (runeLen(name) > 64) e.name = "不超过 64 个字符";
  if (runeLen(v.description.trim()) > 255) e.description = "不超过 255 个字符";
  return e;
}

/** 维护提示文案（后端硬限 200）。 */
export function validateMaintenance(message: string): FormErrors {
  const e: FormErrors = {};
  if (runeLen(message.trim()) > 200) e.message = "不超过 200 个字符";
  return e;
}

export function validateBrand(v: BrandForm): FormErrors {
  const e: FormErrors = {};
  const name = v.displayName.trim();
  if (!name) e.displayName = "展示名称必填";
  else if (runeLen(name) > 64) e.displayName = "不超过 64 个字符";
  if (runeLen(v.tagline.trim()) > 120) e.tagline = "不超过 120 个字符";
  if (runeLen(v.intro.trim()) > 500) e.intro = "不超过 500 个字符";
  if (runeLen(v.contactName.trim()) > 64) e.contactName = "不超过 64 个字符";
  if (runeLen(v.contactEmail.trim()) > 128) e.contactEmail = "不超过 128 个字符";
  else if (v.contactEmail.trim() !== "" && !v.contactEmail.includes("@")) e.contactEmail = "邮箱格式不正确";
  if (!isHTTPUrl(v.supportURL.trim())) e.supportURL = "必须是 http(s) 地址";
  if (!isHTTPUrl(v.privacyURL.trim())) e.privacyURL = "必须是 http(s) 地址";
  if (!isHTTPUrl(v.termsURL.trim())) e.termsURL = "必须是 http(s) 地址";
  // 长度是后端硬限（三条 URL 各 512、图标令牌 64）：先判长度，再判格式，
  // 与后端 NormalizeBrand 的顺序一致，避免"格式对了但被截断"的二次往返。
  for (const k of ["supportURL", "privacyURL", "termsURL"] as const) {
    if (!e[k] && runeLen(v[k].trim()) > 512) e[k] = "链接不超过 512 个字符";
  }
  if (runeLen(v.logoIcon.trim()) > 64) e.logoIcon = "图标标识不合法";
  return e;
}

/**
 * `datetime-local` → RFC3339。
 *
 * 必须由前端补时区：`new Date("2026-11-10T00:00")` 按**浏览器本地时区**解释，
 * 再 toISOString 得到 UTC。把裸字符串直接发给后端，服务器会按自己的时区解读，
 * 容器时区一变整体偏 8 小时。
 */
export function toISO(local: string): string | null {
  if (!local.trim()) return null;
  const d = new Date(local);
  if (Number.isNaN(d.getTime())) return null;
  return d.toISOString();
}

export function validateNotice(v: NoticeForm): FormErrors {
  const e: FormErrors = {};
  if (!v.title.trim()) e.title = "标题必填";
  else if (runeLen(v.title.trim()) > 100) e.title = "不超过 100 个字符";
  if (!v.content.trim()) e.content = "内容必填";
  else if (runeLen(v.content.trim()) > 2000) e.content = "不超过 2000 个字符";
  const start = toISO(v.startsAt);
  const end = toISO(v.endsAt);
  if (v.startsAt.trim() && start === null) e.startsAt = "时间格式不正确";
  if (v.endsAt.trim() && end === null) e.endsAt = "时间格式不正确";
  if (start && end && new Date(end) <= new Date(start)) {
    e.endsAt = "结束时间必须晚于开始时间";
  }
  return e;
}

export function validateRelease(v: ReleaseForm): FormErrors {
  const e: FormErrors = {};
  const version = v.version.trim();
  if (!version) e.version = "版本号必填";
  else if (!SEMVER.test(version)) e.version = "形如 1.2.3（可带 -beta.1）";
  else if (runeLen(version) > 32) e.version = "不超过 32 个字符";
  if (!v.releaseNotes.trim()) e.releaseNotes = "更新说明必填";
  else if (runeLen(v.releaseNotes.trim()) > 4000) e.releaseNotes = "不超过 4000 个字符";
  if (!v.downloadURL.trim()) e.downloadURL = "下载地址必填";
  else if (runeLen(v.downloadURL.trim()) > 1024) e.downloadURL = "下载地址不超过 1024 个字符";
  else if (!isHTTPUrl(v.downloadURL.trim())) e.downloadURL = "必须是 http(s) 地址";
  return e;
}

export function hasError(e: FormErrors): boolean {
  return Object.keys(e).length > 0;
}

/**
 * 品牌接口值 → 表单值。
 *
 * 「初值」与「还原」按钮共用这一份：此前两处各写一遍九个字段，
 * 加一个字段就要改两处，漏一处表现为"还原后该字段仍是脏值"。
 */
export function brandFormOf(b: AdminAppBrand): BrandForm {
  return {
    displayName: b.display_name,
    tagline: b.tagline,
    intro: b.intro,
    logoIcon: b.logo_icon,
    contactName: b.contact_name,
    contactEmail: b.contact_email,
    supportURL: b.support_url,
    privacyURL: b.privacy_url,
    termsURL: b.terms_url,
  };
}

/** 品牌表单是否与服务器值不同（页签「未保存」圆点与丢草稿的判断都靠它）。 */
export function isBrandDirty(form: BrandForm, initial: AdminAppBrand): boolean {
  const base = brandFormOf(initial);
  return (Object.keys(base) as (keyof BrandForm)[]).some((k) => form[k] !== base[k]);
}
