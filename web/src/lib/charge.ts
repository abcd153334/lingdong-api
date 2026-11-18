import { formatMoney, formatNum } from "@/lib/format";
import type { PortalCallDetail } from "@/types";

/**
 * 扣费解释（调用明细详情用）。
 *
 * 为什么单独一个模块：金额解释是**纯函数 + 一张术语表**，
 * 混在组件里会变成十几层三元表达式，而且没法单测。
 *
 * 结论句只解释「按什么算、算了多少的量」，金额一律由 `QuotaUnit` 渲染：
 * 单价是**微**，直接拼进文案就要自己处理单位与符号，两处各拼一次必然不一致。
 * 需要展示单价时用 formatMoney，它读的是下发的 scale。
 */

/** 计费方式（与后端 models.billing_mode 同值）。 */
export type BillingMode = "tokens" | "call" | "spec";

/** 明细行（只含参与计算的项，全零项由调用方过滤）。 */
export interface ChargeRow {
  label: string;
  value: string;
  muted?: boolean;
}

const CALL_KIND_LABEL: Record<string, string> = {
  sync: "同步",
  async: "异步任务",
  precheck: "预检",
};

/**
 * 计费来源文案（**仅管理台**）。
 *
 * 门户不渲染：`upstream` 这个值本身就在说"这条走的是服务商实测用量"，
 * 属于平台计费链路细节。门户只用 `billing_mode` 解释"按什么算"。
 */
const SOURCE_LABEL: Record<string, string> = {
  upstream: "上游实测",
  mixed: "上游 + 估算补齐",
  estimated: "请求侧估算",
  none: "按规格（不使用 token）",
};

const TIER_SOURCE_LABEL: Record<string, string> = {
  fixed: "固定价",
  resolution: "规格档位",
  default: "默认档",
  highest: "最高价档",
  per_task: "按次",
  per_clip: "按条",
};

/** 截顶原因文案（**仅管理台**）：门户只说"已按单次上限截顶"，不暴露风控档位。 */
const CAP_REASON_LABEL: Record<string, string> = {
  global: "全局上限",
  frozen3x: "冻结额 3 倍",
  balance: "余额下限",
};

/**
 * 异常标记文案。
 *
 * 必须覆盖 `backend/internal/model/anomaly.go` 里的**全部**常量：
 * 缺一个，用户就会在账单里看到 `image_n_capped` 这种英文码。
 * 这条约束由 `src/test/units.test.ts` 直接读后端源码比对，不靠人记。
 *
 * 门户只会收到 `reclaimed` 等少数标记，其余是平台风控内部状态；
 * 保留全量映射是因为管理台会看到全部。
 */
const ANOMALY_LABEL: Record<string, string> = {
  charge_capped: "金额已截顶",
  negative_balance_blocked: "余额不足已拦截",
  cache_overlap_clamped: "缓存重叠已修正",
  negative_input: "输入为负已修正",
  reclaimed: "回收补账",
  reclaimed_after_success: "任务已交付但未结算，已补结",
  reclaimed_empty_success: "任务零产物，已退款",
  reclaimed_orphan: "任务记录缺失，已按估算入账",
  // 三条「回收」细分标记：金额去向各不相同，合成一句会让对账分不清
  // 「该退的旧单」与「该收的孤账」。
  reclaimed_retry_superseded: "旧会话已被重试取代，冻结额已退回",
  reclaimed_no_evidence: "无上游受理凭证，冻结额已退回",
  reclaimed_upstream_cost_lost: "已全额退款，但上游可能已计成本",
  completion_unmeasured: "输出未计量",
  late_success_charged: "迟到成功已补收",
  // 迟到成功（先被判失败、后收到回调）的六种结局：金额去向各不相同，
  // 合成一句「迟到成功」会让「交付了但收不到钱」和「照常补扣」看起来一样。
  late_success_free: "迟到成功但未产生费用",
  late_success_after_reclaim: "迟到成功但回收已结算，未重复扣款",
  late_success_usage_archived: "迟到成功但用量已归档，未扣款",
  late_success_user_deleted: "迟到成功但账号已注销，已照常补扣",
  late_success_user_gone: "迟到成功但账号已不存在，未扣款",
  late_success_race_lost: "迟到成功但状态已被抢占，未扣款",
  submit_orphaned: "上游已建单但单号一度丢失，已补记",
  submit_ambiguous: "提交结果未知",
  cancel_race: "取消与完成竞态",
  body_truncated: "响应体被截断",
  // 与 body_truncated 的区别：截断是「读到了但没收全」，这条是「读不出可用响应体」
  // （上游 200 但内容无法解析），排障方向不同：前者查网络/超时，后者查上游返回格式。
  upstream_body_error: "上游响应体无法解析，已按调用失败处理",
  usage_estimated: "用量为估算值",
  output_unmeasured: "输出无法测量",
  client_abort: "客户端断连",
  zero_usage: "上游零用量",
  image_tier_unpriced: "请求的尺寸档位未配价，已按最高档计费",
  image_size_conflict: "size 与 image_size/resolution 不一致，已按 size 计费",
  image_pixels_out_of_window: "请求像素超出价目区间，已按最高档计费",
  image_tier_conflict: "档位与像素指向不同档，已按高价档计费",
  image_ratio_conflict: "画幅与像素比例不一致，已按像素推导画幅",
  tier_upgrade_charged: "上游交付了更高档，已按交付档补收",
  video_tier_unpriced: "请求的视频档位未配价，已按固定档或最高档计费",
  video_tier_missing: "未指定视频档位，已按固定档或最高档计费",
  video_seconds_missing: "未拿到视频时长，已按冻结额入账",
  image_max_n_unset: "模型未配置张数上限，冻结按 1 张、结算按实际交付",
  image_count_zero: "上游未返回图片",
  // 与 image_count_zero 的区别：后者是「上游 200 但没给图、我们看不懂」（仍按 1 张兜底），
  // 前者是「上游明确失败」（0 张、全额释放）。混成一条就分不清该改代码还是该找上游。
  image_response_failed: "上游明确未产出图片，已全额释放",
  image_partial_error: "已交付部分图片，但上游同时报了整单错误",
  image_partial_all_failed: "组图逐项全部失败",
  image_n_unknown: "上游零图且请求未指定张数，已按 1 张兜底",
  image_n_mismatch: "请求张数与实际不符",
  image_n_over_delivered: "上游多出图，超出下单张数部分未计费",
  image_n_capped: "张数超上限，已按上限计费",
  tier_mismatch: "上游实际尺寸与计费档位不一致",
  billing_mode_coerced: "计费方式未配为按规格，已按价目实际计费",
  spec_kind_inferred: "计费类型缺失或与价目不符，已按价目形状纠正",
  spec_kind_conflict: "模型价目与当前接口不匹配，已按接口要求的计费方式处理",
  image_ratio_mismatch: "上游交付画幅与请求的比例不一致",
  image_spec_unrecognized: "请求尺寸无法识别，已按默认档计费",
  upstream_spec_unparsable: "上游回报的尺寸无法解析，已丢弃",
  audio_seconds_unknown: "音频时长未知，按最低消费",
  audio_seconds_estimated: "音频时长按文本估算（未拿到真实时长）",
  upstream_image_count_mismatch: "上游图片数与产物数不符",
  usage_upstream_higher: "上游用量高于请求",
  usage_upstream_lower: "上游用量低于请求",
  usage_upstream_missing: "上游未返回用量，按本地统计计费",
  usage_upstream_input_higher: "上游输入秒数高于请求",
  frozen_drift: "冻结额漂移已修正",
  cap_overflow: "上限计算溢出",
  unpriced_lane: "未配价通道按 0 计",
  charge_calc_failed: "结算公式失效，已按预扣冻结额入账",
  // 流式相关：这些标记让「用户只拿到半条流」的账单可以被解释
  stream_partial_delivered: "流式只交付了部分产物",
  stream_interrupted: "流式输出中途断开",
  response_short_write: "响应未完整送达（按实际送达计费）",
  stream_convert_failed: "响应格式转换失败",
  stream_json_fallback: "请求流式但上游整段返回，已按整段交付",
  // 提示词里的计价参数（如 `--rs 4k`）与 body 不一致：上游按提示词出片、平台按 body 计费。
  // 本轮只留痕不拦截（见 volcengine_ark_weakparams.go），这条标记是对账入口。
  weak_param_pricing_conflict: "提示词里的计价参数与请求参数不一致（按请求参数计费）",
  usage_semantic_unknown: "用量语义未知，按通用口径计费",
  // 负 token 明细（AnomalyNegativeInput 的伴生项，回答"哪个通道被归零了"）
  negative_prompt: "输入 token 为负已归零",
  negative_completion: "输出 token 为负已归零",
  negative_cache: "缓存 token 为负已归零",
  negative_cache_write5m: "5m 缓存写入为负已归零",
  negative_cache_write1h: "1h 缓存写入为负已归零",
  negative_image: "图片 token 为负已归零",
  negative_audio_input: "音频输入 token 为负已归零",
  negative_audio_output: "音频输出 token 为负已归零",
};

/** 调用来源文案（sync 是默认值，调用方应当不渲染它）。 */
export function callKindLabel(kind?: string): string {
  if (!kind) return "";
  return CALL_KIND_LABEL[kind] ?? kind;
}

/** 计费来源文案。 */
export function sourceLabel(source?: string): string {
  if (!source) return "未知";
  return SOURCE_LABEL[source] ?? source;
}

/** 档位来源文案。 */
export function tierSourceLabel(source?: string): string {
  return source ? (TIER_SOURCE_LABEL[source] ?? source) : "";
}

/** 截顶原因文案。 */
export function capReasonLabel(reason?: string): string {
  return reason ? (CAP_REASON_LABEL[reason] ?? reason) : "";
}

/** 口径标记 → 可读列表（未收录的标记原样返回，不隐藏）。 */
export function anomalyLabels(raw?: string): string[] {
  if (!raw) return [];
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => ANOMALY_LABEL[s] ?? s);
}

/**
 * 结论句：一句话说清「按什么算、算了多少的量」。
 *
 * 失败记录优先说"没扣费"——用户点开一条失败记录，第一个问题永远是
 * "这单收我钱了吗"，而不是"按什么公式算的"。
 */
export function chargeConclusion(d: PortalCallDetail): string {
  if (d.status !== "settled") {
    if (d.status === "rejected") return "未执行（未打到上游），未扣费";
    if (d.status === "released") return "本次未扣费";
    if (d.status === "pending") return "仍在进行中，金额待结算";
  }
  if (!d.charge) {
    return "本次未扣费";
  }
  const unit = d.tier_price ?? 0;
  const unitText = unit > 0 ? `单次 ${formatMoney(unit)}` : "";
  const mode = (d.billing_mode ?? "") as BillingMode | "";
  if (mode === "call") {
    return unit > 0 ? `按次计费 · ${unitText}` : "按次计费";
  }
  if (mode === "spec") {
    if (d.modality === "image" && (d.image_n ?? 0) > 0) {
      const spec = d.image_spec ? `（${d.image_spec}）` : "";
      return `按张计费 · ${d.image_n} 张${unit > 0 ? ` × ${formatMoney(unit)}` : ""}${spec}`;
    }
    if (d.modality === "video" && (d.video_seconds ?? 0) > 0) {
      const res = d.video_resolution ? `（${d.video_resolution}）` : "";
      return `按秒计费 · ${d.video_seconds}s${unit > 0 ? ` × ${formatMoney(unit)}` : ""}${res}`;
    }
    if ((d.audio_chars ?? 0) > 0) {
      return `按字符计费 · ${formatNum(d.audio_chars!)} 字符${unit > 0 ? ` × ${formatMoney(unit)}` : ""}`;
    }
    if ((d.audio_duration_secs ?? 0) > 0) {
      return `按时长计费 · ${d.audio_duration_secs}s${unit > 0 ? ` × ${formatMoney(unit)}` : ""}`;
    }
    return unit > 0 ? `按规格计费 · ${unitText}` : "按规格计费";
  }
  // 按量（token）：只列非零通道，避免"入 0 / 出 0 / 缓存 0"这种噪声。
  const parts: string[] = [];
  if (d.prompt_tokens) parts.push(`输入 ${formatNum(d.prompt_tokens)}`);
  if (d.completion_tokens) parts.push(`输出 ${formatNum(d.completion_tokens)}`);
  if (d.cache_tokens) parts.push(`缓存命中 ${formatNum(d.cache_tokens)}`);
  if (d.image_tokens) parts.push(`图片 ${formatNum(d.image_tokens)}`);
  if (d.audio_input_tokens) parts.push(`音频输入 ${formatNum(d.audio_input_tokens)}`);
  if (d.audio_output_tokens) parts.push(`音频输出 ${formatNum(d.audio_output_tokens)}`);
  return parts.length ? `按 token 计费 · ${parts.join(" / ")}` : "按 token 计费";
}

/**
 * 明细行：只返回**非零**的用量项。
 *
 * 这是"去冗余"最核心的一条：一张图像记录的 token 通道全是 0，
 * 平铺出来就是 6 行没有信息的 0。没参与计算的东西不该占一行。
 */
export function chargeRows(d: PortalCallDetail): ChargeRow[] {
  const rows: ChargeRow[] = [];
  if (d.prompt_tokens) rows.push({ label: "输入 tokens", value: formatNum(d.prompt_tokens) });
  if (d.completion_tokens) rows.push({ label: "输出 tokens", value: formatNum(d.completion_tokens) });
  if (d.cache_tokens) rows.push({ label: "缓存命中 tokens", value: formatNum(d.cache_tokens) });
  if (d.image_tokens) rows.push({ label: "图片 tokens", value: formatNum(d.image_tokens) });
  if (d.audio_input_tokens) rows.push({ label: "音频输入 tokens", value: formatNum(d.audio_input_tokens) });
  if (d.audio_output_tokens) rows.push({ label: "音频输出 tokens", value: formatNum(d.audio_output_tokens) });
  if ((d.image_n ?? 0) > 0) {
    rows.push({
      label: "生成张数",
      value: `${d.image_n} 张${d.image_spec ? ` · ${d.image_spec}` : ""}`,
    });
  }
  if ((d.video_seconds ?? 0) > 0) {
    rows.push({
      label: "视频时长",
      value: `${d.video_seconds}s${d.video_resolution ? ` · ${d.video_resolution}` : ""}`,
    });
  }
  if ((d.ref_images ?? 0) > 0) rows.push({ label: "参考图", value: `${d.ref_images} 张` });
  if ((d.audio_duration_secs ?? 0) > 0) {
    rows.push({ label: "音频时长", value: `${d.audio_duration_secs}s` });
  }
  if ((d.audio_chars ?? 0) > 0) rows.push({ label: "合成字符数", value: formatNum(d.audio_chars!) });
  if ((d.rerank_docs ?? 0) > 0) rows.push({ label: "重排文档数", value: formatNum(d.rerank_docs!) });
  return rows;
}

/** 是否发生了降额（截顶），需要显示一行警示。 */
export function isCapped(d: PortalCallDetail): boolean {
  return (d.charge_before_cap ?? 0) > (d.charge ?? 0);
}

/** 事件状态的中文说明（详情摘要条用）。 */
export const STATUS_TEXT: Record<string, string> = {
  settled: "已结算",
  released: "已释放（未扣费）",
  rejected: "未执行",
  pending: "进行中",
};
