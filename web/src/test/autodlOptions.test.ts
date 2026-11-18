import { describe, expect, it } from "vitest";
import {
  AUTODL_RESOLUTION_DEFAULTS,
  AUTODL_WORKFLOW_DEFAULTS,
  autodlOptionsToForm,
  buildAutoDLOptions,
  emptyAutoDLForm,
} from "@/lib/autodlOptions";

describe("AutoDL 私有配置表单", () => {
  it("空表单产出显式全量文档（默认工作流 + 默认档位映射）", () => {
    const r = buildAutoDLOptions(emptyAutoDLForm());
    expect(r.errors).toEqual({});
    expect(r.value).toEqual({
      workflows: { ...AUTODL_WORKFLOW_DEFAULTS },
      resolution_map: { ...AUTODL_RESOLUTION_DEFAULTS },
    });
  });

  it("每次调用返回全新对象（避免抽屉实例互相污染）", () => {
    const a = emptyAutoDLForm();
    const b = emptyAutoDLForm();
    a.workflows.multimodal = "changed";
    a.resolution["720p"] = "480p";
    expect(b.workflows.multimodal).toBe(AUTODL_WORKFLOW_DEFAULTS.multimodal);
    expect(b.resolution["720p"]).toBe(AUTODL_RESOLUTION_DEFAULTS["720p"]);
  });

  it("空工作流就地报错（后端「非空即全量」，静默补默认是最难发现的错）", () => {
    const form = emptyAutoDLForm();
    form.workflows.text = "   ";
    const r = buildAutoDLOptions(form);
    expect(r.errors["workflows.text"]).toContain("请填写");
  });

  it("工作流 ID 字符集非法时拒绝", () => {
    for (const bad of ["wf/1", "工作流", "a b", "x".repeat(65)]) {
      const form = emptyAutoDLForm();
      form.workflows.image = bad;
      const r = buildAutoDLOptions(form);
      expect(r.errors["workflows.image"], bad).toContain("1-64 位");
    }
  });

  it("四个工作流不得重复（同一 ID 服务两种模式必有一种跑错）", () => {
    const form = emptyAutoDLForm();
    form.workflows.first_last = AUTODL_WORKFLOW_DEFAULTS.image;
    const r = buildAutoDLOptions(form);
    expect(r.errors["workflows.first_last"]).toContain("重复");
  });

  it("分辨率取值白名单（空 = 不支持，合法）", () => {
    const form = emptyAutoDLForm();
    form.resolution["1080p"] = "";
    let r = buildAutoDLOptions(form);
    expect(r.errors).toEqual({});
    expect((r.value.resolution_map as Record<string, string>)["1080p"]).toBe("");

    form.resolution["480p"] = "1440p";
    r = buildAutoDLOptions(form);
    expect(r.errors["resolution_map.480p"]).toContain("480p");
  });

  it("回填：null / 非对象 / 缺字段都落默认，且不整体丢弃", () => {
    expect(autodlOptionsToForm(null)).toEqual(emptyAutoDLForm());
    expect(autodlOptionsToForm("oops")).toEqual(emptyAutoDLForm());
    // 只配了 workflows、没配 resolution_map：不能把已有工作流冲回默认
    const form = autodlOptionsToForm({
      workflows: { multimodal: "wf_mm", image: "wf_img", text: "wf_txt", first_last: "wf_fl" },
    });
    expect(form.workflows.multimodal).toBe("wf_mm");
    expect(form.resolution).toEqual(AUTODL_RESOLUTION_DEFAULTS);
  });

  it("回填：非法单值被丢弃并保留默认（脏数据不白屏）", () => {
    const form = autodlOptionsToForm({
      workflows: { multimodal: 123, image: "wf_img" },
      resolution_map: { "720p": "1440p", "1080p": "" },
    });
    expect(form.workflows.multimodal).toBe(AUTODL_WORKFLOW_DEFAULTS.multimodal);
    expect(form.workflows.image).toBe("wf_img");
    expect(form.resolution["720p"]).toBe(AUTODL_RESOLUTION_DEFAULTS["720p"]);
    // 空串是合法取值（= 不支持），必须原样保留
    expect(form.resolution["1080p"]).toBe("");
  });

  it("往返：回填 → 构建保持原值", () => {
    const doc = {
      workflows: { multimodal: "wf_mm", image: "wf_img", text: "wf_txt", first_last: "wf_fl" },
      resolution_map: { "480p": "768p", "720p": "768p", "1080p": "1080p" },
    };
    const r = buildAutoDLOptions(autodlOptionsToForm(doc));
    expect(r.errors).toEqual({});
    expect(r.value).toEqual(doc);
  });
});
