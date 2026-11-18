import { describe, expect, it } from "vitest";

import {
  anomalyLabels,
  capReasonLabel,
  chargeConclusion,
  chargeRows,
  isCapped,
  sourceLabel,
  tierSourceLabel,
} from "@/lib/charge";
import type { PortalCallDetail } from "@/types";

/** 构造最简详情（只填与扣费解释相关的字段）。金额单位一律「微」。 */
function detail(over: Partial<PortalCallDetail>): PortalCallDetail {
  return {
    id: 1,
    key_id: 1,
    created_at: "2026-10-24T00:00:00Z",
    model: "m",
    modality: "chat",
    status: "settled",
    http_status: 200,
    prompt_tokens: 0,
    completion_tokens: 0,
    charge: 0,
    latency_ms: 100,
    is_stream: false,
    ...over,
  } as PortalCallDetail;
}

describe("chargeConclusion", () => {
  it("按 token：只列非零通道", () => {
    const s = chargeConclusion(
      detail({ billing_mode: "tokens", prompt_tokens: 1200, completion_tokens: 300, charge: 1500 }),
    );
    expect(s).toContain("按 token 计费");
    expect(s).toContain("输入 1,200");
    expect(s).toContain("输出 300");
    expect(s).not.toContain("缓存");
  });

  it("按张：给出张数与档位", () => {
    const s = chargeConclusion(
      detail({
        modality: "image",
        billing_mode: "spec",
        image_n: 2,
        image_spec: "1024x1024",
        tier_price: 2000,
        charge: 4000,
      }),
    );
    // 单价走 formatMoney（微 → 币种金额），不再拼接原始微数
    expect(s).toBe("按张计费 · 2 张 × ¥0.002（1024x1024）");
  });

  it("按秒：给出时长与分辨率", () => {
    const s = chargeConclusion(
      detail({
        modality: "video",
        billing_mode: "spec",
        video_seconds: 12,
        video_resolution: "720p",
        tier_price: 900,
        charge: 10800,
      }),
    );
    expect(s).toContain("按秒计费 · 12s × ¥0.0009");
    expect(s).toContain("720p");
  });

  it("按次", () => {
    const s = chargeConclusion(
      detail({ billing_mode: "call", tier_price: 500000, charge: 500000 }),
    );
    expect(s).toBe("按次计费 · 单次 ¥0.5");
  });

  it("失败记录优先说没扣费", () => {
    expect(chargeConclusion(detail({ status: "released", charge: 0 }))).toBe("本次未扣费");
    expect(chargeConclusion(detail({ status: "rejected", charge: 0 }))).toContain("未打到上游");
    expect(chargeConclusion(detail({ status: "pending" }))).toContain("仍在进行中");
  });
});

describe("chargeRows", () => {
  it("全零 token 不产生任何行（去冗余的核心规则）", () => {
    const rows = chargeRows(
      detail({ modality: "image", image_n: 2, prompt_tokens: 0, completion_tokens: 0 }),
    );
    expect(rows.map((r) => r.label)).toEqual(["生成张数"]);
  });

  it("只保留非零通道", () => {
    const rows = chargeRows(
      detail({ prompt_tokens: 10, completion_tokens: 5, cache_tokens: 0, image_tokens: 3 }),
    );
    const labels = rows.map((r) => r.label);
    expect(labels).toContain("输入 tokens");
    expect(labels).toContain("输出 tokens");
    expect(labels).toContain("图片 tokens");
    expect(labels).not.toContain("缓存命中 tokens");
  });
});

describe("口径与术语", () => {
  it("截顶判定", () => {
    expect(isCapped(detail({ charge: 4800, charge_before_cap: 9000 }))).toBe(true);
    expect(isCapped(detail({ charge: 4800, charge_before_cap: 4800 }))).toBe(false);
    expect(isCapped(detail({}))).toBe(false);
  });

  it("异常标记翻中文，未收录的原样返回", () => {
    expect(anomalyLabels("charge_capped,reclaimed")).toEqual(["金额已截顶", "回收补账"]);
    expect(anomalyLabels("brand_new_flag")).toEqual(["brand_new_flag"]);
    expect(anomalyLabels("")).toEqual([]);
  });

  it("术语表与后端枚举对齐", () => {
    expect(sourceLabel("upstream")).toBe("上游实测");
    expect(sourceLabel(undefined)).toBe("未知");
    expect(tierSourceLabel("resolution")).toBe("规格档位");
    expect(capReasonLabel("frozen3x")).toBe("冻结额 3 倍");
  });
});
