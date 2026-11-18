import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Tag } from "@/components/common/StatusTag";
import { Textarea } from "@/components/common/Textarea";
import { isSecretFilled, paramValue, switchOf, visibleParams } from "@/lib/payChannelSchema";
import type { PayChannelParamSpec, PayChannelSchema } from "@/types";

interface Props {
  schema: PayChannelSchema;
  params: Record<string, string>;
  /** 后端下发的脱敏占位（`******`）；值等于它表示「保持原值」 */
  mask: string;
  /** 虚拟开关当前值（由 PayChannelCard 持有，与卡片的状态标记共用一份） */
  mode: string;
  onModeChange: (mode: string) => void;
  onChange: (key: string, value: string) => void;
}

/**
 * 协议参数表单：**由 schema 驱动**，不再遍历 params 里存了哪些键。
 *
 * 三个刻意的约定：
 *
 *  1. **没有「高级设置」折叠区**。所有字段平铺，顺序即 schema 声明顺序。
 *     原来被折叠的 4 个字段（查单/退款/跳回/异步通知）已经从 schema 移除——
 *     它们都能自动推导，运营填了只会增加出错面。
 *
 *  2. **说明一律走问号**（`Field` 的 `tip`），不用常驻小字。
 *     常驻小字会把一张卡片撑成两屏，而运营真正需要的是「不知道怎么填时点一下」。
 *     `hint` 只留给表单错误，这里没有前端校验，所以完全不传。
 *
 *  3. `secret` 字段**不遮蔽输入过程**。`secret` 只决定落库脱敏与回显占位
 *     （`******`）；输入时用密码点会让人无法核对 PEM 的 BEGIN/END 行，
 *     等于逼运营盲贴。已设置与否由 label 旁的 Tag 表达。
 */
export function PayChannelParams({ schema, params, mask, mode, onModeChange, onChange }: Props) {
  const sw = switchOf(schema);

  const renderField = (p: PayChannelParamSpec) => {
    const value = paramValue(params, p.key);
    const filled = isSecretFilled(value, mask);
    return (
      <Field
        key={p.key}
        label={p.label}
        req={p.required}
        tip={p.tip}
        labelExtra={
          p.secret ? (
            <Tag tone={filled ? "success" : "gray"}>{filled ? "已设置" : "未设置"}</Tag>
          ) : undefined
        }
      >
        {p.multiline ? (
          <Textarea
            className="mono pc-pem"
            value={value}
            placeholder={p.placeholder}
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => onChange(p.key, e.target.value)}
          />
        ) : (
          <Input
            type={p.secret ? "password" : "text"}
            className="mono"
            value={value}
            placeholder={p.placeholder}
            autoComplete="off"
            onChange={(e) => onChange(p.key, e.target.value)}
          />
        )}
      </Field>
    );
  };

  return (
    <>
      {sw ? (
        <Field label={sw.label} className="pc-mode">
          <SegmentedChips
            items={sw.options.map((o) => ({ key: o.value, label: o.label }))}
            value={mode}
            onChange={onModeChange}
          />
        </Field>
      ) : null}
      <div className="pc-params">{visibleParams(schema, params, mode).map(renderField)}</div>
    </>
  );
}
