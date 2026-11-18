import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 表单控件契约（S22 回归护栏）。
 *
 * 背景：项目没有 input 组件时，样式全靠 components.css 的 `input[type="..."]` 选择器。
 * 漏写 `type` 的裸 `<input>` 会掉出这套选择器，被 Tailwind preflight 重置成零边框元素，
 * 在页面上表现为「和别处完全不是一个风格」——系统配置页「站点与安全」整片输入框就是这样坏的。
 *
 * 本用例把该缺陷钉成可执行契约：src 下任何 `<input>` 必须
 *   ① 显式声明 `type`（含 `type={...}` 动态形式），或
 *   ② 改用共享组件 `<Input>`（其 type 默认 text）。
 */

/** jsdom 环境下 import.meta.url 不是 file: 协议，统一以 vitest 工作目录（web/）定位源码根 */
const SRC = join(process.cwd(), "src");

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) {
      if (name === "test" || name === "node_modules") continue;
      walk(p, out);
    } else if (name.endsWith(".tsx")) {
      out.push(p);
    }
  }
  return out;
}

/** 去注释但保留行数：避免文档里提到的 `<input>` 被当成真实控件 */
function stripComments(code: string): string {
  return code
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
    .replace(/\/\/[^\n]*/g, (m) => " ".repeat(m.length));
}

/** 取出所有 <input ...> 标签体（支持多行与自闭合） */
function inputTags(code: string): { tag: string; line: number }[] {
  const clean = stripComments(code);
  const re = /<input\b(?:(?!\/>).)*?\/?>/gs;
  const tags: { tag: string; line: number }[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(clean)) !== null) {
    tags.push({ tag: m[0], line: clean.slice(0, m.index).split("\n").length });
  }
  return tags;
}

describe("表单控件契约", () => {
  const files = walk(SRC);

  it("扫描范围覆盖页面与组件目录", () => {
    expect(files.length).toBeGreaterThan(20);
  });

  it("不允许出现未声明 type 的裸 <input>（改用 <Input> 组件）", () => {
    const offenders: string[] = [];

    for (const file of files) {
      const code = readFileSync(file, "utf-8");
      for (const { tag, line } of inputTags(code)) {
        const hasType = /(^|\s)type\s*=/.test(tag);
        if (!hasType) {
          offenders.push(`${relative(SRC, file).replace(/\\/g, "/")}:${line}  ${tag.replace(/\s+/g, " ").slice(0, 72)}`);
        }
      }
    }

    // 用字符串断言便于失败时直接看到违规清单
    expect(offenders.join("\n")).toBe("");
  });

  it("Input 组件自身默认 type=text", () => {
    const code = readFileSync(join(SRC, "components/common/Input.tsx"), "utf-8");
    expect(code).toMatch(/type\s*=\s*"text"/);
  });
});
