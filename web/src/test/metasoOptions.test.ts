import { describe, expect, it } from "vitest";
import {
  METASO_DEFAULT_QUERY_PATH,
  METASO_RESOLUTION_DEFAULTS,
  buildMetasoOptions,
  emptyMetasoForm,
  metasoOptionsToForm,
} from "@/lib/metasoOptions";

describe("Metaso 私有配置表单", () => {
  it("空表单：fast_mode 显式 false、四档全开、不下发 context_ir 与 query_path", () => {
    const r = buildMetasoOptions(emptyMetasoForm());
    expect(r.errors).toEqual({});
    expect(r.value).toEqual({
      fast_mode: false,
      resolution_map: { ...METASO_RESOLUTION_DEFAULTS },
    });
    // 两个跟随语义的键不得出现：它们的缺省等于「用上游默认」
    expect("context_ir_enabled" in r.value).toBe(false);
    expect("query_path" in r.value).toBe(false);
  });

  it("每次调用返回全新对象（避免抽屉实例互相污染）", () => {
    const a = emptyMetasoForm();
    const b = emptyMetasoForm();
    a.resolution["720p"] = "";
    a.fastMode = true;
    a.queryPath = "/custom/";
    expect(b.resolution["720p"]).toBe(METASO_RESOLUTION_DEFAULTS["720p"]);
    expect(b.fastMode).toBe(false);
    expect(b.queryPath).toBe("");
  });

  it("fast_mode 永远显式下发（true 与 false 都必须出现在文档里）", () => {
    const on = emptyMetasoForm();
    on.fastMode = true;
    expect(buildMetasoOptions(on).value.fast_mode).toBe(true);
    expect(buildMetasoOptions(emptyMetasoForm()).value.fast_mode).toBe(false);
  });

  it("context_ir 三态：inherit 不下发，on/off 下发布尔", () => {
    const f = emptyMetasoForm();
    expect("context_ir_enabled" in buildMetasoOptions(f).value).toBe(false);
    f.contextIR = "on";
    expect(buildMetasoOptions(f).value.context_ir_enabled).toBe(true);
    f.contextIR = "off";
    expect(buildMetasoOptions(f).value.context_ir_enabled).toBe(false);
  });

  it("档位取值白名单（空 = 未开通，合法且必须能被清空）", () => {
    const f = emptyMetasoForm();
    f.resolution["2k"] = "";
    let r = buildMetasoOptions(f);
    expect(r.errors).toEqual({});
    expect((r.value.resolution_map as Record<string, string>)["2k"]).toBe("");

    f.resolution["480p"] = "720P"; // 大小写不符：上游字面量必须精确
    r = buildMetasoOptions(f);
    expect(r.errors["resolution_map.480p"]).toContain("480P");
  });

  it("四档都必须出现在 resolution_map 里（缺档会让后端沿用默认，运营看不到真实生效值）", () => {
    const r = buildMetasoOptions(emptyMetasoForm());
    expect(Object.keys(r.value.resolution_map as Record<string, string>).sort()).toEqual(
      ["1080p", "2k", "480p", "720p"].sort(),
    );
  });

  it("query_path：与默认值相同则不下发，不同则原样下发", () => {
    const f = emptyMetasoForm();
    f.queryPath = METASO_DEFAULT_QUERY_PATH;
    expect("query_path" in buildMetasoOptions(f).value).toBe(false);

    f.queryPath = "/api/minimax/v3/query/video_generation/";
    const r = buildMetasoOptions(f);
    expect(r.errors).toEqual({});
    expect(r.value.query_path).toBe("/api/minimax/v3/query/video_generation/");
  });

  it("query_path 格式非法时就地报错（三条与后端同口径）", () => {
    const cases: [string, string][] = [
      ["api/v1/query/", "以 / 开头"],
      ["/api/query/?id=/", "? 或 #"],
      ["/api/#frag/", "? 或 #"],
      ["/" + "a".repeat(200) + "/", "200"],
      ["/api/query", "结尾"],
    ];
    for (const [bad, want] of cases) {
      const f = emptyMetasoForm();
      f.queryPath = bad;
      const r = buildMetasoOptions(f);
      expect(r.errors.query_path, bad).toContain(want);
    }
  });

  it("回填：null / 非对象 / 数组都落默认", () => {
    expect(metasoOptionsToForm(null)).toEqual(emptyMetasoForm());
    expect(metasoOptionsToForm("oops")).toEqual(emptyMetasoForm());
    expect(metasoOptionsToForm([])).toEqual(emptyMetasoForm());
    expect(metasoOptionsToForm(null).fastMode).toBe(false);
  });

  it("回填：只配了部分键时不冲掉其它已知值", () => {
    const form = metasoOptionsToForm({
      fast_mode: true,
      context_ir_enabled: false,
      resolution_map: { "2k": "" },
    });
    expect(form.fastMode).toBe(true);
    expect(form.contextIR).toBe("off");
    // 未出现的档位落默认，出现的档位（含空串）原样保留
    expect(form.resolution["2k"]).toBe("");
    expect(form.resolution["480p"]).toBe(METASO_RESOLUTION_DEFAULTS["480p"]);
  });

  it("回填：非法单值被丢弃并保留默认（脏数据不白屏）", () => {
    const form = metasoOptionsToForm({
      fast_mode: "yes", // 非布尔
      resolution_map: { "480p": "720P", "720p": "768P" },
    });
    expect(form.fastMode).toBe(false);
    expect(form.resolution["480p"]).toBe(METASO_RESOLUTION_DEFAULTS["480p"]);
    expect(form.resolution["720p"]).toBe("768P");
  });

  it("回填：非法 query_path 原样保留（丢掉它等于悄悄改回默认路径）", () => {
    const form = metasoOptionsToForm({ query_path: "/api/minimax/v2/query/video_generation" });
    expect(form.queryPath).toBe("/api/minimax/v2/query/video_generation");
    // 保留的代价是保存时被就地拦下（后端 metaso.go 也硬校验尾斜杠）
    expect(buildMetasoOptions(form).errors.query_path).toContain("结尾");
  });

  it("往返：回填 → 构建保持原值", () => {
    const doc = {
      fast_mode: true,
      context_ir_enabled: true,
      resolution_map: { "480p": "", "720p": "768P", "1080p": "1080P", "2k": "2K" },
      query_path: "/api/minimax/v3/query/video_generation/",
    };
    const r = buildMetasoOptions(metasoOptionsToForm(doc));
    expect(r.errors).toEqual({});
    expect(r.value).toEqual(doc);
  });
});
