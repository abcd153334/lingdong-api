import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { SchemaFields } from "@/components/plugins/SchemaFields";
import {
  cellText,
  fieldKeyOfPath,
  inputTypeOf,
  isMultiline,
  toFormValues,
  toPayload,
} from "@/lib/pluginTypes";
import type { PluginField } from "@/types";

const FIELDS: PluginField[] = [
  { key: "ticketNo", label: "单号", type: "string", required: true, maxLen: 32 },
  { key: "count", label: "次数", type: "int", required: false, maxLen: 0 },
  { key: "done", label: "已完成", type: "bool", required: false, maxLen: 0 },
  { key: "status", label: "状态", type: "enum", required: true, maxLen: 0, options: ["open", "done"] },
  { key: "detail", label: "详情", type: "text", required: false, maxLen: 200 },
];

/**
 * 纯逻辑的口径只有一条：**空串不下发、int 转数字、bool 恒下发**。
 *
 * 这三条是"改记录时把某字段清空"能被服务端看懂的前提：
 * 空串与"不传"在服务端是同一件事，但少传一个键能让请求体自己说清楚。
 */
describe("pluginTypes", () => {
  it("toPayload 去掉空值、保留 false、int 转数字", () => {
    const values = toFormValues(FIELDS, { ticketNo: "T-1", count: 3, done: false });
    expect(values.ticketNo).toBe("T-1");
    expect(values.done).toBe(false);

    const payload = toPayload({ key: "e", name: "E", unique: "", fields: FIELDS, list: { columns: [], actions: [] }, public: [] }, values);
    expect(payload).toEqual({ ticketNo: "T-1", count: 3, done: false });
  });

  it("清空字段：空串不出现在请求体里", () => {
    const values = toFormValues(FIELDS, { ticketNo: "T-1", detail: "写过的备注" });
    values.detail = "";
    const payload = toPayload({ key: "e", name: "E", unique: "", fields: FIELDS, list: { columns: [], actions: [] }, public: [] }, values);
    expect("detail" in payload).toBe(false);
  });

  it("cellText：布尔写是/否，空值写破折号", () => {
    expect(cellText("bool", true)).toBe("是");
    expect(cellText("bool", false)).toBe("否");
    expect(cellText("string", "")).toBe("—");
    expect(cellText("int", 0)).toBe("0");
  });

  it("控件类型与多行判定", () => {
    expect(inputTypeOf(FIELDS[1])).toBe("number");
    expect(inputTypeOf(FIELDS[0])).toBe("text");
    expect(isMultiline(FIELDS[4])).toBe(true);
  });

  it("problems 的 path 只认字段级", () => {
    expect(fieldKeyOfPath("data.ticketNo", "data.")).toBe("ticketNo");
    expect(fieldKeyOfPath("data", "data.")).toBe("");
    expect(fieldKeyOfPath("config.slaHours", "data.")).toBe("");
  });
});

describe("SchemaFields", () => {
  it("枚举渲染成下拉、布尔渲染成开关、错误贴在该字段下", () => {
    const values = toFormValues(FIELDS, {});
    render(
      <SchemaFields
        fields={FIELDS}
        values={values}
        errors={{ status: "只能是 open / done 之一" }}
        idPrefix="t"
        onChange={() => {}}
      />,
    );
    // 枚举的选项来自 manifest（不是前端硬编码）
    expect(screen.getByRole("option", { name: "open" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "done" })).toBeTruthy();
    expect(screen.getByLabelText("已完成")).toBeTruthy();
    expect(screen.getByText("只能是 open / done 之一")).toBeTruthy();
  });
});
