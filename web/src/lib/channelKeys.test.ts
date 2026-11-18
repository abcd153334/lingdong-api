import { describe, expect, it } from "vitest";

import { MAX_KEYS, firstBlockingIssue, parseKeyLines } from "@/lib/channelKeys";

/**
 * 密钥粘贴解析的行为契约。
 *
 * 这一层是「新建渠道」与「密钥池面板」共用的唯一入口：
 * 两处各写一份时，典型分叉是「新建能拦住脱敏串、面板拦不住」，
 * 而面板恰好是改线上渠道的那一处。
 */
describe("parseKeyLines", () => {
  it("只按换行切分，逗号属于密钥本身", () => {
    // 按 ,; 切会把一把带逗号的密钥截成两把废 key，而报错只会是 401。
    const r = parseKeyLines("sk-a,b\nsk-c;d");
    expect(r.keys).toEqual(["sk-a,b", "sk-c;d"]);
    expect(r.issues).toEqual([]);
  });

  it("CRLF 与空行不产生噪音", () => {
    const r = parseKeyLines("sk-a\r\n\r\n  \r\nsk-b\r\n");
    expect(r.keys).toEqual(["sk-a", "sk-b"]);
    expect(r.issues).toEqual([]);
  });

  it("超长报错并定位到行", () => {
    const r = parseKeyLines(`sk-a\nsk-${"x".repeat(1024)}`);
    expect(r.keys).toEqual(["sk-a"]);
    const issue = firstBlockingIssue(r.issues);
    expect(issue?.line).toBe(2);
    expect(issue?.message).toContain("1024");
  });

  it("内嵌空格/制表符报错（粘贴错位的典型形态）", () => {
    const r = parseKeyLines("sk-a sk-b\nsk-c\tsk-d");
    expect(r.keys).toEqual([]);
    expect(r.issues.map((x) => x.line)).toEqual([1, 2]);
    expect(r.issues.every((x) => x.blocking)).toBe(true);
  });

  it("重复行不阻断，但要说清与第几行重复", () => {
    const r = parseKeyLines("sk-a\nsk-b\nsk-a");
    expect(r.keys).toEqual(["sk-a", "sk-b"]);
    const dup = r.issues.find((x) => x.line === 3);
    expect(dup?.blocking).toBe(false);
    expect(dup?.message).toContain("第 1 行");
  });

  it("大小写不同不算重复（与 HMAC 口径一致）", () => {
    const r = parseKeyLines("sk-A\nsk-a");
    expect(r.keys).toEqual(["sk-A", "sk-a"]);
    expect(r.issues).toEqual([]);
  });

  it("脱敏串必须拦：它是面板上最容易被复制的东西", () => {
    for (const masked of ["sk-abc…wxyz", "sk-abc...wxyz", "sk-abc****wxyz"]) {
      const r = parseKeyLines(masked);
      expect(r.keys).toEqual([]);
      expect(firstBlockingIssue(r.issues)?.message).toContain("脱敏");
    }
  });

  it("超过 100 把整批拒绝，且报实际值", () => {
    const raw = Array.from({ length: MAX_KEYS + 1 }, (_, i) => `sk-${i}`).join("\n");
    const r = parseKeyLines(raw);
    expect(r.keys).toHaveLength(MAX_KEYS + 1);
    const issue = firstBlockingIssue(r.issues);
    expect(issue?.line).toBe(0);
    expect(issue?.message).toContain("101");
  });
});
