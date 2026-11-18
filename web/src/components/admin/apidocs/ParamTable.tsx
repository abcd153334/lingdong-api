import { useMemo } from "react";

import type { ApiDocParam, ApiDocParamIn } from "@/types";

/** 位置的中文名。顺序即表格里的分组顺序。 */
const IN_ORDER: ApiDocParamIn[] = ["path", "query", "body", "form", "header"];
const IN_LABEL: Record<ApiDocParamIn, string> = {
  path: "路径",
  query: "查询",
  body: "请求体",
  form: "表单",
  header: "请求头",
};

/**
 * 参数表。
 *
 * 排序规则：路径 → 查询 → 请求体 → 表单 → 请求头，组内保持契约声明顺序。
 * 为什么不按字母排：接入方是"从上往下照着填"的，路径参数先填、请求体最后写，
 * 这个顺序与他自己写代码的顺序一致；字母序会让他来回跳。
 */
export function ParamTable({ params }: { params: ApiDocParam[] }) {
  const sorted = useMemo(
    () =>
      [...params].sort((a, b) => {
        const d = IN_ORDER.indexOf(a.in) - IN_ORDER.indexOf(b.in);
        // sort 必须稳定：相等时按原顺序（V8 与所有现代引擎都保证，
        // 但显式返回 0 而不是 1，避免有人以为这是"随机顺序"）。
        return d !== 0 ? d : 0;
      }),
    [params],
  );

  return (
    <div className="table-wrap" data-testid="apidoc-params">
      <table className="tbl adx-tbl">
        <thead>
          <tr>
            <th>参数</th>
            <th>位置</th>
            <th>类型</th>
            <th>必填</th>
            <th>说明</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((p) => (
            <tr key={`${p.in}.${p.name}`}>
              <td data-label="参数">
                <span className="nm">{p.name}</span>
              </td>
              <td data-label="位置">{IN_LABEL[p.in] ?? p.in}</td>
              <td data-label="类型">
                <span className="mono">{p.type}</span>
              </td>
              <td data-label="必填">
                <ReqTag p={p} />
              </td>
              <td data-label="说明" className="ds">
                {p.desc}
                {p.note ? <span className="n">{p.note}</span> : null}
                {p.required === "conditional" && p.required_note ? (
                  <span className="n">{p.required_note}</span>
                ) : null}
                <ParamMeta p={p} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** 三态必填徽标。conditional 用"条件必填"+具体条件，不能只写"可选"。 */
function ReqTag({ p }: { p: ApiDocParam }) {
  if (p.required === "required") return <span className="adx-req on">必填</span>;
  if (p.required === "conditional") return <span className="adx-req cd">条件必填</span>;
  return <span className="adx-req">可选</span>;
}

/**
 * 枚举 / 默认值 / 取值约束。
 *
 * 全部渲染成等宽小字：这些是**要原样抄进代码**的值，与说明文字混排会抄错
 * （尤其是 `1` 与 `l`、`0` 与 `O` 在普通字体里几乎一样）。
 */
function ParamMeta({ p }: { p: ApiDocParam }) {
  const hasEnum = p.enum.length > 0;
  const hasExtras = hasEnum || p.default || p.constraints.length > 0;
  if (!hasExtras) return null;
  return (
    <div className="adx-meta">
      {p.default ? <code>默认 {p.default}</code> : null}
      {hasEnum
        ? p.enum.map((v) => (
            <code key={v}>{v}</code>
          ))
        : null}
      {p.constraints.map((c) => (
        <code key={`${c.kind}-${c.value}-${c.text}`}>{c.text}</code>
      ))}
    </div>
  );
}
