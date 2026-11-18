/**
 * 渠道级声明式适配配置的表单辅助（纯函数，可单测）。
 *
 * 权威校验在 `backend/internal/relay/declarative.go` 的 ValidateAdapterConfig。
 * 这里**只做**两件事：JSON 语法、以及运营最容易犯的错（值不以 / 开头、缺 content）。
 * 刻意不复制白名单与保留键——两份规则一定会漂移，最终以后端 400 文案为准。
 */
import type { AdapterConfigDoc, AdapterConfigResponsePath } from "@/types";

export interface AdapterConfigForm {
  authHeader: string;
  authPrefix: string;
  /** JSON 文本 */
  pathOverrides: string;
  /** JSON 文本 */
  requestPatch: string;
  /** JSON 文本 */
  responsePath: string;
}

export const EMPTY_ADAPTER_FORM: AdapterConfigForm = {
  authHeader: "",
  authPrefix: "",
  pathOverrides: "",
  requestPatch: "",
  responsePath: "",
};

export type AdapterConfigErrors = Partial<Record<keyof AdapterConfigForm, string>>;

/** 后端 canonical 端点（与 relay.Path* 常量一致，仅用于提示，不参与校验） */
export const CANONICAL_PATHS = [
  "/v1/models",
  "/v1/chat/completions",
  "/v1/completions",
  "/v1/embeddings",
  "/v1/images/generations",
  "/v1/images/edits",
  "/v1/images/variations",
  "/v1/audio/speech",
  "/v1/audio/transcriptions",
  "/v1/audio/translations",
  "/v1/moderations",
  "/v1/rerank",
  "/v1/responses",
  "/v1/video/generations",
  "/v1/video/generations/{id}",
] as const;

function parseObject(text: string, label: string): { value?: Record<string, unknown>; error?: string } {
  const trimmed = text.trim();
  if (!trimmed) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch (e) {
    return { error: `${label}不是合法 JSON：${(e as Error).message}` };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { error: `${label}必须是 JSON 对象（以 { 开头）` };
  }
  return { value: parsed as Record<string, unknown> };
}

/**
 * 归一化鉴权前缀。
 *
 * 后端与插件沙箱**都不自动补空格**（两边语义必须一致，否则换个适配器就 401），
 * 所以防错放在这里：运营填 "Bearer" 时补成 "Bearer "，并在 hint 里说明。
 */
export function normalizePrefix(raw: string): string {
  const v = raw.trim();
  if (!v) return "";
  if (v.toLowerCase() === "none") return "none";
  return v.endsWith(" ") ? v : `${v} `;
}

/** 表单 → 配置。全空返回 null（新建时不带该字段，保持与改造前的请求体一致）。 */
export function buildConfig(form: AdapterConfigForm): {
  config: AdapterConfigDoc | null;
  errors: AdapterConfigErrors;
} {
  const errors: AdapterConfigErrors = {};
  const out: AdapterConfigDoc = {};

  const header = form.authHeader.trim();
  if (header) {
    if (!/^[A-Za-z0-9-]{1,64}$/.test(header)) {
      errors.authHeader = "头名只能是字母/数字/连字符，最长 64";
    } else {
      out.auth_header = header;
    }
  }
  const prefix = normalizePrefix(form.authPrefix);
  if (prefix) out.auth_prefix = prefix;

  const paths = parseObject(form.pathOverrides, "路径覆盖");
  if (paths.error) {
    errors.pathOverrides = paths.error;
  } else if (paths.value && Object.keys(paths.value).length) {
    const bad = Object.entries(paths.value).find(
      ([, v]) => typeof v !== "string" || !(v as string).startsWith("/"),
    );
    if (bad) errors.pathOverrides = `「${bad[0]}」的值必须以 / 开头`;
    else out.path_overrides = paths.value as Record<string, string>;
  }

  const patch = parseObject(form.requestPatch, "请求改写");
  if (patch.error) {
    errors.requestPatch = patch.error;
  } else if (patch.value && Object.keys(patch.value).length) {
    out.request_patch = patch.value;
  }

  const resp = parseObject(form.responsePath, "响应映射");
  if (resp.error) {
    errors.responsePath = resp.error;
  } else if (resp.value && Object.keys(resp.value).length) {
    const content = resp.value.content;
    if (typeof content !== "string" || !content.trim()) {
      errors.responsePath = "必须包含 content（正文字段路径，如 data.answer）";
    } else {
      out.response_path = resp.value as unknown as AdapterConfigResponsePath;
    }
  }

  const empty =
    !out.auth_header && !out.auth_prefix && !out.path_overrides && !out.request_patch && !out.response_path;
  return { config: empty ? null : out, errors };
}

/** 配置 → 表单（后端零值下发 {} 时全部为空）。 */
export function configToForm(cfg?: AdapterConfigDoc | null): AdapterConfigForm {
  const c = cfg ?? {};
  const paths = c.path_overrides && Object.keys(c.path_overrides).length ? c.path_overrides : null;
  const patch = c.request_patch && Object.keys(c.request_patch).length ? c.request_patch : null;
  return {
    authHeader: c.auth_header ?? "",
    // 呈现时去掉尾随空格（input 里看不见，会让人以为没配对）；保存时 normalizePrefix 补回。
    authPrefix: (c.auth_prefix ?? "").replace(/\s+$/, ""),
    pathOverrides: paths ? JSON.stringify(paths, null, 2) : "",
    requestPatch: patch ? JSON.stringify(patch, null, 2) : "",
    responsePath: c.response_path ? JSON.stringify(c.response_path, null, 2) : "",
  };
}

export interface AdapterPreset {
  key: string;
  label: string;
  hint: string;
  form: AdapterConfigForm;
}

/**
 * 场景预设：点击即填，**不保留选中态**。
 *
 * 用 Select 会有"选了 Azure 之后又手改字段、下拉仍显示 Azure"的误导，
 * 所以这里只作为动作按钮。
 */
export const ADAPTER_PRESETS: AdapterPreset[] = [
  { key: "openai", label: "标准 OpenAI", hint: "清空全部高级配置", form: { ...EMPTY_ADAPTER_FORM } },
  {
    key: "path",
    label: "路径不同",
    hint: "例如智谱 /v4/chat/completions",
    form: {
      ...EMPTY_ADAPTER_FORM,
      pathOverrides: '{\n  "/v1/chat/completions": "/v4/chat/completions"\n}',
    },
  },
  {
    key: "azure",
    label: "Azure 风格",
    hint: "api-key 头 + 不加前缀",
    form: { ...EMPTY_ADAPTER_FORM, authHeader: "api-key", authPrefix: "none" },
  },
  {
    key: "token",
    label: "Token 前缀",
    hint: "Authorization: Token <key>",
    form: { ...EMPTY_ADAPTER_FORM, authPrefix: "Token" },
  },
  {
    key: "patch",
    label: "请求体加字段",
    hint: "删 temperature、开 thinking",
    form: {
      ...EMPTY_ADAPTER_FORM,
      requestPatch: '{\n  "temperature": null,\n  "thinking": { "type": "enabled" }\n}',
    },
  },
  {
    key: "resp",
    label: "响应结构不同",
    hint: "从 data.answer 取正文与 token",
    form: {
      ...EMPTY_ADAPTER_FORM,
      responsePath:
        '{\n  "content": "data.answer",\n  "prompt_tokens": "data.usage.input_tokens",\n  "completion_tokens": "data.usage.output_tokens"\n}',
    },
  },
];
