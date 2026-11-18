import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** 徽标色彩枚举（集中定义，页面不得自行映射） */
export type TagTone = "success" | "warning" | "danger" | "info" | "primary" | "gray" | "purple";

/** 实体状态 / HTTP 状态码 → [色调, 文案]（对齐原型 badge 映射表） */
const STATUS_MAP: Record<string, [TagTone, string]> = {
  "0": ["gray", "停用"],
  "1": ["success", "启用"],
  "2": ["info", "禁用"],
  "3": ["danger", "自动禁用"],
  paid: ["success", "已支付"],
  pending: ["warning", "待支付"],
  expired: ["info", "已过期"],
  refunding: ["purple", "退款中"],
  refunded: ["gray", "已退款"],
  need_manual: ["danger", "人工处理"],
  queued: ["info", "排队中"],
  running: ["primary", "运行中"],
  succeeded: ["success", "已成功"],
  failed: ["danger", "失败"],
  canceled: ["gray", "已取消"],
  ok: ["success", "通过"],
  fail: ["danger", "未通过"],
  // 兑换码派生状态（S16，来源后端 redeem.StatusOf）
  available: ["success", "可用"],
  used: ["gray", "已用"],
  // 上游同步 diff 状态（G14，色调对齐原型 admin-models.html:556 SYNC_ST）
  new: ["success", "新增"],
  diff: ["warning", "有差异"],
  same: ["gray", "一致"],
  deprecated: ["danger", "已停售"],
  // 文件类型（门户文件页按 content_type 归类，见 fileTypeKey）
  image: ["info", "图片"],
  audio: ["success", "音频"],
  video: ["warning", "视频"],
  doc: ["gray", "文档"],
  other: ["gray", "其他"],
};

/** 模态 → [色调, 文案, 圆点色]（对齐原型 modalityBadge） */
const MODALITY_MAP: Record<string, [TagTone, string, string]> = {
  chat: ["primary", "对话", "var(--m1)"],
  image: ["purple", "图像", "var(--m2)"],
  audio: ["warning", "音频", "var(--m3)"],
  video: ["danger", "视频", "var(--m4)"],
  embedding: ["info", "向量", "var(--m5)"],
  rerank: ["gray", "重排", "var(--m6)"],
  moderation: ["gray", "审核", "var(--m7)"],
  // 第 8 类模态（决策 H）：音乐/歌词。缺这一项会降级成「灰色 + 原文 music」，
  // 不是报错而是静默不一致 —— 后台列表里会同时出现「音频」和「music」两种口径。
  music: ["success", "音乐", "var(--m8)"],
};

function resolveStatus(status: number | string): [TagTone, string, boolean] {
  if (typeof status === "number" && status >= 100) {
    if (status >= 200 && status < 300) return ["success", String(status), false];
    if (status === 429) return ["warning", String(status), false];
    if (status >= 400) return ["danger", String(status), false];
    return ["info", String(status), false];
  }
  const hit = STATUS_MAP[String(status)];
  if (hit) return [hit[0], hit[1], status === "running"];
  return ["gray", String(status), false];
}

export interface StatusTagProps {
  /** 实体状态（0-3 或枚举串）或 HTTP 状态码 */
  status: number | string;
  className?: string;
}

/** 语义键 → [色调, 文案]（未知键降级为灰色原文，不隐藏信息） */
export function tagOf(key: string): [TagTone, string] {
  return STATUS_MAP[key] ?? ["gray", key];
}

/** 通用徽标（已有语义文案、只需指定色调时使用；页面层禁止自造 `badge-*` 类） */
export function Tag({
  tone,
  className,
  testId,
  title,
  children,
}: {
  tone: TagTone;
  className?: string;
  testId?: string;
  /**
   * 悬停说明。
   *
   * 存在的理由很具体：徽标里显示的是**人话**（「登录失败」），而排障要看的是
   * **原始事件名**（`login_failed`）。把原文塞进徽标会把表格撑变形，
   * 放 title 里既不占位又随时可查。
   */
  title?: string;
  children: ReactNode;
}) {
  return (
    <span className={cn(`badge badge-${tone}`, className)} data-testid={testId} title={title}>
      {children}
    </span>
  );
}

/** 日志事件类型 → [色调, 文案]（`pending` 在日志域=冻结中，与订单域「待支付」冲突，故独立映射） */
const LOG_KIND_MAP: Record<string, [TagTone, string]> = {
  consume: ["success", "消耗"],
  client_abort: ["gray", "客户端断开"],
  upstream_error: ["danger", "上游错误"],
  error: ["warning", "失败"],
  pending: ["info", "冻结中"],
  // 预检期拒绝（0 费、一次都没打到上游）：必须与 upstream_error 分开着色 ——
  // 两者混在一起时，"渠道失败率"会被大量鉴权/限流噪声淹没。
  rate_limited: ["warning", "预检限流"],
  auth_error: ["danger", "预检鉴权失败"],
  insufficient_balance: ["warning", "余额不足"],
  invalid_request: ["gray", "预检参数错误"],
};

/** 调用日志事件徽标 */
export function LogKindTag({ kind }: { kind: string }) {
  const [tone, label] = LOG_KIND_MAP[kind] ?? ["gray", kind];
  return <Tag tone={tone}>{label}</Tag>;
}

/** 文件 content_type → 文件类型徽标键（pdf 归「文档」，其余「其他」） */
export function fileTypeKey(contentType: string): string {
  const ct = (contentType ?? "").toLowerCase();
  if (ct.startsWith("image/")) return "image";
  if (ct.startsWith("audio/")) return "audio";
  if (ct.startsWith("video/")) return "video";
  if (ct.startsWith("text/") || ct.includes("pdf")) return "doc";
  return "other";
}

/** 状态徽标（枚举→色彩集中映射；running 带呼吸点） */
export function StatusTag({ status, className }: StatusTagProps) {
  const [tone, label, pulse] = resolveStatus(status);
  return (
    <span className={cn(`badge badge-${tone}`, className)}>
      {pulse ? <span className="pulse" /> : null}
      {label}
    </span>
  );
}

/**
 * 计费会话态 → 通用徽标等价键。
 * `pending` 在订单域=待支付，在计费会话域=进行中，故必须在此显式映射，不可直接交给 STATUS_MAP。
 * （门户「调用记录」与「主页最近调用」共用，避免页面各自重复映射。）
 */
export function usageStatusKey(status: string): string {
  if (status === "settled") return "succeeded";
  if (status === "released") return "failed";
  if (status === "pending") return "running";
  // 预检期拒绝（一次都没打到上游、0 费）：语义上确实是"失败"，但与 released 的
  // 责任方不同，列表里靠「事件类型」列区分；徽标统一用 failed 避免新增色调。
  if (status === "rejected") return "failed";
  return status;
}

export interface ModalityTagProps {
  modality: string;
  className?: string;
}

/** 模态徽标（对话/图像/音频/视频/向量/重排/审核） */
export function ModalityTag({ modality, className }: ModalityTagProps) {
  const [tone, label, dot] = MODALITY_MAP[modality] ?? ["gray", modality, "var(--t3)"];
  return (
    <span className={cn(`badge badge-${tone}`, className)}>
      <i
        style={{
          width: 6,
          height: 6,
          borderRadius: "50%",
          background: dot,
          display: "inline-block",
        }}
      />
      {label}
    </span>
  );
}
