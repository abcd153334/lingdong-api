import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { Textarea } from "@/components/common/Textarea";

import type { ApiDocParam } from "@/types";
import type { ParamValues } from "@/lib/apidocs/request";

export interface FieldInputsProps {
  params: ApiDocParam[];
  values: ParamValues;
  onChange: (name: string, value: string) => void;
}

/** 位置的中文名（与参数表同一份口径）。 */
const IN_LABEL: Record<string, string> = {
  path: "路径",
  query: "查询",
  body: "请求体",
  form: "表单",
  header: "请求头",
};

/**
 * 按契约自动生成输入控件。
 *
 * 控件选择规则（**只看 `type` 与 `enum`，不看参数名**）：
 *   - 有 enum → 下拉（自由输入会让接入方填一个必然被拒的值）
 *   - boolean → 是/否下拉（三个态：不传 / true / false，布尔参数"不传"很常见）
 *   - array   → 多行文本，一行一项（从文档里复制粘贴的天然形态）
 *   - 其余    → 单行文本
 *
 * 为什么不按参数名做特例（比如 `*_id` 用数字框、`password` 用密码框）：
 * 参数名是数据，特例是逻辑，两者会漂移 —— 契约里改个名字，特例就静默失效。
 * 唯一的例外是 `secret` 类字段的遮挡，它按**类型 + 名字后缀**判定且只影响显示。
 */
export function FieldInputs({ params, values, onChange }: FieldInputsProps) {
  if (!params.length) return null;
  return (
    <div className="form-grid">
      {params.map((p) => {
        const label = `${p.name}（${IN_LABEL[p.in] ?? p.in}）`;
        const v = values[p.name] ?? "";
        const set = (next: string) => onChange(p.name, next);
        // 多行/长文本占两列，其余一列：否则一个 models 白名单会把布局挤成一条竖线。
        const wide = p.type === "array" || p.type === "object";

        if (p.enum.length) {
          return (
            <Field key={p.name} label={label} req={p.required === "required"}>
              <Select
                items={p.enum.map((e) => [e, e] as [string, string])}
                allLabel="不传"
                value={v}
                onChange={(ev) => set(ev.target.value)}
              />
            </Field>
          );
        }
        if (p.type === "boolean") {
          return (
            <Field key={p.name} label={label} req={p.required === "required"}>
              <Select
                items={[
                  ["true", "true"],
                  ["false", "false"],
                ]}
                allLabel="不传"
                value={v}
                onChange={(ev) => set(ev.target.value)}
              />
            </Field>
          );
        }
        if (wide) {
          return (
            <Field key={p.name} label={label} req={p.required === "required"} span>
              <Textarea
                rows={3}
                value={v}
                placeholder={p.type === "array" ? "一行一项，也可用逗号分隔" : ""}
                onChange={(ev) => set(ev.target.value)}
              />
            </Field>
          );
        }
        return (
          <Field key={p.name} label={label} req={p.required === "required"}>
            <Input
              value={v}
              // type 只在"看起来是密钥"时改成 password：别的字段遮起来
              // 会让用户没法核对刚粘贴的模型名。
              type={looksSecret(p.name) ? "password" : "text"}
              autoComplete="off"
              // 数字类不设 type=number：它会把 `1.5` 静默截断成 `1`，
              // 而契约里的 integer 校验应该在服务端给出明确拒绝。
              inputMode={p.type === "integer" || p.type === "number" ? "numeric" : undefined}
              placeholder={hintOf(p)}
              onChange={(ev) => set(ev.target.value)}
            />
          </Field>
        );
      })}
    </div>
  );
}

/** 密钥类字段遮挡显示（只影响显示，不影响提交的值）。 */
function looksSecret(name: string): boolean {
  return /(password|secret|token|api_key)$/i.test(name);
}

/**
 * 输入框占位提示：优先用契约里的示例，没有就用默认值。
 *
 * 不猜、不造：占位符里出现一个"看起来像例子"的值，用户会直接拿它去发请求。
 */
function hintOf(p: ApiDocParam): string {
  if (typeof p.example === "string" || typeof p.example === "number") return String(p.example);
  if (p.default) return `默认 ${p.default}`;
  return "";
}
