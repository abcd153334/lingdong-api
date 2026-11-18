import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

import { MAX_IMAGE_N, MAX_PRICE_SECONDS } from "@/lib/quantity";

/**
 * 单位与口径的**静态门禁**。
 *
 * 这三条检查都不是"再写一遍单测"，而是把「同一个人、同一件事，在四个地方
 * 说的是不是同一句话」变成可执行的断言 —— 前一轮整改出问题，正是因为
 * 这些一致性只存在于人的记忆里。
 */

const WEB = process.cwd();
const SRC = path.join(WEB, "src");
const REPO = path.resolve(WEB, "..");

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(ts|tsx)$/.test(name)) out.push(p);
  }
  return out;
}

const sourceFiles = walk(SRC);
const read = (p: string) => readFileSync(p, "utf8");

describe("量纲门禁", () => {
  it("`numOrNull` 不得复现（它内部是 microOf，名字却不含单位，天生会被误用）", () => {
    // 只禁**定义**：文档里提到它是为了记事故，那是应该保留的。
    const hits = sourceFiles
      .filter((f) => !f.endsWith("units.test.ts"))
      .filter((f) => /(function|const)\s+numOrNull\b/.test(read(f)));
    expect(hits).toEqual([]);
  });

  it("前后端的上限常量必须逐字一致", () => {
    const specfacts = read(path.join(REPO, "backend/internal/service/billing/specfacts.go"));
    const specprices = read(path.join(REPO, "backend/internal/service/billing/specprices.go"));

    const goMaxImageN = /MaxImageN\s*=\s*(\d+)/.exec(specfacts);
    const goMaxSeconds = /MaxPriceSeconds\s*=\s*(\d+)/.exec(specprices);
    expect(goMaxImageN, "后端没有 MaxImageN 常量了？").not.toBeNull();
    expect(goMaxSeconds, "后端没有 MaxPriceSeconds 常量了？").not.toBeNull();

    expect(MAX_IMAGE_N).toBe(Number(goMaxImageN![1]));
    expect(MAX_PRICE_SECONDS).toBe(Number(goMaxSeconds![1]));
  });

  it("后端写出的每个 anomaly 标记都必须有中文文案", () => {
    const anomaly = read(path.join(REPO, "backend/internal/model/anomaly.go"));
    const values = [...anomaly.matchAll(/Anomaly\w+\s*=\s*"([^"]+)"/g)].map((m) => m[1]);
    expect(values.length).toBeGreaterThan(20);

    const charge = read(path.join(SRC, "lib/charge.ts"));
    const missing = values.filter((v) => !new RegExp(`\\b${v}:`).test(charge));
    // 缺一个，用户就会在账单里看到英文码
    expect(missing).toEqual([]);
  });

  it("统计契约（看板/门户趋势）里不得再出现「分」字段", () => {
    // 规则：stats 包对外只下发微。订单表存分是支付渠道的要求，但换算必须在
    // 服务端出口完成 —— 一旦 `recharge_cent` 回到契约里，每个页面都要自己乘一万。
    const types = read(path.join(SRC, "types/index.ts"));
    for (const name of ["BoardToday", "BoardTrendPoint", "UserTrendPoint"]) {
      const m = new RegExp(`export interface ${name} \\{([\\s\\S]*?)\\n\\}`).exec(types);
      expect(m, `types/index.ts 里找不到 ${name}`).not.toBeNull();
      expect(m![1], `${name} 又在声明「分」字段了`).not.toMatch(/\b\w*_cent\b/);
    }

    // 后端也必须同口径（两边任一侧改回去都能被这条挡住）。
    const stats = read(path.join(REPO, "backend/internal/service/stats/stats.go"));
    expect(stats).not.toMatch(/json:"[^"]*_cent"/);
  });

  it("页面不得对统计金额做跨量纲换算", () => {
    // 反例（真实发生过）：`microFromMinor(today.recharge_cent)` —— 服务端已经给了微，
    // 页面再乘一次 10000，毛利直接变成成本的一万分之一。
    const dashboard = read(path.join(SRC, "pages/admin/Dashboard.tsx"));
    expect(dashboard).not.toMatch(/microFromMinor\(today\./);
    expect(dashboard).not.toMatch(/recharge_cent/);
  });
});
