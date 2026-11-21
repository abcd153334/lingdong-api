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
 * 把服务端描述归一成"这几项一定有"的形状。
 *
 * 为什么需要它（2026-11-18 真实事故）：前端是热更新、后端要重启，两者**不同步**
 * 是常态。新增 `list.sort` / `list.filters` / `form` 之后，页面直接读
 * `list.sort[0]` 与 `entity.form.sections`，对着一台还没重启的后端就是
 * `Cannot read properties of undefined` —— 整个弹窗被错误边界接住、白屏。
 *
 * 归一规则（缺字段 = 能力更少，而不是崩）：
 *  - `sort` / `filters` 缺失 → 空数组：表头不可排序、工具栏不出筛选器；
 *  - `form` 缺失 → **单段单列、按字段声明顺序**，与后端"不声明 form"时的投影逐字一致；
 *  - `columns` / `actions` 缺失（更老的版本）→ 空数组：表格退化成"什么都没有"，
 *    但仍能读到顶部的错/空态，而不是白屏。
 *
 * 类型上的"必填"是**归一之后**的契约：调用方只经过这里拿描述，就不会碰到半截数据。
 */
export function normalizeEntity(raw: PluginEntity): PluginEntity {
  const e = raw as Partial<PluginEntity> & Pick<PluginEntity, "fields">;
  const fields = e.fields ?? [];
  const list = e.list ?? ({} as PluginEntity["list"]);
  const sections = e.form?.sections;
  return {
    ...(raw as PluginEntity),
    fields,
    list: {
      columns: list.columns ?? [],
      actions: list.actions ?? [],
      sort: list.sort ?? [],
      filters: list.filters ?? [],
    },
    form:
      sections && sections.length > 0
        ? { sections }
        : {
            sections:
              fields.length > 0
                ? [{ title: "", columns: 1 as const, fields: fields.map((f) => f.key) }]
                : [],
          },
  };
}

/** 批量归一（插件详情里的 entities） */
export function normalizeEntities(raw: PluginEntity[] | undefined): PluginEntity[] {
  return (raw ?? []).map(normalizeEntity);
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
