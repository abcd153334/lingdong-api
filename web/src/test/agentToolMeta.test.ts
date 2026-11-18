import { describe, expect, it } from "vitest";
import { describeToolArgs, describeToolResult, toolMetaOf } from "@/components/admin/agent/toolMeta";

/**
 * 后端 toolImpls 里的 28 个工具（backend/internal/service/agent/tools.go）。
 *
 * 跨语言没法直接读清单，这里只能硬编码 —— 但至少数量与名字是**显式**的：
 * 后端加工具而这里没加，用例会红，而不是让新工具在界面上显示英文原名。
 */
const TOOLS = [
  "schema_get",
  "spec_support_check",
  "vendor_list",
  "adapter_list",
  "channel_list",
  "channel_get",
  "model_list",
  "model_get",
  "spec_list",
  "spec_get",
  "pricing_basis",
  "docs_read",
  "skill_read",
  "doc_tree",
  "doc_node_get",
  "doc_node_create",
  "doc_node_update",
  "doc_endpoint_get",
  "doc_endpoint_create",
  "doc_endpoint_update",
  "doc_fields_write",
  "doc_check",
  "doc_seed_diff",
  "validate_draft",
  "dry_run_draft",
  "propose_change",
  "submit_unsupported",
];

describe("toolMeta", () => {
  it("28 个工具都有中文名（不是直接把英文名甩给运营）", () => {
    for (const name of TOOLS) {
      const meta = toolMetaOf(name);
      expect(meta.label).not.toBe(name);
      expect(meta.label).toMatch(/[\u4e00-\u9fa5]/);
    }
  });

  it("未登记的工具兜底显示原名，不崩", () => {
    const meta = toolMetaOf("some_new_tool");
    expect(meta.label).toBe("some_new_tool");
    expect(meta.icon).toBe("wrench");
  });

  it("参数摘要只说最有用的一项", () => {
    expect(describeToolArgs("schema_get", '{"section":"endpoints"}')).toBe("端点");
    expect(describeToolArgs("channel_get", '{"id":12}')).toBe("渠道 #12");
    expect(describeToolArgs("docs_read", '{"doc_id":3,"offset":40}')).toContain("第 40 段起");
    expect(describeToolArgs("propose_change", '{"operations":[{},{},{}]}')).toBe("3 项操作");
  });

  it("参数被截断成非法 JSON 时返回空串而不是抛错", () => {
    expect(describeToolArgs("channel_get", '{"id":12,"foo":"bar')).toBe("");
  });

  it("结果摘要：错误优先，其次条数/提案号", () => {
    expect(describeToolResult("channel_list", '{"ok":false,"error":"密钥无效"}', false)).toEqual({
      text: "密钥无效",
      tone: "bad",
    });
    expect(describeToolResult("channel_list", '{"ok":true,"items":[{},{},{}]}', true).text).toBe("3 条");
    expect(describeToolResult("propose_change", '{"ok":true,"proposal_id":11}', true).text).toBe("提案 #11");
    expect(describeToolResult("dry_run_draft", '{"ok":true}', true).text).toBe("探测通过");
  });
});
