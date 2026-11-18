/**
 * 请求体 JSON 的两个纯函数：校验与排版。
 *
 * 两个调试面板（`/docs` 在线运行、`/admin/api-docs` 试运行）都要「手写整个 JSON 体」，
 * 规则必须一模一样：一边允许尾逗号、另一边拒绝，读者会以为是接口的问题。
 * 因此实现只在这里，别处一律引用。
 */

/**
 * 语法检查。合法（或空）返回 null，否则返回一句能显示给人看的错。
 *
 * `JSON.parse` 抛的是浏览器实现相关的英文串（Chrome 是
 * `Unexpected token } in JSON at position 5`，Firefox 又不同），直接显示等于把
 * 内部细节丢给读者。这里只给"去查什么" —— 位置信息靠编辑器自己高亮。
 */
export function jsonBodyError(text: string): string | null {
  const t = text.trim();
  if (t === "") return null;
  try {
    JSON.parse(t);
    return null;
  } catch {
    return "JSON 语法错误：检查括号、引号是否成对，以及最后一项后面有没有多余的逗号。";
  }
}

/**
 * 排版成 2 空格缩进。
 *
 * 非法时**原样返回**：这是"格式化"按钮用的，读者正在写的中间态本来就可能不合法，
 * 这里清空或报错都只会让人丢掉刚敲的内容。
 */
export function prettyJson(text?: string): string {
  const t = (text ?? "").trim();
  if (t === "") return "";
  try {
    return JSON.stringify(JSON.parse(t), null, 2);
  } catch {
    return text ?? "";
  }
}
