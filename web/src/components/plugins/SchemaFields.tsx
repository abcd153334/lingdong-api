import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";
import { inputTypeOf, isMultiline, type FormValues } from "@/lib/pluginTypes";
import type { PluginField } from "@/types";

/**
 * 字段描述 → 控件。插件表单与插件配置共用这一个渲染器。
 *
 * 三条约定（改之前先读）：
 *  - 单一列：全站 `.form-grid` 到 ≤359px 才降单列，375px 下两列每列只剩 159px，
 *    URL/日期这类字段会被压到看不见内容。插件表单不继承那个断点，固定单列。
 *  - 不做前端语义校验：错误从服务端 `problems[].path` 回来，经 `errors` 传进来，
 *    落在 `Field` 的 `hint` 上（`hint` 是"常驻小字"，藏进气泡等于让人盲改）。
 *  - 不写 `tip=`：本组件在 `components/**` 下，同样受文案门禁约束（基线 0）。
 */
export function SchemaFields({
  fields,
  values,
  errors,
  idPrefix,
  disabled,
  onChange,
}: {
  fields: PluginField[];
  values: FormValues;
  /** fieldKey → 错误文案（来自服务端 problems） */
  errors: Record<string, string>;
  /** 控件 id 前缀：同一页面里可能有多个表单，不能共用 */
  idPrefix: string;
  disabled?: boolean;
  onChange: (key: string, value: unknown) => void;
}) {
  return (
    <div className="plugin-form">
      {fields.map((f) => {
        const id = `${idPrefix}-${f.key}`;
        const err = errors[f.key];
        if (f.type === "bool") {
          return (
            <SwitchRow
              key={f.key}
              id={id}
              label={f.label}
              checked={values[f.key] === true}
              disabled={disabled}
              onChange={(v) => onChange(f.key, v)}
            />
          );
        }
        if (f.type === "enum") {
          return (
            <Field key={f.key} label={f.label} req={f.required} htmlFor={id} hint={err} hintWarn={!!err}>
              <Select
                id={id}
                items={f.options ?? []}
                allLabel={f.required ? undefined : "未选择"}
                value={String(values[f.key] ?? "")}
                disabled={disabled}
                onChange={(e) => onChange(f.key, e.target.value)}
              />
            </Field>
          );
        }
        if (isMultiline(f)) {
          return (
            <Field key={f.key} label={f.label} req={f.required} htmlFor={id} hint={err} hintWarn={!!err}>
              <Textarea
                id={id}
                rows={4}
                maxLength={f.maxLen || undefined}
                value={String(values[f.key] ?? "")}
                disabled={disabled}
                onChange={(e) => onChange(f.key, e.target.value)}
              />
            </Field>
          );
        }
        return (
          <Field key={f.key} label={f.label} req={f.required} htmlFor={id} hint={err} hintWarn={!!err}>
            <Input
              id={id}
              type={inputTypeOf(f)}
              // 只有单行文本才吃 maxLen：日期/数字的 maxLength 没有意义，
              // 而给它们加会让手机键盘与原生校验互相打架。
              maxLength={f.type === "string" ? f.maxLen || undefined : undefined}
              value={String(values[f.key] ?? "")}
              disabled={disabled}
              onChange={(e) => onChange(f.key, e.target.value)}
            />
          </Field>
        );
      })}
    </div>
  );
}
