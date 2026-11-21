import { cn } from "@/lib/utils";
import { SchemaField } from "@/components/plugins/SchemaFields";
import type { FormValues } from "@/lib/pluginTypes";
import type { PluginEntity, PluginField } from "@/types";

/**
 * 分组表单：按 `entity.form.sections` 渲染（分段标题 + 1/2 列）。
 *
 * 三条约定：
 *  - **不自己排字段**：哪一段、几列、顺序如何，全由后端投影下发。前端不做
 *    "把漏掉的字段补到最后"这类推断 —— 那样同一个 manifest 会有两份布局逻辑。
 *  - **两列只在段内生效**：`.plugin-form__grid--2` 是段内网格，不是整页网格；
 *    单列段（长文本、地址、备注）因此不会被邻段的列宽带歪。
 *  - `.plugin-form__bool`（开关行）在两列里横跨整行：半宽开关右侧留白会让人
 *    以为旁边还有一列没加载出来。
 */
export function SchemaForm({
  entity,
  values,
  errors,
  idPrefix,
  disabled,
  onChange,
}: {
  entity: PluginEntity;
  values: FormValues;
  errors: Record<string, string>;
  idPrefix: string;
  disabled?: boolean;
  onChange: (key: string, value: unknown) => void;
}) {
  const byKey = new Map<string, PluginField>(entity.fields.map((f) => [f.key, f]));

  return (
    <div className="plugin-form">
      {entity.form.sections.map((sec, i) => {
        // 投影已保证字段存在；这里再过滤一次是为了"manifest 正在升级"的中间态
        // （旧描述 + 新字段表）不至于渲染出 undefined.label 的控件。
        const fields = sec.fields.map((k) => byKey.get(k)).filter((f): f is PluginField => !!f);
        if (fields.length === 0) return null;
        const grid = (
          <div className={cn("plugin-form__grid", sec.columns === 2 && "plugin-form__grid--2")}>
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
        // 标题为空 = 未声明 form 的实体（单段），不渲染段头。
        if (!sec.title) return <div key={i}>{grid}</div>;
        return (
          <section key={i} className="plugin-form__sec">
            <h4 className="plugin-form__sec-title">{sec.title}</h4>
            {grid}
          </section>
        );
      })}
    </div>
  );
}
