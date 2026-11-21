import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";
import { inputTypeOf, isMultiline, type FormValues } from "@/lib/pluginTypes";
import type { PluginField } from "@/types";

/**
 * 字段描述 → 控件。插件表单、插件配置、分组表单共用**这一个**字段渲染器。
 *
 * 四条约定（改之前先读）：
 *  - 控件宽度归容器（X14）：这里不写 width，列宽由 `.plugin-form__grid` 决定。
 *  - 不做前端语义校验：错误从服务端 `problems[].path` 回来，经 `errors` 传进来，
 *    落在 `Field` 的 `hint` 上（`hint` 是"常驻小字"，藏进气泡等于让人盲改）。
 *  - `help` 是字段说明（填什么），与 `hint`（错了什么）分工不同：同时存在时
 *    错误优先显示 —— 出错时用户要看的是原因，不是又读一遍说明。
 *  - `readOnly` 只影响渲染，**不是安全边界**（服务端判据仍是 manifest 与安装状态）。
 */
export function SchemaField({
  field: f,
  value,
  error,
  idPrefix,
  disabled,
  onChange,
}: {
  field: PluginField;
  value: unknown;
  error?: string;
  idPrefix: string;
  disabled?: boolean;
  onChange: (key: string, value: unknown) => void;
}) {
  const id = `${idPrefix}-${f.key}`;
  const locked = disabled || f.readOnly;
  // 说明与错误共用同一行位置：错误优先。
  const help = error ? undefined : f.help;

  if (f.type === "bool") {
    return (
      <div className="plugin-form__bool">
        <SwitchRow
          id={id}
          label={f.label}
          checked={value === true}
          disabled={locked}
          onChange={(v) => onChange(f.key, v)}
        />
        {help ? <p className="plugin-form__help">{help}</p> : null}
        {error ? (
          <p className="plugin-form__help plugin-form__help--warn">{error}</p>
        ) : null}
      </div>
    );
  }

  if (f.type === "enum") {
    return (
      <Field label={f.label} req={f.required} htmlFor={id} hint={error} hintWarn={!!error}>
        <Select
          id={id}
          items={f.options ?? []}
          allLabel={f.required ? undefined : "未选择"}
          value={String(value ?? "")}
          disabled={locked}
          onChange={(e) => onChange(f.key, e.target.value)}
        />
        {help ? <p className="plugin-form__help">{help}</p> : null}
      </Field>
    );
  }

  if (isMultiline(f)) {
    return (
      <Field label={f.label} req={f.required} htmlFor={id} hint={error} hintWarn={!!error}>
        <Textarea
          id={id}
          rows={4}
          maxLength={f.maxLen || undefined}
          placeholder={f.placeholder || undefined}
          value={String(value ?? "")}
          disabled={locked}
          onChange={(e) => onChange(f.key, e.target.value)}
        />
        {help ? <p className="plugin-form__help">{help}</p> : null}
      </Field>
    );
  }

  return (
    <Field label={f.label} req={f.required} htmlFor={id} hint={error} hintWarn={!!error}>
      <Input
        id={id}
        type={inputTypeOf(f)}
        // 只有单行文本才吃 maxLen：日期/数字的 maxLength 没有意义，
        // 而给它们加会让手机键盘与原生校验互相打架。
        maxLength={f.type === "string" ? f.maxLen || undefined : undefined}
        // min/max 只对整数有意义（manifest 也只允许 int 声明）。
        min={f.type === "int" && f.min !== undefined ? f.min : undefined}
        max={f.type === "int" && f.max !== undefined ? f.max : undefined}
        placeholder={f.placeholder || undefined}
        value={String(value ?? "")}
        disabled={locked}
        onChange={(e) => onChange(f.key, e.target.value)}
      />
      {help ? <p className="plugin-form__help">{help}</p> : null}
    </Field>
  );
}

/** 扁平字段表（插件配置用：config 没有分段声明）。 */
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
      {fields.map((f) => (
        <SchemaField
          key={f.key}
          field={f}
          value={values[f.key]}
          error={errors[f.key]}
          idPrefix={idPrefix}
          disabled={disabled}
          onChange={onChange}
        />
      ))}
    </div>
  );
}
