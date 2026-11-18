import { describe, expect, it } from "vitest";
import { asArray } from "@/lib/array";

// 数组兜底的唯一职责：null/undefined 收敛成 []，其它值原样返回（不复制、不过滤）。
describe("asArray", () => {
  it("null / undefined → 空数组", () => {
    expect(asArray(null)).toEqual([]);
    expect(asArray(undefined)).toEqual([]);
  });

  it("数组原样返回（同一个引用）", () => {
    const src = [{ id: 1 }];
    expect(asArray(src)).toBe(src);
  });

  it("非数组的意外值也不抛错（防后端字段类型漂移）", () => {
    // 后端一旦把数组字段写成字符串，直接 .map() 会炸；这里至少让渲染继续。
    expect(asArray("x" as unknown as string[])).toEqual([]);
  });
});
