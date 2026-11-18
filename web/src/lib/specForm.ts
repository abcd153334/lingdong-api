/**
 * 模型价格表单 ↔ `spec_prices` 的唯一转换实现。
 *
 * 为什么单独成模块（而不是留在页面组件里）：
 *  1. 转换逻辑必须是**纯函数**，否则「读」和「写」没法放在一起做往返测试；
 *  2. 历史事故正是往返不对称：`specToForm` 按原值读（`String(sp.max_n)`），
 *     `buildSpec` 按金额写（`microOf` ×1e6），往返一次 4 变 4000000，
 *     图片模型每次保存都被后端 `1–128` 拒绝；
 *  3. 组件里的 toast 属于表现层，不该参与"这个值合不合法"的判定。
 *
 * 现在两条路径共用 `SPEC_FIELDS` 这一张「量纲登记表」：
 * 同一个字段的读法与写法由同一条规则推导，结构上不可能再错位。
 */

import { microOf, microToInput } from "@/lib/money";
import {
  MAX_IMAGE_N,
  MAX_PRICE_SECONDS,
  countOfInput,
  ratioOfInput,
  type Parsed,
} from "@/lib/quantity";
import type { Modality, SpecPrices } from "@/types";

export type SpecKind = "per_image" | "per_video" | "per_task" | "per_second" | "per_clip";

/* ---------------------------- 量纲登记表 ---------------------------- */

type SpecFieldRule =
  | { kind: "money"; label: string }
  | { kind: "count"; label: string; min: number; max: number }
  | { kind: "ratio"; label: string; min: number; max: number };

const SPEC_FIELDS = {
  // 音乐/歌词按**实际交付条数**结算；per_task_price 是存量口径，两栏都留着，
  // 由 kind 决定哪一栏生效（后端拒绝两种口径同时非空）。
  per_clip_price: { kind: "money", label: "音乐每条单价" },
  per_task_price: { kind: "money", label: "音乐按次单价（存量）" },
  per_second_price: { kind: "money", label: "每秒单价" },
  min_price: { kind: "money", label: "最低消费" },
  ref_image_price: { kind: "money", label: "参考图单价" },
  ref_audio_price: { kind: "money", label: "参考音频单价" },
  ref_video_price: { kind: "money", label: "参考视频单价" },

  // —— 以下是**数量**，不是金额：一律不换算 ——
  // 图片是张数上限（≤128）；按条计费时是单次任务的计费条数（≤4，见后端 MaxMusicClips）。
  // 上限取宽的那个，窄的那个由后端按 kind 校验并给出具体文案。
  max_n: { kind: "count", label: "单次产出上限", min: 1, max: MAX_IMAGE_N },
  max_seconds: { kind: "count", label: "最大秒数", min: 1, max: MAX_PRICE_SECONDS },
  ref_image_free: { kind: "count", label: "参考图免费张数", min: 0, max: MAX_IMAGE_N },
  long_after_seconds: { kind: "count", label: "超时阈值", min: 1, max: MAX_PRICE_SECONDS },

  // —— 倍率：存**微比例**（1.5 倍 → 1500000），也不是金额 ——
  long_video_multiplier: { kind: "ratio", label: "超时倍率", min: 1, max: 10 },
} satisfies Record<string, SpecFieldRule>;

type SpecScalar = keyof typeof SPEC_FIELDS;

/** 表单字符串 → 数值（微 / 原值 / 微比例）。三态：留空 / 非法 / 合法。 */
export function readSpecField(name: SpecScalar, raw: string): Parsed<number | null> {
  const rule: SpecFieldRule = SPEC_FIELDS[name];
  if (rule.kind === "count") return countOfInput(raw, rule);
  if (rule.kind === "ratio") return ratioOfInput(raw, rule);
  const t = raw.trim();
  if (t === "") return { ok: true, value: null };
  const v = microOf(t);
  if (v === null) {
    return { ok: false, error: `${rule.label}的金额格式不正确（只能填数字，最多 6 位小数）` };
  }
  return { ok: true, value: v };
}

/** 库内值 → 表单字符串（与 readSpecField 严格互逆）。 */
function specFieldToInput(name: SpecScalar, sp: SpecPrices): string {
  const rule: SpecFieldRule = SPEC_FIELDS[name];
  const v = sp[name as keyof SpecPrices];
  if (v === null || v === undefined) return "";
  if (rule.kind === "money" || rule.kind === "ratio") return microToInput(v as number);
  return String(v);
}

/* ---------------------------- 表单结构 ---------------------------- */

/** 规格价表单（全部是字符串：金额输入框与"未配置"必须能共存） */
export interface SpecForm {
  kind: SpecKind;
  tier_price: Record<string, string>;
  max_n: string;
  second_price: Record<string, string>;
  max_seconds: string;
  resolution_fixed: string;
  long_after_seconds: string;
  /** 倍率按"倍数"填（1.5），提交时转成微比例 */
  long_video_multiplier: string;
  ref_image_price: string;
  ref_image_free: string;
  ref_audio_price: string;
  ref_video_price: string;
  ref_video_second_price: Record<string, string>;
  per_clip_price: string;
  per_task_price: string;
  per_second_price: string;
  /** 最低消费（微 → 表单里填金额） */
  min_price: string;
  /**
   * 允许智能时长（`"true"` / `""`）。
   *
   * 为什么也是字符串而不是 boolean：`setSpecField(key, raw: string)` 是全页
   * 唯一的字段写入口，布尔单独走一套会让「表单 → 提交」出现第二条路径。
   * 真值判定只在 `buildSpecPrices` 一处：`=== "true"`。
   */
  allow_auto_duration: string;
}

export function emptySpec(kind: SpecKind): SpecForm {
  return {
    kind,
    tier_price: {},
    // 图片的 max_n **默认为空**，不给可见的「4」。
    //
    // 曾经的默认值是 "4"，理由是「给运营一个合理的起点」。它错在两个地方：
    //  1. 组图（sequential_image_generation，最多 15 张）与图层拆分
    //     （1 张底图 + 16 张图层 = 17 张）都需要 max_n ≥ 实际张数，
    //     而 4 会先把它们卡死，表现为上游 400 / 本地 invalid_tier；
    //  2. 「4」看起来像一个业务决策，实际只是一个没人验证过的数字 ——
    //     运营不会去动它，于是一个随手写的默认值变成了所有新模型的收费上限。
    // 留空 = 不提交该字段，后端回落到引擎上限（MAX_IMAGE_N），这才是正确的默认。
    max_n: "",
    second_price: {},
    max_seconds: "",
    resolution_fixed: "",
    long_after_seconds: "",
    long_video_multiplier: "",
    ref_image_price: "",
    ref_image_free: "",
    ref_audio_price: "",
    ref_video_price: "",
    ref_video_second_price: {},
    per_clip_price: "",
    per_task_price: "",
    per_second_price: "",
    min_price: "",
    allow_auto_duration: "",
  };
}

/** 微价目表 → 字符串映射（只保留有值的档位） */
export function tierToForm(map: Record<string, number> | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(map ?? {})) {
    if (typeof v === "number" && Number.isFinite(v)) out[k] = microToInput(v);
  }
  return out;
}

export function specToForm(sp: SpecPrices | null | undefined, kind: SpecKind): SpecForm {
  if (!sp) return emptySpec(kind);
  const k = (sp.kind || kind) as SpecKind;
  return {
    kind: k,
    tier_price: tierToForm(sp.tier_price),
    second_price: tierToForm(sp.second_price),
    ref_video_second_price: tierToForm(sp.ref_video_second_price),
    resolution_fixed: sp.resolution_fixed ?? "",
    max_n: specFieldToInput("max_n", sp),
    max_seconds: specFieldToInput("max_seconds", sp),
    long_after_seconds: specFieldToInput("long_after_seconds", sp),
    long_video_multiplier: specFieldToInput("long_video_multiplier", sp),
    ref_image_price: specFieldToInput("ref_image_price", sp),
    ref_image_free: specFieldToInput("ref_image_free", sp),
    ref_audio_price: specFieldToInput("ref_audio_price", sp),
    ref_video_price: specFieldToInput("ref_video_price", sp),
    per_clip_price: specFieldToInput("per_clip_price", sp),
    per_task_price: specFieldToInput("per_task_price", sp),
    per_second_price: specFieldToInput("per_second_price", sp),
    min_price: specFieldToInput("min_price", sp),
    // 布尔不在量纲登记表里（没有单位、没有"留空 vs 0"）：
    // 库内 null / false / 缺失一律显示为关，只有显式 true 才是"true"。
    allow_auto_duration: sp.allow_auto_duration === true ? "true" : "",
  };
}

/** 数值字段的回显：0/缺失一律显示为空（0 在约束语义里是"不限"）。 */
function numOrEmpty(v: number | null | undefined): string {
  if (v === null || v === undefined || v === 0) return "";
  return String(v);
}

/* ---------------------------- 金额输入组 ---------------------------- */

/**
 * 解析一组「金额输入框」，返回微。
 *
 * 成组解析而不是逐个 `readSpecField`：档位表要一次把 N 个格子检查完、
 * 只报第一个错，否则运营得点 N 次保存才知道哪几格错了。
 */
export function parseMoneyFields(
  fields: { key: string; label: string; raw: string }[],
): { ok: true; values: Record<string, number | null> } | { ok: false; error: string } {
  const values: Record<string, number | null> = {};
  for (const f of fields) {
    const t = f.raw.trim();
    if (t === "") {
      values[f.key] = null;
      continue;
    }
    const v = microOf(t);
    if (v === null) {
      return { ok: false, error: `${f.label}的金额格式不正确（只能填数字，最多 6 位小数）` };
    }
    values[f.key] = v;
  }
  return { ok: true, values };
}

/** 档位单价表 → 微映射（空档位剔除、非法金额报错）。 */
function moneyMap(raw: Record<string, string>, label: string): Parsed<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const [tier, text] of Object.entries(raw)) {
    const t = text.trim();
    if (t === "") continue;
    const v = microOf(t);
    if (v === null) {
      return { ok: false, error: `${label}「${tier}」的单价「${t}」不是合法金额` };
    }
    out[tier] = v;
  }
  return { ok: true, value: out };
}

/**
 * 价目是否「全部为 0」= 完全免费。
 *
 * 0 在规格计费里是**合法**配置（免费档 / 免费模型），但它也可能是「把元当微」或
 * 「清空后顺手填了 0」的结果，而后果是收入静默归零 —— 这种错误在账单上完全看不出来。
 *
 * 判据必须是**全量**：只要还有一个非 0 价（含 `min_price` 这道地板），就不是免费。
 * 只看主价目（`tier_price` / `second_price`）会漏掉「档位全 0 但最低消费 0.01 元」，
 * 那时确认框说的话与事实不符。
 */
export function specIsFree(sp: SpecPrices): boolean {
  const vals: number[] = [];
  const push = (v: number | null | undefined) => {
    if (v !== null && v !== undefined) vals.push(v);
  };
  for (const v of Object.values(sp.tier_price ?? {})) push(v);
  for (const v of Object.values(sp.second_price ?? {})) push(v);
  for (const v of Object.values(sp.ref_video_second_price ?? {})) push(v);
  push(sp.per_task_price);
  push(sp.per_second_price);
  push(sp.min_price);
  push(sp.ref_image_price);
  push(sp.ref_video_price);
  push(sp.ref_audio_price);
  return vals.length > 0 && vals.every((v) => v === 0);
}

/* ---------------------------- 组装 ---------------------------- */

/**
 * 表单 → `spec_prices`。纯函数，失败时返回可展示的中文原因。
 *
 * 逐键**显式构造**而不是 `{...form.spec}`：后端 `SpecPrices` 是严格解码
 * （`DisallowUnknownFields`），多带一个与 kind 无关的键就会被 400
 * （例如给视频模型带 `tier_price`）。
 */
export function buildSpecPrices(spec: SpecForm, modality: Modality): Parsed<SpecPrices> {
  // 音频按秒：kind=per_second。单价必填，最低消费可选（上游"¥0.001/秒，最低 ¥0.01"）。
  // 按次的模型遇到长短文本混用时无法定价（5 秒广告语与 10 分钟有声书同价），
  // 所以音频必须有按秒这一档。
  if (modality === "audio") {
    const perSecond = parseMoneyFields([
      { key: "per_second_price", label: "每秒单价", raw: spec.per_second_price },
      { key: "min_price", label: "最低消费", raw: spec.min_price },
    ]);
    if (!perSecond.ok) return perSecond;
    const unit = perSecond.values.per_second_price;
    if (unit === null) {
      return { ok: false, error: "音频按秒必须填写每秒单价" };
    }
    // 0 价是合法配置（显式免费，与 token 的 0 价同一语义）：后端 perSecondCharge
    // 允许 0 元，前端不该比后端更严 —— 否则「免费音频模型」永远配不出来。
    const minPrice = perSecond.values.min_price;
    if (minPrice !== null && minPrice < 0) {
      return { ok: false, error: "最低消费不能为负数（不需要最低消费就留空）" };
    }
    const maxSeconds = readSpecField("max_seconds", spec.max_seconds);
    if (!maxSeconds.ok) return maxSeconds;
    // 必填：它是单次音频的**最坏金额**，也是结算期的金额上限。
    // 留空时后端会拒绝保存 —— 前端必须给出同一句话，否则运营只能看到 400。
    if (maxSeconds.value === null) {
      return { ok: false, error: "音频按秒必须填写最大秒数（单次最坏金额与结算上限由它决定）" };
    }
    const out: SpecPrices = { kind: "per_second", per_second_price: unit };
    // 0 与"留空"同义（都不收最低消费）：落 0 会让库里存一个没有含义的价目字段，
    // 对账时还得多解释一次"0 表示不限制"。
    if (minPrice !== null && minPrice > 0) out.min_price = minPrice;
    out.max_seconds = maxSeconds.value;
    return { ok: true, value: out };
  }

  // 音乐/歌词：kind=per_clip，**按实际交付条数**结算（一次任务出 1~2 首）。
  // 必须在 video 分支之前判：music 落不到 video 的 seconds 矩阵，也不会命中 image 的 max_n。
  //
  // per_task_price 只作存量回显/保存：老模型的价目还是那个口径，界面上留一栏让它
  // 原样存回，否则运营打开旧模型点保存就会把价目清空（后端随后按"未配价"拒绝调用）。
  if (modality === "music") {
    const clip = readSpecField("per_clip_price", spec.per_clip_price);
    if (!clip.ok) return clip;
    const legacy = readSpecField("per_task_price", spec.per_task_price);
    if (!legacy.ok) return legacy;
    if (clip.value === null && legacy.value === null) {
      return { ok: false, error: "音乐必须配置每条单价（结算按实际交付条数）" };
    }
    // 0 价是合法配置：后端判的是 `< 0`（未配置才是 nil），显式 0 = 免费。
    // 前端拦 0 会让「免费音乐模型」永远配不出来。
    if (clip.value === null) {
      return { ok: true, value: { kind: "per_task", per_task_price: legacy.value as number } };
    }
    const out: SpecPrices = { kind: "per_clip", per_clip_price: clip.value };
    const maxN = readSpecField("max_n", spec.max_n);
    if (!maxN.ok) return maxN;
    if (maxN.value !== null) {
      // 与后端 MaxMusicClips 同一数字：超过 4 条没有任何音乐模型会用到，
      // 而配大了会让一次冻结把用户余额整段占住。
      if (maxN.value > 4) {
        return { ok: false, error: "音乐的计费条数上限最多 4 条（一次任务通常出 1~2 首）" };
      }
      out.max_n = maxN.value;
    }
    return { ok: true, value: out };
  }

  if (modality === "video") {
    // 语义（与后端一致）：**配了哪些档 = 该模型开放哪些档**，不强求三档齐全。
    // 强制齐全会逼运营给上游根本不支持的档位填假价，调用方真的传了那个档位就是
    // 「冻结成功 → 适配器 400 → 退款」的反复抖动。
    const second = moneyMap(spec.second_price, "视频按秒");
    if (!second.ok) return second;
    if (Object.keys(second.value).length === 0) {
      return {
        ok: false,
        error: "视频按秒至少配置一个档位的每秒单价（配了哪些档就代表开放哪些档）",
      };
    }

    // 参考视频每秒价：只保留填了的档位；一档都没有就等于「仍按条计费」。
    const refSecond = moneyMap(spec.ref_video_second_price, "参考视频每秒");
    if (!refSecond.ok) return refSecond;
    const hasRefSecond = Object.keys(refSecond.value).length > 0;

    const after = readSpecField("long_after_seconds", spec.long_after_seconds);
    if (!after.ok) return after;
    const mult = readSpecField("long_video_multiplier", spec.long_video_multiplier);
    if (!mult.ok) return mult;
    if ((after.value === null) !== (mult.value === null)) {
      return { ok: false, error: "超时阈值与超时倍率必须同时填写（只填一个不会生效）" };
    }
    if (mult.value !== null && mult.value <= 1_000_000) {
      return { ok: false, error: "超时倍率必须大于 1（等于 1 表示不加价，请直接留空）" };
    }

    const prices = parseMoneyFields([
      { key: "ref_image_price", label: "参考图加价", raw: spec.ref_image_price },
      { key: "ref_video_price", label: "参考视频加价", raw: spec.ref_video_price },
      { key: "ref_audio_price", label: "参考音频加价", raw: spec.ref_audio_price },
    ]);
    if (!prices.ok) return prices;

    const out: SpecPrices = { kind: "per_video", second_price: second.value };
    // 固定档位必须原样带回：它是最容易在重建里丢掉的一个键，漏了就是「改一次显示名
    // → 该模型所有不传 resolution 的请求全部 400」。且必须命中秒价表 ——
    // 指向一个没有单价的档位，等于冻结成功但结算每轮 ErrInvalidTier。
    const fixedTier = spec.resolution_fixed.trim();
    if (fixedTier) {
      if (second.value[fixedTier] === undefined) {
        return {
          ok: false,
          error: `固定档位 ${fixedTier} 没有配置每秒单价：请先在上面的价目表里填上，否则不传 resolution 的请求无法计价`,
        };
      }
      out.resolution_fixed = fixedTier;
    }
    // 新旧两种参考视频口径互斥（后端保存期也拒绝同时非空）：
    // 切换时另一条必须**整个不出现**，不能只"不动它"。
    if (hasRefSecond) {
      out.ref_video_second_price = refSecond.value;
    } else if (prices.values.ref_video_price !== null) {
      out.ref_video_price = prices.values.ref_video_price;
    }
    if (prices.values.ref_image_price !== null) out.ref_image_price = prices.values.ref_image_price;
    if (prices.values.ref_audio_price !== null) out.ref_audio_price = prices.values.ref_audio_price;

    // 参考图免费张数：空 = 不免费；0 也是"不免费"，别把 0 当成"全免费"。
    // 上界由登记表兜住（0–MaxImageN）：没有上界时一次单位错就能把 2 存成 200 万。
    const free = readSpecField("ref_image_free", spec.ref_image_free);
    if (!free.ok) return free;
    if (free.value !== null) out.ref_image_free = free.value;

    const maxSec = readSpecField("max_seconds", spec.max_seconds);
    if (!maxSec.ok) return maxSec;
    // 必填：单条视频的最坏金额 = 每秒单价 × 它，结算上限也用它。
    if (maxSec.value === null) {
      return { ok: false, error: "视频按秒必须填写最大秒数（单条视频的最坏金额与结算上限由它决定）" };
    }
    out.max_seconds = maxSec.value;

    if (after.value !== null && mult.value !== null) {
      out.long_after_seconds = after.value;
      out.long_video_multiplier = mult.value;
    }
    // 智能时长开关（布尔）。
    //
    // `spec_prices` 是**一整列 JSON 覆盖写**，不是逐键 patch：不写这个键就等于
    // 关掉它。所以「运营刚打开开关 → 改个显示名 → 保存」若不带上它，开关会静默
    // 复位成关，而页面此时已经不报任何错（下一条 `seconds=-1` 才 400）。
    if (spec.allow_auto_duration === "true") out.allow_auto_duration = true;
    return { ok: true, value: out };
  }

  // 图像（按张）
  const tier = moneyMap(spec.tier_price, "图片按张");
  if (!tier.ok) return tier;
  if (Object.keys(tier.value).length === 0) {
    return { ok: false, error: "图片按张至少配置一个分辨率单价" };
  }
  // 默认档（resolution_fixed）是「不传 size / auto」时的计费档。
  //
  // 只校验「非空时必须命中已配档位」——与后端保存期同一条判据。
  // 以前这里硬拦「默认价必须等于某个档位单价」，而默认档是靠价格反推的：
  // 改一次档价就会悄悄换掉默认档，运营完全看不见。现在默认档就是它自己。
  const defTier = spec.resolution_fixed.trim();
  if (defTier && tier.value[defTier] === undefined) {
    return {
      ok: false,
      error: `默认档「${defTier}」不在已配置的档位里（${Object.keys(tier.value).join(" / ")}）`,
    };
  }

  // 张数上限必填：它是「一次调用最多收多少张的钱」的唯一依据，也是结算上限。
  const nMax = readSpecField("max_n", spec.max_n);
  if (!nMax.ok) return nMax;
  if (nMax.value === null) {
    return { ok: false, error: "图片按张必须填写单次张数上限（单次最坏金额与结算上限由它决定）" };
  }

  const out: SpecPrices = { kind: "per_image", tier_price: tier.value };
  if (defTier) out.resolution_fixed = defTier;
  out.max_n = nMax.value;
  return { ok: true, value: out };
}


/**
 * 档位清晰度权重：`2K`→2、`1.5K`→1.5；认不出的键返回 0（排最后）。
 *
 * 与后端 `billing.ImageTierRank`（按面积 = (N×1024)²）同一套规则。
 * 前端曾经用它自己的词表下标排序，于是自定义档位（`8K` 不在词表里）
 * `indexOf` 返回 -1，被排成"最低档" —— 与后端结论正好相反。
 */
export function tierRank(tier: string): number {
  const m = /^(\d+(?:\.\d+)?)K$/i.exec(tier.trim());
  if (!m) return 0;
  const n = Number(m[1]);
  return Number.isFinite(n) && n > 0 ? n : 0;
}
