import type { ReactNode } from "react";

import { billingModeLabel } from "@/lib/format";
import { microToInput, moneyOf } from "@/lib/money";
import { emptySpec, specToForm, type SpecForm, type SpecKind } from "@/lib/specForm";
import type { AdminModelItem, AdminModelPayload, BillingMode, Modality } from "@/types";
import type { TagFormRow } from "@/components/admin/ModelTagsField";

/**
 * 模型表单的纯逻辑（无 JSX 依赖的部分）。
 *
 * 与旧模型页的区别只有一个：从页面文件里搬出来给 `ModelDialog` 与右栏列表共用，
 * 避免"列表里的价目概要和弹窗里的价目矩阵各写一份"。
 */

/** 价格通道定义（与后端 laneCN 逐条同序同名） */
export const PRICE_LANES: { key: PriceLane; label: string; hint?: string }[] = [
  { key: "input", label: "输入价" },
  { key: "output", label: "输出价", hint: "留空 = 与输入同价" },
  { key: "cache", label: "缓存读取价", hint: "留空 = 与输入同价" },
  { key: "cache_create", label: "缓存写入价", hint: "留空 = 与输入同价" },
  { key: "cache_create_1h", label: "1h 缓存写入价", hint: "留空 = 按 5m 档计价" },
  { key: "image", label: "图片输入价", hint: "留空 = 与输入同价" },
  { key: "audio", label: "音频输入价", hint: "留空 = 与输入同价" },
  { key: "audio_output", label: "音频输出价", hint: "留空 = 与输入同价" },
];

export type PriceLane =
  | "input"
  | "output"
  | "cache"
  | "cache_create"
  | "cache_create_1h"
  | "image"
  | "audio"
  | "audio_output"
  | "call";

/**
 * 模态 → 可选计费方式（唯一真相源是后端 schema.modalityRules）。
 *
 * 不在这里写一份"看起来合理"的表：`audio` 不能按量（响应是二进制，
 * 解析不出 token），但可以按次或按秒；图像/视频/音乐只能按规格 ——
 * 前端多给一个选项就是"能选中、保存被拒"。
 */
export function billingModeOptions(m: Modality): [BillingMode, string][] {
  if (m === "image") return [["spec", "图片按张"]];
  if (m === "video") return [["spec", "视频按秒"]];
  if (m === "music") return [["spec", "音乐按次"]];
  if (m === "audio")
    return [
      ["call", "按次"],
      ["spec", "语音按秒"],
    ];
  return [
    ["tokens", "按用量（Token）"],
    ["call", "按次"],
  ];
}

/** 模态 → 该模态应使用的规格价类型（切换模态时的默认结构） */
export function modalitySpecKind(m: Modality): SpecKind {
  if (m === "video") return "per_video";
  // 音乐/歌词按实际交付条数计费（per_clip）；per_task 是存量口径，只在旧模型上回显。
  if (m === "music") return "per_clip";
  if (m === "audio") return "per_second";
  return "per_image";
}

export interface ModelForm {
  id: string;
  display_name: string;
  /**
   * 「自己设的图标」对应的可渲染地址（来自模型行；空 = 没有自己的图标）。
   *
   * 不能用行上的 `icon_url` 无条件填：那是**派生后**的展示地址（可能来自供应商），
   * 填进来会让表单在 `icon_own` 为空时也画一张图，运营会以为模型自己设过。
   */
  iconUrl: string;
  /**
   * 模型**自己设的**图标 token（`""` = 未单独设置，沿用供应商图标）。
   *
   * 存 `icon_own` 而不是展示值 `icon`：展示值是"自己设的 or 供应商的"派生结果，
   * 拿它当表单初值就等于把继承关系固化成自定义 —— 之后供应商换图标它不再跟随，
   * 而运营根本没做过这个决定。
   */
  icon_own: string;
  /** 一句话说明（文档组成员表的默认文案）；空串 = 清空 */
  description: string;
  vendor: string;
  modality: Modality;
  billing_mode: BillingMode;
  enabled: boolean;
  /** 9 条 lane 的金额输入（键与后端 laneCN 同名） */
  prices: Record<PriceLane, string>;
  spec: SpecForm;
  /**
   * 模型级窗口覆盖（原始输入，空串 = 继承助手全局设置）。
   *
   * 存字符串而不是 number：空串与 0 在界面上必须可区分 ——
   * 空 = "没填（继承）"，0 也等于继承，但用户把 128000 删空后
   * 如果被强行回填成 0，他会以为保存失败。
   */
  context_window: string;
  max_output_tokens: string;
  /**
   * 营销标签（数组顺序即展示顺序）。
   *
   * 存表单行而不是后端形状：`until` 在表单里是**日期**（后端按业务时区解释成当日末刻），
   * 直接拿 RFC3339 填进日期输入框会显示空值。
   */
  tags: TagFormRow[];
}

/** 空串 → 0（= 继承）；非法输入返回 NaN，由提交前的校验拦下。 */
export function intOrZero(raw: string): number {
  const t = raw.trim();
  if (t === "") return 0;
  const n = Number(t);
  return Number.isFinite(n) ? Math.trunc(n) : Number.NaN;
}

export function emptyPrices(): Record<PriceLane, string> {
  return {
    input: "",
    output: "",
    cache: "",
    cache_create: "",
    cache_create_1h: "",
    image: "",
    audio: "",
    audio_output: "",
    call: "",
  };
}

export function emptyForm(vendorId: string): ModelForm {
  return {
    id: "",
    display_name: "",
    icon_own: "",
    iconUrl: "",
    description: "",
    vendor: vendorId,
    modality: "chat",
    billing_mode: "tokens",
    enabled: true,
    prices: emptyPrices(),
    spec: emptySpec("per_image"),
    context_window: "",
    max_output_tokens: "",
    tags: [],
  };
}

export function formFromModel(m: AdminModelItem): ModelForm {
  return {
    id: m.id,
    display_name: m.display_name ?? "",
    icon_own: m.icon_own ?? "",
    iconUrl: m.icon_own ? (m.icon_url ?? "") : "",
    description: m.description ?? "",
    vendor: m.vendor,
    modality: m.modality,
    billing_mode: m.billing_mode,
    enabled: m.enabled,
    prices: {
      input: microToInput(m.prices?.input ?? undefined),
      output: microToInput(m.prices?.output ?? undefined),
      cache: microToInput(m.prices?.cache ?? undefined),
      cache_create: microToInput(m.prices?.cache_create ?? undefined),
      cache_create_1h: microToInput(m.prices?.cache_create_1h ?? undefined),
      image: microToInput(m.prices?.image ?? undefined),
      audio: microToInput(m.prices?.audio ?? undefined),
      audio_output: microToInput(m.prices?.audio_output ?? undefined),
      call: microToInput(m.prices?.call ?? undefined),
    },
    spec: specToForm(m.spec_prices, modalitySpecKind(m.modality)),
    // 0 = 继承 → 显示成空（让人看到"没填"，而不是一个会误导的 0）
    context_window: m.context_window ? String(m.context_window) : "",
    max_output_tokens: m.max_output_tokens ? String(m.max_output_tokens) : "",
    // 回填用 until_date（业务时区日期）而不是 until（UTC 时刻）：日期输入框只认 YYYY-MM-DD。
    tags: (m.tags ?? []).map((t) => ({
      label: t.label,
      color: t.color,
      untilDate: t.until_date ?? "",
    })),
  };
}

/** 列表里的价目概要：只给"按什么算 + 代表价"，细节在编辑弹窗 */
export function priceSummary(m: AdminModelItem): ReactNode {
  if (m.billing_mode === "tokens") {
    if (m.prices?.input == null) return <span className="muted small">未配置</span>;
    return (
      <span className="mono small">
        入 {moneyOf(m.prices.input)}
        {m.prices.output != null ? ` · 出 ${moneyOf(m.prices.output)}` : ""} / 1M
      </span>
    );
  }
  if (m.billing_mode === "call") {
    return m.prices?.call == null ? (
      <span className="muted small">未配置</span>
    ) : (
      <span className="mono small">{moneyOf(m.prices.call)} / 次</span>
    );
  }
  const sp = m.spec_prices;
  if (!sp) return <span className="muted small">未配置</span>;
  if (sp.kind === "per_clip") {
    // 按条计费：必须把「每条多少钱」与「最多几条」一起给 —— 只显示单价会让运营
    // 以为一次任务最多收一条的钱，而冻结期是按 max_n 占额度的。
    if (sp.per_clip_price == null) return <span className="muted small">未配置</span>;
    const cap = sp.max_n != null ? ` · 最多 ${sp.max_n} 条` : " · 最多 2 条";
    return (
      <span className="mono small">
        {billingModeLabel(m)} · {moneyOf(sp.per_clip_price)} / 条{cap}
      </span>
    );
  }
  if (sp.kind === "per_task") {
    // 存量口径：音乐/歌词单一定价，直接给出金额：列表里只看"音乐按次"无法判断配没配价，
    // 而缺价的音乐模型是**能建出来但一调就 400**。
    return sp.per_task_price == null ? (
      <span className="muted small">未配置</span>
    ) : (
      <span className="mono small">
        {billingModeLabel(m)} · {moneyOf(sp.per_task_price)}
      </span>
    );
  }
  if (sp.kind === "per_second") {
    // 音频按秒：单价 + 最低消费一起给 —— 只显示单价会让运营以为短文本也按秒实收。
    if (sp.per_second_price == null) return <span className="muted small">未配置</span>;
    return (
      <span className="mono small">
        {billingModeLabel(m)} · {moneyOf(sp.per_second_price)} / 秒
        {sp.min_price != null ? ` · 最低 ${moneyOf(sp.min_price)}` : ""}
      </span>
    );
  }
  if (sp.kind === "per_video") {
    const tiers = Object.entries(sp.second_price ?? {});
    if (!tiers.length) return <span className="muted small">未配置</span>;
    const [tier, price] = tiers.sort((a, b) => a[1] - b[1])[0];
    return (
      <span className="mono small">
        {billingModeLabel(m)} · {tier} {moneyOf(price)} / s
      </span>
    );
  }
  const tiers = Object.entries(sp.tier_price ?? {});
  if (!tiers.length) return <span className="muted small">未配置</span>;
  const [tier, price] = tiers.sort((a, b) => a[1] - b[1])[0];
  return (
    <span className="mono small">
      {billingModeLabel(m)} · {tier} {moneyOf(price)} / 张
    </span>
  );
}

/** 价格通道名 → 请求体字段名（**逐条显式列出**）。 */
export function assignPrice(body: AdminModelPayload, lane: PriceLane, v: number | null): void {
  switch (lane) {
    case "input":
      body.input_price = v;
      break;
    case "output":
      body.output_price = v;
      break;
    case "cache":
      body.cache_price = v;
      break;
    case "cache_create":
      body.cache_create_price = v;
      break;
    case "cache_create_1h":
      body.cache_create_price_1h = v;
      break;
    case "image":
      body.image_price = v;
      break;
    case "audio":
      body.audio_price = v;
      break;
    case "audio_output":
      body.audio_output_price = v;
      break;
    case "call":
      body.call_price = v;
      break;
  }
}
