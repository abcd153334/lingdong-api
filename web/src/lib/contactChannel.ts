import { useMemo, useState } from "react";

import { usePortalConfigQuery } from "@/lib/api";
import { maskEmail, maskPhone } from "@/lib/format";
import type { ContactChannel } from "@/types";

/**
 * 邮箱形态：本地部分 + @ + 至少两级域名。
 * 与后端 `verify.ValidateFormat` 同一形态（后端权威判定在服务端）。
 */
const EMAIL_PATTERN = /^[^@\s]+@[^@\s]+\.[^@\s]+$/;

/**
 * 手机号形态：仅中国大陆 11 位。
 *
 * 刻意比后端更严：后端接受 6–15 位纯数字（支持 `+` 前缀的国际号），
 * 前端收紧是为了在用户提交前就拦住明显写错的号码。要支持境外号码时，
 * 只改这一处（并同步后端 `verify.ValidateFormat` 的口径）。
 */
const PHONE_PATTERN = /^1[3-9]\d{9}$/;

/**
 * 联系方式渠道（邮箱 / 短信）的**唯一真相源**。
 *
 * 这个文件存在的理由：渠道的文案、输入属性、正则、默认值、可用性判定原先
 * 散在四个页面里各写一遍（Register / Login 找回抽屉 / Settings 换绑 /
 * 管理台渠道选项），于是：
 *   - 「短信」与「手机号」两个词在不同页面混用；
 *   - 默认渠道被硬编码成 "email"，运营只开短信时注册页直接发给后端一个
 *     策略不允许的渠道（400 bind_method_disabled）；
 *   - 可用性只看策略白名单（contact_channels），不看通道是否真的配好。
 *
 * 现在页面只消费这里导出的 `CHANNEL_META` 与两个 hook，不许再出现
 * `ch === "email" ? …` 这类分支。
 */

/**
 * 联系方式渠道集合（顺序即账户弹窗「账户安全」两行的展示顺序）。
 *
 * 与 `CHANNEL_META` 同源：新增一个渠道时两处一起改，`contactChannel.test.tsx` 有断言守。
 * 这里同时是 `useChannelAvailability` 的遍历源 —— 页面/组件不许再写 `["email","sms"]`。
 */
export const CONTACT_CHANNELS = ["email", "sms"] as const satisfies readonly ContactChannel[];

/** 渠道静态元数据：文案 / 输入属性 / 校验的唯一来源 */
export interface ChannelMeta {
  value: ContactChannel;
  /** 完整称谓：邮箱 / 手机号 */
  label: string;
  /** 短称谓（单选、管理台选项）：邮箱 / 短信 */
  shortLabel: string;
  /** 用户中心行标题：绑定邮箱 / 绑定手机 */
  rowLabel: string;
  /** 找回密码抽屉里的字段名：绑定邮箱 / 绑定手机号 */
  drawerLabel: string;
  inputType: "email" | "tel";
  autoComplete: "email" | "tel";
  /** 新填写的占位符（注册/找回）：you@example.com / 13800138000 */
  placeholder: string;
  /** 找回密码：请输入账号绑定邮箱 / 请输入账号绑定手机号 */
  boundPlaceholder: string;
  /** 换绑：请输入新邮箱 / 请输入新手机号 */
  newPlaceholder: string;
  /** 验证码字段名：邮箱验证码 / 手机号验证码 */
  codeLabel: string;
  pattern: RegExp;
  /** 展示用脱敏（邮箱首字符 + *** + 域名；手机 3+****+4） */
  mask: (value: string) => string;
  formatError: string;
  unchangedError: string;
  /** 通道未就绪时用户中心该行的说明 */
  disabledText: string;
}

export const CHANNEL_META: Record<ContactChannel, ChannelMeta> = {
  email: {
    value: "email",
    label: "邮箱",
    shortLabel: "邮箱",
    rowLabel: "绑定邮箱",
    drawerLabel: "绑定邮箱",
    inputType: "email",
    autoComplete: "email",
    placeholder: "you@example.com",
    boundPlaceholder: "请输入账号绑定邮箱",
    newPlaceholder: "请输入新邮箱",
    codeLabel: "邮箱验证码",
    pattern: EMAIL_PATTERN,
    mask: maskEmail,
    formatError: "邮箱格式不正确",
    unchangedError: "新邮箱与当前一致",
    disabledText: "管理员未开启邮件服务，暂时无法绑定",
  },
  sms: {
    value: "sms",
    label: "手机号",
    shortLabel: "短信",
    rowLabel: "绑定手机",
    drawerLabel: "绑定手机号",
    inputType: "tel",
    autoComplete: "tel",
    placeholder: "13800138000",
    boundPlaceholder: "请输入账号绑定手机号",
    newPlaceholder: "请输入新手机号",
    codeLabel: "手机号验证码",
    pattern: PHONE_PATTERN,
    mask: maskPhone,
    formatError: "手机号格式不正确",
    unchangedError: "新手机号与当前一致",
    disabledText: "管理员未开启短信服务，暂时无法绑定",
  },
};

export function channelMeta(ch: ContactChannel): ChannelMeta {
  return CHANNEL_META[ch];
}

/** 渠道 → 用户资料字段（换绑时用来取"当前绑定值"） */
export function contactOf(ch: ContactChannel, email: string, phone: string): string {
  return ch === "email" ? email : phone;
}

/** 目标格式校验：返回错误文案，合法返回 null */
export function channelFormatError(ch: ContactChannel, target: string): string | null {
  const t = (target ?? "").trim();
  return channelMeta(ch).pattern.test(t) ? null : channelMeta(ch).formatError;
}

/** 换绑目标校验（格式 + 与当前值一致）：返回错误文案，合法返回空串 */
export function bindTargetError(
  ch: ContactChannel,
  current: string,
  target: string,
): string {
  const meta = channelMeta(ch);
  const t = (target ?? "").trim();
  if (!meta.pattern.test(t)) return meta.formatError;
  if (current && t === current) return meta.unchangedError;
  return "";
}

/**
 * 保留用户的选择，非法则回落到首项；空集合返回 null。
 *
 * **不排序**：`channels` 的顺序由后端 `authcfg.channelPriority` 决定
 * （当前 sms 优先于 email），前端再排一次就是第二个真相源。
 * 这也意味着 e2e 桩件必须给出「已排序」的数组，否则测不出优先级。
 */
export function resolveChannel(
  channels: ContactChannel[],
  picked: ContactChannel | null,
): ContactChannel | null {
  if (picked && channels.includes(picked)) return picked;
  return channels[0] ?? null;
}

/**
 * 生效渠道集合（策略 ∩ 就绪），供注册页与找回抽屉使用。
 *
 * 返回值三种状态，页面必须区分：
 *   - `null`：门户配置**还没回来** —— 不得假设任何渠道可用（不要先画一个邮箱框）；
 *   - `[]`：配置已到但没有任何可用渠道（策略未勾选，或勾了但没配好发送服务）；
 *   - 非空：按优先级排序，首项即默认。
 *
 * 请求失败时 `isFetched` 也为 true 且 data 为 undefined → 归入 `[]`：
 * 宁可让注册页显示「暂无可用渠道」，也不给一个必然发不出码的输入框。
 */
export function useAvailableChannels(): ContactChannel[] | null {
  const { data, isFetched } = usePortalConfigQuery();
  if (!isFetched) return null;
  return data?.auth.available_contact_channels ?? [];
}

/**
 * 换绑通道可用性（**仅传输就绪**，与服务端 `BindContact` 的判据一致）。
 *
 * 刻意不用 available_contact_channels：换绑是账号安全操作，后端从不读
 * `auth_register.contact_channels`。用策略集合去判按钮，会把后端允许的
 * 操作在前端关掉（策略只勾邮箱的站点将无法换绑手机，而接口其实是通的）。
 */
export function useChannelAvailability(): {
  channels: ContactChannel[] | null;
  isAvailable: (ch: ContactChannel) => boolean;
} {
  const { data, isFetched } = usePortalConfigQuery();
  const channels: ContactChannel[] | null = isFetched
    ? CONTACT_CHANNELS.filter((ch) =>
        ch === "email" ? Boolean(data?.email_enabled) : Boolean(data?.sms_enabled),
      )
    : null;
  return {
    channels,
    isAvailable: (ch) => channels?.includes(ch) ?? false,
  };
}

export interface ChannelPicker {
  /** null = 配置未返回；见 useAvailableChannels */
  channels: ContactChannel[] | null;
  channel: ContactChannel | null;
  meta: ChannelMeta | null;
  select: (ch: ContactChannel) => void;
  /** 要求验证联系方式，但没有任何可用渠道：页面必须阻断，不能画一张空表单 */
  blocked: boolean;
}

/**
 * 页面级渠道选择器（注册页 / 找回抽屉）。
 *
 * 用**派生**而不是 `useEffect` 收敛：effect 会先渲染一帧旧渠道再纠正，
 * 那一帧恰好是用户可能点到"发送验证码"的窗口。
 *
 * `picked` 是"偏好"不是真相：集合变化时同帧收敛；运营先把 sms 移除再加回，
 * 用户此前的 sms 选择会复活（不持久化，刷新即重置，可接受）。
 */
export function useContactChannelPicker(): ChannelPicker {
  const { data, isFetched } = usePortalConfigQuery();
  const channels = isFetched ? (data?.auth.available_contact_channels ?? []) : null;
  const [picked, setPicked] = useState<ContactChannel | null>(null);

  const channel = useMemo(() => resolveChannel(channels ?? [], picked), [channels, picked]);
  const meta = channel ? channelMeta(channel) : null;
  const blocked =
    channels !== null && channels.length === 0 && data?.auth.contact_verify === "required";

  return { channels, channel, meta, select: setPicked, blocked };
}
