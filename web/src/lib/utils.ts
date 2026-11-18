import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** 合并 Tailwind 类名（shadcn 约定工具） */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}

/**
 * 穷举守卫：switch 漏了分支时**编译期**报错，而不是运行期静默返回 undefined。
 *
 * 为什么需要它：应用管理弹窗的页签曾用 `default: null` 兜底，于是 `overview`
 * 落在 default 上照样渲染，加页签忘写 case 也不会红（只会显示错内容）。
 * 用法：`default: return assertNever(k)` —— 漏一个 key 就编译不过。
 */
export function assertNever(v: never): never {
  throw new Error(`未处理的分支：${String(v)}`);
}

/**
 * 取「首字」用于无图标时的占位块（唯一实现）。
 *
 * 为什么必须统一：占位块出现在实体图标、站点 logo、用户头像三处，
 * 各处自己写 `slice(0,1)` 的后果是——中英文混排时大小写规则不一致
 * （`api` → `a` vs `A`）、emoji/代理对被截成半个码元（渲染成「�」）。
 *
 * 规则：
 *   - 用 `Array.from` 按**码点**取第一个字符，代理对（emoji、部分生僻字）不会被劈开；
 *   - ASCII 字母转大写（`openai` → `O`），其余原样（中文、日文没有大小写）；
 *   - 空串/纯空白 → fallback（默认 `?`，与既有头像占位一致）。
 */
export function initialOf(name: string | undefined | null, fallback = "?"): string {
  const s = (name ?? "").trim();
  if (!s) return fallback;
  const first = Array.from(s)[0] ?? "";
  // 只对 ASCII 字母做大写：`toUpperCase` 作用在中文/数字上无副作用，
  // 但作用在 `ß` 上会变成两个字符（`SS`），占位块宽度会跳。
  return /^[a-z]$/.test(first) ? first.toUpperCase() : first;
}
