import { describe, expect, it } from "vitest";
import { FALLBACK_SCHEMAS } from "@/lib/payChannelSchema";
import { PAY_CHANNEL_SCHEMAS } from "../../e2e/fixtures/payChannelSchemas";

/**
 * 第三份 schema 副本（e2e 桩）的漂移护栏。
 *
 * schema 一共有三份：Go 的 `channelSchemas`（有快照测试）、前端 `FALLBACK_SCHEMAS`
 * （有快照测试）、e2e 桩（**之前完全没有守护**）。桩一旦漂移，e2e 会拿一份
 * 与实际后端不同的声明去渲染，用例仍然全绿，但真机上字段集可能已经对不上。
 *
 * 这里用与 Go / 前端完全相同的签名格式比对，把三份副本都拉进同一张网。
 */
function signature(schemas: readonly any[]): string {
  const lines: string[] = [];
  for (const s of schemas) {
    lines.push(
      `${s.protocol}|${s.label}|${s.method_mode}|${s.implemented ? "true" : "false"}|${(s.methods ?? []).join(",")}`,
    );
    for (const sw of s.switches ?? []) {
      lines.push(`  @switch ${sw.key}:${sw.default}:${sw.options.map((o: any) => `${o.value}=${o.label}`).join(",")}`);
    }
    for (const p of s.params) {
      const flags = [
        p.required ? "req" : "",
        p.secret ? "secret" : "",
        p.hidden ? "hidden" : "",
        p.multiline ? "multiline" : "",
      ]
        .filter(Boolean)
        .join(",");
      let row = `  ${p.key}:${p.group}:${flags}:${p.default ?? ""}`;
      // 显隐条件也要进签名：桩漏了 show_when，e2e 会把两组验签字段同时渲染出来，
      // 用例仍然能过，但和真实后端对不上。
      if (p.show_when) row += `:when=${p.show_when.field}:${p.show_when.value}`;
      lines.push(row);
    }
  }
  return lines.join("\n");
}

describe("e2e schema 桩", () => {
  it("与 FALLBACK_SCHEMAS 形状完全一致", () => {
    expect(signature(PAY_CHANNEL_SCHEMAS)).toBe(signature(FALLBACK_SCHEMAS));
  });

  it("协议顺序一致（默认协议必须是 epay）", () => {
    expect(PAY_CHANNEL_SCHEMAS.map((s) => s.protocol)).toEqual(
      FALLBACK_SCHEMAS.map((s) => s.protocol),
    );
    expect(PAY_CHANNEL_SCHEMAS[0].protocol).toBe("epay");
  });
});
