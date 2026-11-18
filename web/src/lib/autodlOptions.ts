/**
 * AutoDL 渠道私有配置（channels.adapter_options）的表单辅助（纯函数，可单测）。
 *
 * 权威校验在 `backend/internal/relay/adapters/autodl.go` 的 `parseAutoDLOptions`。
 * 这里只复制**三条运营一改就会踩、且能就地判定**的规则（ID 字符集、四项不得重复、
 * 分辨率取值白名单），其余（体积上限、未知键、四项必须齐全）交给后端 400 文案 ——
 * 本模块的形状是固定的，不可能产生未知键。
 *
 * 语义：`null` / `{}` = 使用内置默认。前端编辑时把默认值回填进表单，
 * 保存后写入的是**显式全量文档**（运营看得见当前真实生效的工作流 ID）。
 */

export type AutoDLMode = "multimodal" | "image" | "text" | "first_last";

export type AutoDLTier = "480p" | "720p" | "1080p";

import {
  AUTODL_MODES as GEN_MODES,
  AUTODL_RESOLUTION_DEFAULTS as GEN_RESOLUTION_DEFAULTS,
  AUTODL_UPSTREAM_RESOLUTIONS as GEN_UPSTREAM_RESOLUTIONS,
  AUTODL_WORKFLOW_DEFAULTS as GEN_WORKFLOW_DEFAULTS,
} from "./adapterOptions.gen";

/** 四种模式的展示顺序（由后端生成，与 autodlModes 一致） */
export const AUTODL_MODE_ORDER: AutoDLMode[] = [...GEN_MODES];

export const AUTODL_MODE_LABELS: Record<AutoDLMode, string> = {
  multimodal: "多模态（图+音）",
  image: "图片参考（图生视频）",
  text: "纯文本（文生视频）",
  first_last: "首尾帧",
};

/** 识别规则说明（与 planAutoDLRequest 的 switch 逐条对应，改动时两边同步） */
export const AUTODL_MODE_TIPS: Record<AutoDLMode, string> = {
  multimodal: "请求带音频（可同时带参考图）时命中；上游 ref_image_0 必填",
  image: "有参考图、无音频时命中",
  text: "既无参考图也无音频时命中",
  first_last: "同时提供 first_frame 与 last_frame 时命中；该工作流不接受参考图与音频",
};

/**
 * 内置默认工作流（与后端 autodlDefaultWorkflows 逐字一致）。
 * placeholder 也用它们：字段留空 = 报错（后端「非空即全量」，静默补默认是最难发现的配置错）。
 */
export const AUTODL_WORKFLOW_DEFAULTS: Record<AutoDLMode, string> = GEN_WORKFLOW_DEFAULTS;

export const AUTODL_TIERS: AutoDLTier[] = ["480p", "720p", "1080p"];

/**
 * 平台档位 → 上游画质基名（"" = 该档不支持）。
 *
 * 720p 映射到 768p 是**口径约定**（768p 计入平台 720p 档）；1080p 默认空，
 * 因为上游最高 768p —— 静默降档会让「用户要 1080p、拿到 768p、按 1080p 付费」。
 */
export const AUTODL_RESOLUTION_DEFAULTS: Record<AutoDLTier, string> = GEN_RESOLUTION_DEFAULTS;

/** 上游画质基名白名单（与 parseAutoDLOptions 的取值校验一致） */
export const AUTODL_UPSTREAM_RESOLUTIONS = GEN_UPSTREAM_RESOLUTIONS;

export const AUTODL_RESOLUTION_ITEMS: [string, string][] = [
  ["", "不支持（该档请求直接拒绝）"],
  ["480p", "480p"],
  ["768p", "768p（计入 720p 档）"],
  ["1080p", "1080p（上游当前无此画质）"],
];

export interface AutoDLOptionsForm {
  workflows: Record<AutoDLMode, string>;
  resolution: Record<AutoDLTier, string>;
}

/** 每次返回新对象：表单是可变态，共享嵌套对象会让「恢复默认」污染其他抽屉实例。 */
export function emptyAutoDLForm(): AutoDLOptionsForm {
  return {
    workflows: { ...AUTODL_WORKFLOW_DEFAULTS },
    resolution: { ...AUTODL_RESOLUTION_DEFAULTS },
  };
}

/** 错误键：`workflows.<mode>` / `resolution_map.<tier>`（与后端 ConfigError.Field 同名） */
export type AutoDLOptionsErrors = Partial<Record<string, string>>;

function asRecord(v: unknown): Record<string, unknown> | null {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return null;
  return v as Record<string, unknown>;
}

/**
 * 把渠道下发的 adapter_options 原文转成表单态。
 *
 * 容错目标只有一个：**脏数据/缺失不能白屏，也不能把已知值冲掉**。
 * 单个键读不出来就落默认，不整体丢弃（否则运营打开抽屉看到的全是默认值，
 * 一保存就把线上配置改回默认了）。
 */
export function autodlOptionsToForm(raw: unknown): AutoDLOptionsForm {
  const form = emptyAutoDLForm();
  const doc = asRecord(raw);
  if (!doc) return form;

  const wf = asRecord(doc.workflows);
  if (wf) {
    for (const m of AUTODL_MODE_ORDER) {
      const v = wf[m];
      if (typeof v === "string" && v.trim()) form.workflows[m] = v.trim();
    }
  }
  const rm = asRecord(doc.resolution_map);
  if (rm) {
    for (const t of AUTODL_TIERS) {
      const v = rm[t];
      // 空字符串是合法取值（= 不支持），必须原样保留，不能当"读不出来"处理
      if (typeof v === "string" && (AUTODL_UPSTREAM_RESOLUTIONS as readonly string[]).includes(v.trim())) {
        form.resolution[t] = v.trim();
      }
    }
  }
  return form;
}

/** 表单态 → 请求体（显式全量文档） */
export interface AutoDLOptionsBuild {
  value: Record<string, unknown>;
  errors: AutoDLOptionsErrors;
}

const WORKFLOW_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

/**
 * 表单态 → 落库的 JSON 文档，同时给出就地错误。
 *
 * 只要 errors 非空，调用方**不得**发请求：后端同样会拒，
 * 但那条 400 只会显示一句 toast，运营不知道该改哪一项。
 */
export function buildAutoDLOptions(form: AutoDLOptionsForm): AutoDLOptionsBuild {
  const errors: AutoDLOptionsErrors = {};
  const workflows: Record<string, string> = {};

  for (const m of AUTODL_MODE_ORDER) {
    const v = (form.workflows[m] ?? "").trim();
    const key = `workflows.${m}`;
    if (!v) {
      errors[key] = `请填写${AUTODL_MODE_LABELS[m]}的上游工作流（默认 ${AUTODL_WORKFLOW_DEFAULTS[m]}）`;
      continue;
    }
    if (!WORKFLOW_PATTERN.test(v)) {
      errors[key] = "工作流 ID 只能是 1-64 位字母/数字/下划线/连字符";
      continue;
    }
    workflows[m] = v;
  }

  // 四项不得重复：同一 ID 服务两种模式必然有一种跑错，且错的那次照样扣费。
  const seen = new Map<string, AutoDLMode>();
  for (const m of AUTODL_MODE_ORDER) {
    const v = workflows[m];
    if (!v) continue;
    const prev = seen.get(v);
    if (prev) {
      errors[`workflows.${m}`] = `与「${AUTODL_MODE_LABELS[prev]}」重复：同一工作流不能服务两种模式`;
    } else {
      seen.set(v, m);
    }
  }

  const resolution: Record<string, string> = {};
  for (const t of AUTODL_TIERS) {
    const v = (form.resolution[t] ?? "").trim();
    const key = `resolution_map.${t}`;
    if (!(AUTODL_UPSTREAM_RESOLUTIONS as readonly string[]).includes(v)) {
      errors[key] = "取值只能是 不支持 / 480p / 768p / 1080p";
      continue;
    }
    resolution[t] = v;
  }

  return { value: { workflows, resolution_map: resolution }, errors };
}
