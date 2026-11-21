import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { SchemaFields } from "@/components/plugins/SchemaFields";
import {
  cellText,
  fieldKeyOfPath,
  inputTypeOf,
  isMultiline,
  normalizeEntities,
  normalizeEntity,
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

    const payload = toPayload({ key: "e", name: "E", unique: "", fields: FIELDS, list: { columns: [], actions: [], sort: [], filters: [] }, form: { sections: [] }, public: [] }, values);
    expect(payload).toEqual({ ticketNo: "T-1", count: 3, done: false });
  });

  it("清空字段：空串不出现在请求体里", () => {
    const values = toFormValues(FIELDS, { ticketNo: "T-1", detail: "写过的备注" });
    values.detail = "";
    const payload = toPayload({ key: "e", name: "E", unique: "", fields: FIELDS, list: { columns: [], actions: [], sort: [], filters: [] }, form: { sections: [] }, public: [] }, values);
    expect("detail" in payload).toBe(false);
  });

  // 描述字段是**叠加式**扩展点：前端热更新、后端要重启，两者不同步是常态。
  // 归一化必须把"缺字段"变成"能力更少"，而不是让 `undefined[0]` 崩掉整个弹窗。
  it("normalizeEntity：旧后端缺 sort/filters/form 时退化成单段单列", () => {
    const legacy = {
      key: "e",
      name: "E",
      unique: "ticketNo",
      fields: FIELDS,
      list: { columns: [{ key: "ticketNo", label: "单号", type: "string" }], actions: ["create"] },
      public: [],
    } as unknown as Parameters<typeof normalizeEntity>[0];

    const e = normalizeEntity(legacy);
    expect(e.list.sort).toEqual([]);
    expect(e.list.filters).toEqual([]);
    expect(e.list.columns).toHaveLength(1);
    // 没有 form ⇒ 全部字段进一个无标题段，顺序与声明一致
    expect(e.form.sections).toHaveLength(1);
    expect(e.form.sections[0].title).toBe("");
    expect(e.form.sections[0].columns).toBe(1);
    expect(e.form.sections[0].fields).toEqual(FIELDS.map((f) => f.key));
  });

  it("normalizeEntity：已有 form 原样保留，不做二次排版", () => {
    const full = {
      key: "e",
      name: "E",
      unique: "",
      fields: FIELDS,
      list: { columns: [], actions: [], sort: [], filters: [] },
      form: { sections: [{ title: "基本信息", columns: 2 as const, fields: ["ticketNo", "status"] }] },
      public: [],
    };
    expect(normalizeEntity(full).form).toEqual(full.form);
  });

  it("normalizeEntities：undefined 归一成空数组（描述还没到不该崩）", () => {
    expect(normalizeEntities(undefined)).toEqual([]);
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
