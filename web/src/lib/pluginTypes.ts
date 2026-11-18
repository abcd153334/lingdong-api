import type { PluginEntity, PluginField } from "@/types";

/**
 * 插件渲染的纯逻辑层（无 React）。
 *
 * 边界：这里**不做语义校验** —— 必填、长度、枚举、唯一性的唯一判据在服务端
 * (`service/appplugins`)。前端只在提交后把服务端返回的 `problems[].path`
 * 贴到对应字段下面。任何"前端也校验一遍"的写法都会在同一份规则上分叉，
 * 而分叉的表现是"前端说没问题、服务端拒绝"，比不校验更难排查。
 */

/** 表单值：文本类用 string，布尔用 boolean，其他保持原样（提交时再转）。 */
export type FormValues = Record<string, unknown>;

/** 输入框的 HTML type（int 用 number 只是为了手机上弹数字键盘，校验仍在服务端）。 */
export function inputTypeOf(f: PluginField): "text" | "number" | "date" | "url" {
  switch (f.type) {
    case "int":
      return "number";
    case "date":
      return "date";
    case "link":
      return "url";
    default:
      return "text";
  }
}

/** 是否多行输入 */
export function isMultiline(f: PluginField): boolean {
  return f.type === "text";
}

/** 把已保存的记录摊成表单值（缺失的字段留空，不是 undefined）。 */
export function toFormValues(fields: PluginField[], data: Record<string, unknown> | undefined): FormValues {
  const out: FormValues = {};
  for (const f of fields) {
    const v = data?.[f.key];
    if (f.type === "bool") {
      out[f.key] = v === true;
    } else if (v === undefined || v === null) {
      out[f.key] = f.default === undefined || f.default === null ? "" : String(f.default);
    } else {
      out[f.key] = String(v);
    }
  }
  return out;
}

/**
 * 表单值 → 请求体。
 *
 * 空串一律不下发（服务端把"空串 / null / 不传"当同一件事，但少传一个键
 * 可以让"改这条记录时把某字段清空"这件事在请求里看得见）。
 */
export function toPayload(e: PluginEntity, values: FormValues): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of e.fields) {
    const raw = values[f.key];
    if (f.type === "bool") {
      out[f.key] = raw === true;
      continue;
    }
    const s = typeof raw === "string" ? raw.trim() : raw === undefined || raw === null ? "" : String(raw);
    if (s === "") continue;
    out[f.key] = f.type === "int" ? Number(s) : s;
  }
  return out;
}

/** 单元格文本（表格与"复制"共用一份口径）。 */
export function cellText(type: string, v: unknown): string {
  if (v === undefined || v === null || v === "") return "—";
  if (type === "bool") return v === true ? "是" : "否";
  return String(v);
}

/** 列渲染需要的类型判断（link 要渲染成 <a>） */
export function isLinkColumn(type: string): boolean {
  return type === "link";
}

/** 只读展示用的字段列表顺序：与 manifest 声明顺序一致（不排序、不隐藏）。 */
export function fieldOf(e: PluginEntity, key: string): PluginField | undefined {
  return e.fields.find((f) => f.key === key);
}

/**
 * 服务端 problems 的 path 形如 `data.ticketNo` / `config.slaHours`，
 * 这里剥掉前缀拿到字段 key；不是字段级问题返回空串（走顶部汇总）。
 */
export function fieldKeyOfPath(path: string, prefix: string): string {
  if (!path.startsWith(prefix)) return "";
  const rest = path.slice(prefix.length);
  return rest.includes(".") || rest.includes("[") ? "" : rest;
}
