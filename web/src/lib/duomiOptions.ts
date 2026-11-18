/**
 * 多米API（duomiapi）渠道私有配置（channels.adapter_options）的表单辅助（纯函数，可单测）。
 *
 * 权威校验在 `backend/internal/relay/adapters/duomiapi.go` 的 `parseDuomiOptions`
 * （DisallowUnknownFields + 尾部多余内容拒绝 + 体积上限 4096 字节 + 已知模型协议锁）。
 * 这里只保留**运营一改就会踩、且能就地判定**的规则：路径前缀、
 * metadata 白名单去重、模型覆盖表的形状与协议锁。
 *
 * 语义：`null` / `{}` = 全部使用内置默认。
 *   - `auth_prefix` **三态表达**：`-` 是「空前缀（裸 token）」的显式写法，
 *     而「键不存在」才表示「用内置默认 Bearer + 空格」。表单里直接填空串无法区分两者，
 *     所以必须做成下拉三态，否则运营永远配不出「不带 Bearer」的渠道。
 *   - `metadata_allow` **永远显式下发**：它决定上游是否额外加水印 / 生成音轨，
 *     直接改变产出与成本；靠「缺省即默认」的隐式语义会让运营看不出真实生效值。
 *   - `paths.*` 未改动时**不下发**（跟随内置默认，避免把默认路径钉死在库里）。
 *   - `model_overrides` 用 JSON 文本框：它承载任意对外模型 ID 的协议覆盖，做成表格
 *     会逼前端维护一份与后端档案重复的模型清单；文本框 + 即时校验反而不会漂。
 *     形状仍是严格白名单（4 个子键），拼错一个键就是 400。
 *
 * **含 `suno_defaults`（B6-1）**：音乐子协议的渠道级默认（是否自定义歌词 / 是否纯音乐 /
 * 上游模型版本）。它决定「这个渠道出的到底是歌还是纯音乐」，属于产品形态而不是每次请求的参数。
 */

import {
  DUOMI_DEFAULT_METADATA_ALLOW as GEN_DEFAULT_METADATA_ALLOW,
  DUOMI_KNOWN_MODELS as GEN_KNOWN_MODELS,
  DUOMI_METADATA_KEYS as GEN_METADATA_KEYS,
  DUOMI_OVERRIDE_KEYS as GEN_OVERRIDE_KEYS,
  DUOMI_PATH_KEYS as GEN_PATH_KEYS,
  DUOMI_PROTOCOLS as GEN_PROTOCOLS,
} from "./adapterOptions.gen";

/**
 * 子协议枚举 —— 由后端生成（adapterOptions.gen.ts），这里只做类型收窄。
 *
 * 手写第二份的代价不是「校验不一致」，而是整渠道 503：后端 DisallowUnknownFields
 * 会因为一个多出来的键拒绝整段配置。
 */
export const DUOMI_PROTOCOLS = GEN_PROTOCOLS;
export type DuomiProtocol = (typeof DUOMI_PROTOCOLS)[number];

export const DUOMI_PROTOCOL_ITEMS: [DuomiProtocol, string][] = [
  ["seedance", "seedance（火山：文生 / 图生 / 首尾帧 / 多模态参考）"],
  ["kling", "kling（按素材自动选 text2video / image2video）"],
  ["kling.multi", "kling.multi（固定 multi-image2video，1–4 张参考图）"],
  ["kling.omni", "kling.omni（固定 omni-video，3–15 秒）"],
  ["muse", "muse（多米原生视频：参考图 1–8 张 + 画幅，无分辨率参数）"],
  ["nano", "nano（Gemini 图像：有参考图走编辑端点，无图走文生图端点）"],
  ["gptimage", "gptimage（OpenAI 兼容图像，异步任务）"],
  ["suno.music", "suno.music（SUNO 作曲，一次出 2 首）"],
  ["suno.lyrics", "suno.lyrics（SUNO 写词，产物是纯文本）"],
];

/**
 * 可灵 mode 枚举（决定上游画质档位）。"" = 该模型不使用 mode。
 *
 * 档位不在这里定义：11-02 起「模型能出哪几档」只由 `models.spec_prices` 与
 * 请求参数决定，渠道配置不参与 —— 渠道再写一个 fixed_tier 只会造成
 * 「配置说 720p、价目按 1080p 收」的分裂。
 */
export const DUOMI_KLING_MODES = ["", "std", "pro"] as const;
export type DuomiKlingMode = (typeof DUOMI_KLING_MODES)[number];

/** 路径键枚举（与后端 duomiPathKeys 一致） */
export const DUOMI_PATH_KEYS = GEN_PATH_KEYS;
export type DuomiPathKey = (typeof DUOMI_PATH_KEYS)[number];

/**
 * 路径键 → 内置默认（与后端逐字一致）。
 *
 * `kling.prefix` 以 `/` 结尾：上游任务号**直接拼在末尾**（`{prefix}{task_id}`）。
 * `gptimage.poll` 是**前缀**（`{prefix}/{task_id}`），其余都是完整路径。
 * nano 有两个提交端点（图生图单独开在 `/nano-banana-edit`），查询只挂在
 * `/api/gemini/nano-banana/{id}` 下 —— 所以 `nano.poll` 与 `nano.submit` 是两个键。
 */
export const DUOMI_DEFAULT_PATHS: Record<DuomiPathKey, string> = {
  "seedance.submit": "/api/v3/contents/generations/tasks",
  "kling.prefix": "/api/video/kling/v1/videos/",
  "muse.submit": "/v1/videos/generations",
  // 查询路径**不是**提交路径 + /{id}：两者没有任何前缀关系。写成后者不会报错，
  // 只会让每次轮询都 404，任务一直 running 到超时退款。
  "muse.poll": "/v1/videos/tasks",
  "nano.submit": "/api/gemini/nano-banana",
  "nano.edit": "/api/gemini/nano-banana-edit",
  "nano.poll": "/api/gemini/nano-banana",
  "gptimage.submit": "/v1/images/generations",
  "gptimage.poll": "/v1/tasks",
  "suno.submit": "/api/suno/generate",
  "suno.feed": "/api/suno/feed",
  "suno.lyrics": "/api/suno/submit/lyrics",
};

export const DUOMI_PATH_ITEMS: [DuomiPathKey, string][] = [
  ["seedance.submit", "Seedance 任务提交路径"],
  ["kling.prefix", "可灵视频路径前缀"],
  ["muse.submit", "Muse 视频提交路径"],
  ["muse.poll", "Muse 视频查询路径前缀（任务号拼在末尾）"],
  ["nano.submit", "Nano 文生图提交路径"],
  ["nano.edit", "Nano 图像编辑提交路径"],
  ["nano.poll", "Nano 任务查询路径"],
  ["gptimage.submit", "GPT Image 提交路径"],
  ["gptimage.poll", "GPT Image 查询路径前缀"],
  ["suno.submit", "SUNO 作曲提交路径"],
  ["suno.feed", "SUNO 查询路径"],
  ["suno.lyrics", "SUNO 歌词提交路径"],
];

/** 路径长度上限（与后端 duomiMaxPathLen 一致） */
export const DUOMI_MAX_PATH_LEN = 200;
/** 配置体积上限（与后端 duomiMaxOptionsBytes 一致；超了后端整体拒绝） */
export const DUOMI_MAX_OPTIONS_BYTES = 4096;
/** 鉴权前缀长度上限（与后端 parseDuomiOptions 内联判定一致） */
export const DUOMI_MAX_AUTH_PREFIX_LEN = 32;
/** SUNO 模型版本长度上限（与后端 suno_defaults.mv 判定一致） */
export const DUOMI_MAX_SUNO_MV_LEN = 64;

/** SUNO 三态布尔下拉项（"" = 不下发，沿用内置默认） */
export const DUOMI_TRIBOOL_ITEMS: [TriBool, string][] = [
  ["", "沿用内置默认"],
  ["true", "是"],
  ["false", "否"],
];

/** 内置默认鉴权前缀（与后端 duomiDefaultAuthPrefix 一致） */
export const DUOMI_DEFAULT_AUTH_PREFIX = "Bearer ";

/**
 * 鉴权头形态三态。
 *
 * `none` 下发 `"-"`（后端会归一成空串），`default` 不下发（用内置默认）。
 * 二者不能合成「填空串」，否则运营想切裸 token 时会发现「改了没反应」。
 */
export type DuomiAuthMode = "default" | "none" | "custom";

export const DUOMI_AUTH_ITEMS: [DuomiAuthMode, string][] = [
  ["default", `内置默认（${DUOMI_DEFAULT_AUTH_PREFIX.trim()} + 空格）`],
  ["none", "无前缀（Authorization: <token>）"],
  ["custom", "自定义"],
];

/** metadata 透传的**全量**已知键（与后端 duomiMetadataKnown 一致）；只有这些能被放行 */
export const DUOMI_METADATA_KEYS = GEN_METADATA_KEYS;
export type DuomiMetadataKey = (typeof DUOMI_METADATA_KEYS)[number];

/** 与后端内置默认一致：放行「是否出声 / 平台水印」——它们直接改变产出与上游成本 */
export const DUOMI_DEFAULT_METADATA_ALLOW = GEN_DEFAULT_METADATA_ALLOW;

/**
 * 白名单勾选项。
 *
 * 逐项**不配问号**：三项的后果（改变产出与成本）在区块级说明里已完整交代，
 * 再给每个开关挂一个问号只会把「默认无提示」的文案预算耗在重复信息上
 * （UI 契约门禁的 M19 就是按文件冻结问号数）。标签本身就写明后果。
 */
export const DUOMI_METADATA_ITEMS: [DuomiMetadataKey, string][] = [
  ["aigc_watermark", "aigc_watermark（AIGC 合规水印，历史键，默认不放行）"],
  ["generate_audio", "generate_audio（同步生成音轨，影响产出与成本）"],
  ["watermark", "watermark（平台水印，影响产出）"],
];

/**
 * 模型覆盖对象允许的 4 个子键（与后端 duomiOverride 的 json tag 逐字一致）。
 *
 * 这里与后端不一致的代价不是「配置不生效」，而是**整个渠道所有模型 503**：
 * 后端用 DisallowUnknownFields 解析，多一个键就整段拒绝（2026-11-04 fixed_tier 事故）。
 */
export const DUOMI_OVERRIDE_KEYS = GEN_OVERRIDE_KEYS;
export type DuomiOverrideKey = (typeof DUOMI_OVERRIDE_KEYS)[number];

export interface DuomiKnownModel {
  protocol: DuomiProtocol;
  upstream: string;
  mode: DuomiKlingMode;
}

/**
 * 平台官方支持、且种子里已建模型的档案（与后端 duomiModels 逐条一致）。
 *
 * 用途只有两个：①「已知模型不得改成不兼容协议」的本地校验（后端同样会拒）；
 * ② UI 里展示「填了什么会被接受」。**不**用它去限制 model_overrides 的键集 ——
 * 新增上游型号走配置即可，不需要发版。
 */
export const DUOMI_KNOWN_MODELS: Record<string, DuomiKnownModel> = GEN_KNOWN_MODELS;

export interface DuomiOptionsForm {
  authMode: DuomiAuthMode;
  /** authMode === "custom" 时生效 */
  authCustom: string;
  /** 空串 = 用内置默认（提交时不下发） */
  paths: Record<DuomiPathKey, string>;
  /** 放行清单（可勾空 = 什么都不透传） */
  metadataAllow: DuomiMetadataKey[];
  /** model_overrides 的 JSON 文本；空文本 = 不配置 */
  overridesText: string;
  /**
   * SUNO 渠道级默认（三态字符串）。
   *
   * 为什么不用 boolean：`false` 与「不配」在上游语义不同 —— 不配等于沿用内置默认，
   * 配成 false 才是「显式关掉」。用 boolean + 一个"是否启用"开关会多一个可漏勾的状态，
   * 三态下拉能一次性表达三种意图。
   */
  sunoCustomMode: TriBool;
  sunoInstrumental: TriBool;
  /** SUNO 模型版本（chirp-v4 等）；空 = 用绑定表的上游模型名 */
  sunoMv: string;
  /**
   * 库里存在但前端不认识的顶层键（只可能来自手工改库）。
   *
   * 必须记录下来并在保存时报错：静默丢弃等于「运营一点保存就把别人手工配的东西删了」。
   */
  unknownKeys: string[];
}

/** 三态布尔："" = 不下发（沿用内置默认） */
export type TriBool = "" | "true" | "false";

/** 每次返回新对象：表单是可变态，共享嵌套对象会让「恢复默认」污染其他抽屉实例。 */
export function emptyDuomiForm(): DuomiOptionsForm {
  return {
    authMode: "default",
    authCustom: "",
    paths: {
      "seedance.submit": "",
      "kling.prefix": "",
      "muse.submit": "",
      "muse.poll": "",
      "nano.submit": "",
      "nano.edit": "",
      "nano.poll": "",
      "gptimage.submit": "",
      "gptimage.poll": "",
      "suno.submit": "",
      "suno.feed": "",
      "suno.lyrics": "",
    },
    metadataAllow: [...DUOMI_DEFAULT_METADATA_ALLOW],
    overridesText: "",
    sunoCustomMode: "",
    sunoInstrumental: "",
    sunoMv: "",
    unknownKeys: [],
  };
}

/** 错误键与后端 ConfigError.Field 同名（`paths.<key>` / `model_overrides.<id>.<sub>`） */
export type DuomiOptionsErrors = Record<string, string>;

const KNOWN_TOP_KEYS = new Set([
  "auth_prefix",
  "model_overrides",
  "paths",
  "metadata_allow",
  "suno_defaults",
]);

/** 布尔字段的 JSON 值 → 三态字符串（非布尔一律当作「未配置」，不硬塞 false） */
function triOf(v: unknown): TriBool {
  if (v === true) return "true";
  if (v === false) return "false";
  return "";
}

function asRecord(v: unknown): Record<string, unknown> | null {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return null;
  return v as Record<string, unknown>;
}

function isProtocol(v: string): v is DuomiProtocol {
  return (DUOMI_PROTOCOLS as readonly string[]).includes(v);
}

/**
 * 把渠道下发的 adapter_options 原文转成表单态。
 *
 * 容错目标与 Metaso 一致：**脏数据不能白屏，也不能把已知值冲掉**。单个键读不出来
 * 就落该键的默认，不整体丢弃（否则运营打开抽屉看到全是默认值，一保存就把线上配置改回默认）。
 *
 * `paths.*` 例外：库里的值**原样保留**（哪怕格式非法），非法值由 buildDuomiOptions
 * 报错要求运营显式修正 —— 悄悄改回默认会让在途任务查错端点，表现为「全都查不到」。
 */
export function duomiOptionsToForm(raw: unknown): DuomiOptionsForm {
  const form = emptyDuomiForm();
  const doc = asRecord(raw);
  if (!doc) return form;

  form.unknownKeys = Object.keys(doc).filter((k) => !KNOWN_TOP_KEYS.has(k));

  if (typeof doc.auth_prefix === "string") {
    const p = doc.auth_prefix;
    // "" 与 "-" 在后端都归一成「空前缀」（"" 是历史手填、"-" 是显式写法），表单合并到「无前缀」态
    if (p === "-" || p === "") {
      form.authMode = "none";
    } else if (p === DUOMI_DEFAULT_AUTH_PREFIX) {
      // 只认「与内置默认逐字相同」这一种：`Bearer`（少一个空格）在后端是**另一个值**，
      // 若也算默认，运营一保存就会把它静默改成 `Bearer `—— 那属于掩盖问题。
      form.authMode = "default";
    } else {
      form.authMode = "custom";
      form.authCustom = p;
    }
  }

  const paths = asRecord(doc.paths);
  if (paths) {
    for (const k of DUOMI_PATH_KEYS) {
      const v = paths[k];
      if (typeof v === "string" && v.trim()) form.paths[k] = v.trim();
    }
  }

  if (Array.isArray(doc.metadata_allow)) {
    const picked: DuomiMetadataKey[] = [];
    for (const it of doc.metadata_allow) {
      if (typeof it !== "string") continue;
      const k = it.trim();
      if ((DUOMI_METADATA_KEYS as readonly string[]).includes(k) && !picked.includes(k as DuomiMetadataKey)) {
        picked.push(k as DuomiMetadataKey);
      }
    }
    form.metadataAllow = picked;
  }

  const ov = asRecord(doc.model_overrides);
  if (ov && Object.keys(ov).length > 0) {
    // 原文回填（不做字段重排）：运营看到的应是「库里存的样子」，改了才叫改
    form.overridesText = JSON.stringify(ov, null, 2);
  }

  const suno = asRecord(doc.suno_defaults);
  if (suno) {
    form.sunoCustomMode = triOf(suno.custom_mode);
    form.sunoInstrumental = triOf(suno.make_instrumental);
    if (typeof suno.mv === "string") form.sunoMv = suno.mv.trim();
  }

  return form;
}

export interface DuomiOptionsBuild {
  value: Record<string, unknown>;
  errors: DuomiOptionsErrors;
}

/**
 * 表单态 → 落库的 JSON 文档，同时给出就地错误。
 *
 * 只要 errors 非空，调用方**不得**发请求：后端同样会拒，但那条 400 只有一句话，
 * 运营不知道该改哪一项。
 */
export function buildDuomiOptions(form: DuomiOptionsForm): DuomiOptionsBuild {
  const errors: DuomiOptionsErrors = {};
  const value: Record<string, unknown> = {};

  if (form.unknownKeys.length > 0) {
    errors.adapter_options =
      `配置里存在未知键 ${form.unknownKeys.join(", ")}（只可能来自手工改库）。` +
      `直接保存会丢掉它们，请先确认后手工清理。`;
  }

  // ---- auth_prefix（三态）
  if (form.authMode === "none") {
    value.auth_prefix = "-";
  } else if (form.authMode === "custom") {
    const p = form.authCustom;
    if (/[\r\n]/.test(p)) {
      errors.auth_prefix = "鉴权前缀不能包含换行";
    } else if (p.length > DUOMI_MAX_AUTH_PREFIX_LEN) {
      errors.auth_prefix = `长度不能超过 ${DUOMI_MAX_AUTH_PREFIX_LEN} 个字符`;
    } else if (p === "-") {
      // "-" 是「空前缀」的保留写法，走「无前缀」选项表达，不要让自定义值撞上
      errors.auth_prefix = "「-」是保留值（表示无前缀），请改用上面的「无前缀」选项";
    } else if (p.trim() === "") {
      errors.auth_prefix = "自定义前缀不能为空；不带前缀请选「无前缀」";
    } else {
      value.auth_prefix = p;
    }
  }
  // authMode === "default" 时**不下发**：让后端回落到内置默认，避免把默认值钉死在库里

  // ---- paths（未改动不下发）
  const paths: Record<string, string> = {};
  for (const k of DUOMI_PATH_KEYS) {
    const v = (form.paths[k] ?? "").trim();
    if (!v) continue;
    if (!v.startsWith("/")) {
      errors[`paths.${k}`] = "必须以 / 开头";
    } else if (/[?#]/.test(v)) {
      errors[`paths.${k}`] = "不能包含 ? 或 #（上游任务号会直接拼在路径末尾）";
    } else if (v.length > DUOMI_MAX_PATH_LEN) {
      errors[`paths.${k}`] = `长度不能超过 ${DUOMI_MAX_PATH_LEN}`;
    } else if (v !== DUOMI_DEFAULT_PATHS[k]) {
      paths[k] = v;
    }
  }
  if (Object.keys(paths).length > 0) value.paths = paths;

  // ---- metadata_allow（永远显式：它决定产出与成本）
  const seen: DuomiMetadataKey[] = [];
  for (const k of form.metadataAllow) {
    if (!(DUOMI_METADATA_KEYS as readonly string[]).includes(k)) {
      errors.metadata_allow = `未知的透传键 ${k}`;
      continue;
    }
    if (!seen.includes(k)) seen.push(k);
  }
  // 固定顺序输出：让「内容相同」的配置生成「字节相同」的 JSON（便于比对与回滚）
  value.metadata_allow = DUOMI_METADATA_KEYS.filter((k) => seen.includes(k));

  // ---- suno_defaults（只在显式配置时下发；键名与后端 json tag 一致）
  //
  // 后端字段名是 `make_instrumental`（上游 SUNO 的叫法），不是 `instrumental`：
  // 写错后端会按「未知字段」整体拒绝，而运营只会看到「配置不是合法的 JSON」。
  const suno: Record<string, unknown> = {};
  if (form.sunoCustomMode !== "") suno.custom_mode = form.sunoCustomMode === "true";
  if (form.sunoInstrumental !== "") suno.make_instrumental = form.sunoInstrumental === "true";
  const mv = form.sunoMv.trim();
  if (mv) {
    if (mv.length > DUOMI_MAX_SUNO_MV_LEN) {
      errors["suno_defaults.mv"] = `模型版本不能超过 ${DUOMI_MAX_SUNO_MV_LEN} 个字符`;
    } else {
      suno.mv = mv;
    }
  }
  // 纯音乐 + 自定义歌词是互斥形态：同时开等于「给的歌词永远不会被唱」，后端保存期也会拒。
  if (suno.custom_mode === true && suno.make_instrumental === true) {
    errors.suno_defaults = "自定义歌词模式与纯音乐不能同时开启（纯音乐不唱歌词）";
  }
  if (Object.keys(suno).length > 0) value.suno_defaults = suno;

  // ---- model_overrides（JSON 文本框）
  const text = form.overridesText.trim();
  if (text) {
    const ovErr = "model_overrides";
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch (e) {
      errors[ovErr] = `不是合法的 JSON：${e instanceof Error ? e.message : String(e)}`;
      parsed = undefined;
    }
    const obj = parsed === undefined ? null : asRecord(parsed);
    if (parsed !== undefined && !obj) {
      errors[ovErr] = '必须是 JSON 对象（形如 {"模型ID": {"protocol": "seedance"}}）';
    }
    if (obj) {
      const out: Record<string, Record<string, unknown>> = {};
      for (const [rawID, rawOv] of Object.entries(obj)) {
        const id = rawID.trim();
        if (!id) {
          errors[ovErr] = "模型 ID 不能为空";
          continue;
        }
        const one = asRecord(rawOv);
        if (!one) {
          errors[`${ovErr}.${id}`] = "每个模型的配置必须是一个 JSON 对象";
          continue;
        }
        const entry: Record<string, unknown> = {};
        let ok = true;
        for (const key of Object.keys(one)) {
          if (!(DUOMI_OVERRIDE_KEYS as readonly string[]).includes(key)) {
            errors[`${ovErr}.${id}.${key}`] = `未知字段（只支持 ${DUOMI_OVERRIDE_KEYS.join(" / ")}）`;
            ok = false;
          }
        }
        const protocol = typeof one.protocol === "string" ? one.protocol.trim() : "";
        if (protocol) {
          if (!isProtocol(protocol)) {
            errors[`${ovErr}.${id}.protocol`] = `协议只能是 ${DUOMI_PROTOCOLS.join(" / ")}`;
            ok = false;
          } else {
            entry.protocol = protocol;
          }
        }
        const mode = typeof one.mode === "string" ? one.mode.trim() : "";
        if (mode) {
          if (mode !== "std" && mode !== "pro") {
            errors[`${ovErr}.${id}.mode`] = "mode 只支持 std / pro";
            ok = false;
          } else {
            entry.mode = mode;
          }
        }
        // kling.multi（multi-image2video）不接受 mode：画质由端点本身决定，
        // 下发会直接 400 并被归因成渠道故障。后端保存期同样硬拒，这里提前拦。
        const effectiveProto = protocol || DUOMI_KNOWN_MODELS[id]?.protocol || "";
        if (effectiveProto === "kling.multi" && mode) {
          errors[`${ovErr}.${id}.mode`] =
            "kling.multi 协议不支持 mode（画质由 multi-image2video 端点决定），请删除该字段";
          ok = false;
        }
        // duration_as_string 是**布尔**（后端是 *bool）：写成 "true" 字符串会被拒。
        // 不写 = 沿用内置档案；写 false 才是「显式要求数字」——两者不能合并。
        if ("duration_as_string" in one) {
          const v = one.duration_as_string;
          if (typeof v !== "boolean") {
            errors[`${ovErr}.${id}.duration_as_string`] =
              "duration_as_string 只能是 JSON 布尔 true / false（不要写成字符串）";
            ok = false;
          } else {
            entry.duration_as_string = v;
          }
        }
        const up = typeof one.upstream_model === "string" ? one.upstream_model.trim() : "";
        if (up) entry.upstream_model = up;

        // 已知模型的协议不可改（后端同样会拒）：改了会让提交走错信封，
        // 表现成「上游拒绝」而不是「配置错」，排查成本极高。
        const known = DUOMI_KNOWN_MODELS[id];
        if (known && protocol && protocol !== known.protocol) {
          errors[`${ovErr}.${id}.protocol`] =
            `${id} 属于 ${known.protocol} 协议，不能改成 ${protocol}（新增型号请换一个对外模型 ID）`;
          ok = false;
        }
        if (ok) out[id] = entry;
      }
      if (Object.keys(out).length > 0) value.model_overrides = out;
    }
  }

  // 体积上限只在没有其它错误时才提示（否则运营会被两条互相掩盖的消息绕晕）
  if (Object.keys(errors).length === 0) {
    const bytes = new TextEncoder().encode(JSON.stringify(value)).length;
    if (bytes > DUOMI_MAX_OPTIONS_BYTES) {
      errors.adapter_options = `配置体积 ${bytes} 字节，超过上限 ${DUOMI_MAX_OPTIONS_BYTES}（请减少模型覆盖条目）`;
    }
  }

  return { value, errors };
}

/**
 * 路径的**非阻断**提示：`kling.prefix` 末尾不是 `/` 时任务号会直接贴上去。
 *
 * 不做成 error：后端只要求「以 / 开头、不含 ?#」，若上游真用非斜杠分隔符，
 * 前端硬拒会挡住合法配置。这里只把后果说清楚。
 */
export function duomiKlingPrefixHint(prefix: string): string {
  const v = prefix.trim();
  if (!v || v === DUOMI_DEFAULT_PATHS["kling.prefix"] || v.endsWith("/")) return "";
  return `末尾不是 /：查询时会拼成 ${v}<任务号>，请确认上游确实是这个形状`;
}

/** 关闭某个模型覆盖时的提示语（UI 用，避免运营误以为「清空文本框 = 恢复默认」） */
export function duomiOverridesHint(): string {
  return "留空 = 全部使用内置档案；只填需要覆盖的模型 ID。protocol 决定提交/查询信封，填错会一直 400。";
}
