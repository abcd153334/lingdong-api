import { useState } from "react";

import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Textarea } from "@/components/common/Textarea";
import type { DocSamples } from "@/types/docsadmin";

/**
 * 示例代码卡：四段文本，按语言切换编辑。
 *
 * 四条决定：
 *  1. **语言用胶囊切换而不是子页签**：四段是同一件事的四种写法，不是四个视图；
 *     页签会让「另一段没写」变得不可见。
 *  2. 一次只显示一段（textarea）：四段并排会把右栏撑成横向滚动。
 *  3. 里面只写 `{{base_url}}`，域名在投影期替换 —— 写死域名等于换域名后
 *     整站示例都在教用户调旧地址。
 *  4. 「生成」按当前字段示例值重算，结果**只回到输入框**（不落库）：
 *     点一下生成就改库等于给示例代码开了第二条写路径。
 */
const LANGS: [keyof DocSamples, string][] = [
  ["curl", "cURL"],
  ["python", "Python"],
  ["node", "Node"],
  ["go", "Go"],
];

export function SamplesCard({
  title,
  hint,
  value,
  onChange,
  onGenerate,
  generating,
  onClear,
}: {
  /** 段落标题（"示例代码" / "第二种写法"） */
  title: string;
  hint?: string;
  value: DocSamples | null;
  onChange: (v: DocSamples | null) => void;
  onGenerate?: () => void;
  generating?: boolean;
  onClear?: () => void;
}) {
  const [lang, setLang] = useState<keyof DocSamples>("curl");
  const cur = value ?? { curl: "", python: "", node: "", go: "" };
  const filled = LANGS.filter(([k]) => cur[k].trim() !== "").length;

  return (
    <div className="adp-samples">
      <div className="adp-samples__hd">
        <span className="adp-samples__t">{title}</span>
        {hint ? <span className="adp-samples__hint">{hint}</span> : null}
        <div className="adp-samples__acts">
          {onGenerate ? (
            <button
              type="button"
              className="btn btn-sm"
              disabled={generating}
              onClick={onGenerate}
              title="按当前字段的示例值重新生成四段代码"
            >
              <Icon name="refresh" size={14} className={generating ? "adp-spin" : undefined} />
              生成
            </button>
          ) : null}
          {onClear ? (
            <button type="button" className="btn btn-ghost btn-sm" onClick={onClear}>
              清空
            </button>
          ) : null}
        </div>
      </div>

      <SegmentedChips
        items={LANGS.map(([k, label]) => ({
          key: k,
          label: cur[k].trim() ? label : `${label} ·`,
        }))}
        value={lang}
        onChange={(k) => setLang(k as keyof DocSamples)}
      />

      <Textarea
        className="adp-code"
        rows={10}
        value={cur[lang]}
        aria-label={`${title} · ${LANGS.find(([k]) => k === lang)?.[1] ?? lang}`}
        placeholder={
          filled === 0
            ? "点「生成」按当前字段示例值生成，或直接粘贴。地址一律写 {{base_url}}。"
            : undefined
        }
        onChange={(e) => onChange({ ...cur, [lang]: e.target.value })}
      />
    </div>
  );
}

/** 第二种写法的标题（页面上只显示有内容的那一段）。 */
export function ExampleTitleInput({
  value,
  onChange,
}: {
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <Input
      className="adp-title-input"
      value={value}
      placeholder="这段示例的场景名（如「流式输出」；留空则不显示标题）"
      aria-label="示例标题"
      onChange={(e) => onChange(e.target.value)}
    />
  );
}
