/**
 * Metaso（秘塔 MiniMax-H3）渠道私有配置（channels.adapter_options）的表单辅助（纯函数，可单测）。
 *
 * 权威校验在 `backend/internal/relay/adapters/metaso.go` 的 `parseMetasoOptions`
 * （DisallowUnknownFields + 尾部多余内容拒绝 + 体积上限 4096 字节）。这里只复制
 * **运营一改就会踩、且能就地判定**的规则：档位取值白名单、轮询路径的三条格式约束。
 * 体积上限与未知键不可能由本模块产生（形状固定）。
 *
 * 语义：`null` / `{}` = 全部使用内置默认。与 AutoDL 不同，这里**不**把默认值当作
 * 「必须先回填再保存」的显式文档：
 *   - `fast_mode` 永远显式写入（它决定真实计费通道，含糊即事故）；
 *   - `resolution_map` 显式写入四档（缺档 = 该档未开通，必须能被运营清空并持久化）；
 *   - `context_ir_enabled` / `query_path` 跟随「未指定」语义，未改动时不下发，
 *     以免把上游未来的默认值钉死在库里。
 */

import {
  METASO_RESOLUTION_DEFAULTS as GEN_RESOLUTION_DEFAULTS,
  METASO_TIERS as GEN_TIERS,
  METASO_UPSTREAM_RESOLUTIONS as GEN_UPSTREAM_RESOLUTIONS,
} from "./adapterOptions.gen";

export type MetasoTier = (typeof GEN_TIERS)[number];

/** 四档的固定遍历顺序（由后端生成，与计费侧 second_price_quota 的键一致） */
export const METASO_TIERS: MetasoTier[] = [...GEN_TIERS];

/**
 * 平台档位 → 上游字面量的内置默认（与后端 metasoDefaultResolution 逐字一致）。
 *
 * 720p 映射 768P 是**口径约定**（上游 768P 计入平台 720p 档）；四档全开是产品拍板
 * （按上游价目表逐档计费）。若真实上游只认 768P/2K，运营把对应档位清成「未开通」即可，
 * 不需要发版。
 */
export const METASO_RESOLUTION_DEFAULTS: Record<MetasoTier, string> = GEN_RESOLUTION_DEFAULTS;

/** 上游字面量白名单（与后端 metasoUpstreamResolutions 一致）；"" = 该档未开通 */
export const METASO_UPSTREAM_RESOLUTIONS = GEN_UPSTREAM_RESOLUTIONS;

export const METASO_RESOLUTION_ITEMS: [string, string][] = [
  ["", "未开通（该档请求直接拒绝）"],
  ["480P", "480P"],
  ["768P", "768P（计入 720p 档）"],
  ["1080P", "1080P"],
  ["2K", "2K（上游时长上限 15 秒）"],
];

/** 轮询路径内置默认（与后端 metasoDefaultQueryPath 一致）：上游任务号直接拼在其后 */
export const METASO_DEFAULT_QUERY_PATH = "/api/minimax/v2/query/video_generation/";

/** 轮询路径长度上限（与后端 metasoMaxQueryPathLen 一致） */
export const METASO_MAX_QUERY_PATH_LEN = 200;

/**
 * context_ir_enabled 三态：上游该字段是布尔，但「不下发」与「下发 false」不同
 * （前者用上游默认，后者是显式关闭），所以表单必须能表达第三态。
 */
export type MetasoContextIR = "inherit" | "on" | "off";

export const METASO_CONTEXT_IR_ITEMS: [string, string][] = [
  ["inherit", "不指定（用上游默认）"],
  ["off", "显式关闭"],
  ["on", "开启"],
];

export interface MetasoOptionsForm {
  /**
   * true = 该渠道下单走优先通道（上游 fast_mode=true）。
   *
   * **只应出现在绑定 metaso-h3-fast 的渠道上**：平台是「先算价冻结 → 再选渠道」，
   * 价格由模型价目决定，渠道开关只决定实际走哪条上游通道。标准价模型配 true
   * 就是「按标准价买优先通道」，计费与实付倒挂。
   */
  fastMode: boolean;
  contextIR: MetasoContextIR;
  resolution: Record<MetasoTier, string>;
  /** 空串 = 用内置默认（提交时不下发） */
  queryPath: string;
}

/** 每次返回新对象：表单是可变态，共享嵌套对象会让「恢复默认」污染其他抽屉实例。 */
export function emptyMetasoForm(): MetasoOptionsForm {
  return {
    fastMode: false,
    contextIR: "inherit",
    resolution: { ...METASO_RESOLUTION_DEFAULTS },
    queryPath: "",
  };
}

/** 错误键：`resolution_map.<tier>` / `query_path`（与后端 ConfigError.Field 同名） */
export type MetasoOptionsErrors = Partial<Record<string, string>>;

function asRecord(v: unknown): Record<string, unknown> | null {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return null;
  return v as Record<string, unknown>;
}

/**
 * 把渠道下发的 adapter_options 原文转成表单态。
 *
 * 容错目标：**脏数据不能白屏，也不能把已知值冲掉**。单个键读不出来就落该键的默认，
 * 不整体丢弃（否则运营打开抽屉看到全是默认值，一保存就把线上配置改回默认了）。
 *
 * `query_path` 例外：库里的值**原样保留**（哪怕格式非法）。丢掉它就等于把轮询
 * 悄悄改回默认路径 —— 在途任务会在错误的 URL 上查，表现为「全都查不到」。
 * 非法值由 buildMetasoOptions 报错，要求运营显式修正。
 */
export function metasoOptionsToForm(raw: unknown): MetasoOptionsForm {
  const form = emptyMetasoForm();
  const doc = asRecord(raw);
  if (!doc) return form;

  if (typeof doc.fast_mode === "boolean") form.fastMode = doc.fast_mode;

  if (typeof doc.context_ir_enabled === "boolean") {
    form.contextIR = doc.context_ir_enabled ? "on" : "off";
  }

  const rm = asRecord(doc.resolution_map);
  if (rm) {
    for (const t of METASO_TIERS) {
      const v = rm[t];
      // 空串是合法取值（= 未开通），必须原样保留，不能被当成「读不出来」
      if (typeof v === "string" && (METASO_UPSTREAM_RESOLUTIONS as readonly string[]).includes(v.trim())) {
        form.resolution[t] = v.trim();
      }
    }
  }

  if (typeof doc.query_path === "string" && doc.query_path.trim()) {
    form.queryPath = doc.query_path.trim();
  }
  return form;
}

/** 表单态 → 请求体 */
export interface MetasoOptionsBuild {
  value: Record<string, unknown>;
  errors: MetasoOptionsErrors;
}

/**
 * 表单态 → 落库的 JSON 文档，同时给出就地错误。
 *
 * 只要 errors 非空，调用方**不得**发请求：后端同样会拒，但那条 400 只有一句话，
 * 运营不知道该改哪一项。
 */
export function buildMetasoOptions(form: MetasoOptionsForm): MetasoOptionsBuild {
  const errors: MetasoOptionsErrors = {};
  const value: Record<string, unknown> = {
    // 永远显式：这条决定真实计费通道，不能靠「缺省即 false」的隐式语义
    fast_mode: form.fastMode,
  };

  if (form.contextIR === "on") value.context_ir_enabled = true;
  else if (form.contextIR === "off") value.context_ir_enabled = false;

  const resolution: Record<string, string> = {};
  for (const t of METASO_TIERS) {
    const v = (form.resolution[t] ?? "").trim();
    const key = `resolution_map.${t}`;
    if (!(METASO_UPSTREAM_RESOLUTIONS as readonly string[]).includes(v)) {
      errors[key] = "取值只能是 未开通 / 480P / 768P / 1080P / 2K";
      continue;
    }
    // 与后端同档等价（metaso.go:206-211）：跨档映射（如 1080p→2K）会让用户按低档价
    // 拿到高档成片，而平台按高档付上游成本 —— 直接的价格倒挂，必须就地拦。
    if (v !== "" && v !== METASO_RESOLUTION_DEFAULTS[t]) {
      errors[key] = `${t} 档只能映射到 ${METASO_RESOLUTION_DEFAULTS[t]}（跨档会造成价格倒挂）`;
      continue;
    }
    resolution[t] = v;
  }
  value.resolution_map = resolution;

  const qp = form.queryPath.trim();
  if (qp) {
    if (!qp.startsWith("/")) {
      errors.query_path = "必须以 / 开头";
    } else if (!qp.endsWith("/")) {
      // 后端硬校验（metaso.go:182-190）：轮询是 query_path + 任务号 的直接拼接，
      // 少了尾斜杠会拼成 `/query/video_generationabc` → 404 → 判超时退款。
      errors.query_path = "必须以 / 结尾（轮询时任务号会直接拼在末尾）";
    } else if (qp.includes("?") || qp.includes("#")) {
      errors.query_path = "不能包含 ? 或 #（上游任务号会直接拼在路径末尾）";
    } else if (qp.length > METASO_MAX_QUERY_PATH_LEN) {
      errors.query_path = `长度不能超过 ${METASO_MAX_QUERY_PATH_LEN}`;
    } else if (qp !== METASO_DEFAULT_QUERY_PATH) {
      // 与默认值不同才下发：一致时省一层「把默认值钉死在库里」的风险
      value.query_path = qp;
    }
  }

  return { value, errors };
}

