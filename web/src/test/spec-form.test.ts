import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

import { buildSpecPrices, emptySpec, specIsFree, specToForm, type SpecKind } from "@/lib/specForm";
import type { Modality, SpecPrices } from "@/types";

/**
 * 规格价表单的**往返契约测试**。
 *
 * 数据源是仓库根 `testdata/spec_payloads.json` —— 后端
 * `controller/admin_model_spec_test.go` 读的是同一个文件。
 * 于是「什么是一个合法的 spec_prices」只有一份定义：
 * 后端证明它存得进去，前端证明它能原样还原。
 *
 * 这条测试要防的是历史上真实发生过的那类事故：
 *   `specToForm` 按原值读（String(sp.max_n)），`buildSpec` 按金额写（microOf ×1e6），
 *   往返一次 4 → 4000000，图片模型每次保存都被后端 `1–128` 拒绝。
 * 只测其中一侧永远发现不了 —— 两边都是"绿"的。
 */

type Fixture = { name: string; modality: Modality; spec: SpecPrices };

const FILE = path.resolve(process.cwd(), "../testdata/spec_payloads.json");
const cases: Fixture[] = JSON.parse(readFileSync(FILE, "utf8")) as Fixture[];

const byName = (name: string): Fixture => {
  const c = cases.find((x) => x.name === name);
  if (!c) throw new Error(`fixture 里没有 ${name}`);
  return c;
};

describe("spec 表单往返", () => {
  for (const c of cases) {
    it(`回填 → 提交 必须原样还原：${c.name}`, () => {
      const form = specToForm(c.spec, c.spec.kind as SpecKind);
      const r = buildSpecPrices(form, c.modality);
      expect(r.ok).toBe(true);
      if (!r.ok) return;
      // 深等：多一个键、少一个键、值差一位都算错
      expect(r.value).toEqual(c.spec);
    });
  }
});

describe("数量字段绝不做金额换算（×1e6 回归）", () => {
  it("视频：max_seconds / ref_image_free / long_after_seconds 按原值提交", () => {
    const base = specToForm(byName("video_full").spec, "per_video");
    const r = buildSpecPrices(
      { ...base, max_seconds: "37", ref_image_free: "3", long_after_seconds: "20" },
      "video",
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.max_seconds).toBe(37);
    expect(r.value.ref_image_free).toBe(3);
    expect(r.value.long_after_seconds).toBe(20);
  });

  it("视频：超时倍率按微比例存（1.5 → 1500000）", () => {
    const base = specToForm(byName("video_full").spec, "per_video");
    const r = buildSpecPrices({ ...base, long_video_multiplier: "1.5" }, "video");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.long_video_multiplier).toBe(1_500_000);
  });

  it("图片：max_n 按原值提交（4 就是 4 张）", () => {
    const base = specToForm(byName("image_minimal").spec, "per_image");
    const r = buildSpecPrices({ ...base, max_n: "7" }, "image");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.max_n).toBe(7);
  });

  it("音频：max_seconds 按原值提交", () => {
    const base = specToForm(byName("audio_second").spec, "per_second");
    const r = buildSpecPrices({ ...base, max_seconds: "120" }, "audio");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.max_seconds).toBe(120);
  });
});

describe("智能时长开关（布尔，不是金额也不是秒数）", () => {
  const autoForm = () => specToForm(byName("video_auto_duration").spec, "per_video");

  it("库内 true → 表单 \"true\" → 提交 true", () => {
    const form = autoForm();
    expect(form.allow_auto_duration).toBe("true");
    const r = buildSpecPrices(form, "video");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.allow_auto_duration).toBe(true);
  });

  it("关掉后**整键不出现**（不是写 false）", () => {
    // spec_prices 是整列 JSON 覆盖写：写 false 与不写等价，但多一个假值会让
    // 「到底哪些模型开了智能时长」的筛选/对账多一层歧义。
    const r = buildSpecPrices({ ...autoForm(), allow_auto_duration: "" }, "video");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect("allow_auto_duration" in r.value).toBe(false);
  });

  it("库内 null / 缺失一律显示为关", () => {
    const minimal = byName("video_minimal").spec;
    expect(specToForm(minimal, "per_video").allow_auto_duration).toBe("");
    expect(
      specToForm({ ...minimal, allow_auto_duration: null }, "per_video").allow_auto_duration,
    ).toBe("");
  });
});

describe("区间校验（前端与后端同界）", () => {
  const videoForm = () => specToForm(byName("video_full").spec, "per_video");
  const imageForm = () => specToForm(byName("image_minimal").spec, "per_image");

  it("max_n 超过 128 被拦", () => {
    const r = buildSpecPrices({ ...imageForm(), max_n: "999" }, "image");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("1–128");
  });

  it("max_seconds 超过 3600 被拦", () => {
    const r = buildSpecPrices({ ...videoForm(), max_seconds: "9999" }, "video");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("1–3600");
  });

  it("ref_image_free 超过上限被拦（后端此前没有上界，会静默漏收）", () => {
    const r = buildSpecPrices({ ...videoForm(), ref_image_free: "2000000" }, "video");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("0–128");
  });

  it("张数/秒数不接受小数", () => {
    const r = buildSpecPrices({ ...imageForm(), max_n: "4.5" }, "image");
    expect(r.ok).toBe(false);
  });
});

describe("默认档", () => {
  it("默认档不在已配档位里必须被拦", () => {
    const form = {
      ...emptySpec("per_image"),
      resolution_fixed: "8K",
      tier_price: { "1K": "0.2" },
      max_n: "4",
    };
    const r = buildSpecPrices(form, "image");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("不在已配置的档位里");
  });

  it("默认档命中档位时原样写出（不参与金额计算）", () => {
    const form = {
      ...emptySpec("per_image"),
      resolution_fixed: "2K",
      tier_price: { "1K": "0", "2K": "0" },
      max_n: "4",
    };
    const r = buildSpecPrices(form, "image");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.resolution_fixed).toBe("2K");
  });

  it("未配置默认档时放行（后端不会替用户挑档，不传 size 的请求会被拒绝）", () => {
    const form = {
      ...emptySpec("per_image"),
      tier_price: { "1K": "0.2" },
      max_n: "4",
    };
    const r = buildSpecPrices(form, "image");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.resolution_fixed).toBeUndefined();
  });
});

describe("必填上限", () => {
  it("max_n 留空必须被拦：它是单次最坏金额与结算上限的依据", () => {
    const form = {
      ...emptySpec("per_image"),
      tier_price: { "1K": "0.2" },
      max_n: "",
    };
    const r = buildSpecPrices(form, "image");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("张数上限");
  });

  it("max_seconds 留空必须被拦（视频）", () => {
    const base = specToForm(byName("video_minimal").spec, "per_video");
    const r = buildSpecPrices({ ...base, max_seconds: "" }, "video");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("最大秒数");
  });

  it("max_seconds 留空必须被拦（音频）", () => {
    const base = specToForm(byName("audio_second").spec, "per_second");
    const r = buildSpecPrices({ ...base, max_seconds: "" }, "audio");
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.error).toContain("最大秒数");
  });

  it("新建图片模型不预设张数上限（留空 = 引擎上限）", () => {
    // 曾经的默认值是 "4"，它会把组图（最多 15 张）与图层拆分（需 17 张）
    // 直接卡死，而运营不会去动这个看起来像业务决策的数字。
    expect(emptySpec("per_image").max_n).toBe("");
  });

  it("免费价目可以保存（0 是显式免费，不是未配置）", () => {
    const form = {
      ...emptySpec("per_image"),
      tier_price: { "1K": "0" },
      max_n: "1",
    };
    const r = buildSpecPrices(form, "image");
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.value.tier_price).toEqual({ "1K": 0 });
  });
});

describe("specIsFree：0 价确认的判据", () => {
  const base: SpecPrices = { kind: "per_image", tier_price: { "1K": 0 }, max_n: 1 };

  it("价目全为 0 → 完全免费，必须二次确认", () => {
    expect(specIsFree(base)).toBe(true);
  });

  it("有任意一档非 0 → 不是免费（免费档 + 付费档是合法策略）", () => {
    expect(specIsFree({ ...base, tier_price: { "1K": 0, "4K": 36000 } })).toBe(false);
  });

  it("档位全 0 但最低消费非 0 → 不是免费：确认框不能说「不产生任何扣费」", () => {
    expect(
      specIsFree({ kind: "per_second", per_second_price: 0, min_price: 10000, max_seconds: 60 }),
    ).toBe(false);
  });

  it("视频参考视频每秒价也算在内：漏了它会把「只对参考视频收费」误判成免费", () => {
    expect(
      specIsFree({
        kind: "per_video",
        second_price: { "720p": 0 },
        ref_video_second_price: { "720p": 6000 },
        max_seconds: 10,
      }),
    ).toBe(false);
  });

  it("没有任何价格字段 → 不算免费（那是未配置，保存会被拦）", () => {
    expect(specIsFree({ kind: "per_task" })).toBe(false);
  });
});
