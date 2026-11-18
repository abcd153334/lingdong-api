import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import type { DocSamples, DocsAdminPreset } from "@/types/docsadmin";

import { KeyValueEditor, LinesField } from "./Bits";
import { SamplesCard } from "./SamplesCard";

/**
 * 一种调用方式（预设）：同一路径的另一种用法。
 *
 * 每个预设带**自己的**示例代码：选中一种用法，读者要的是这个用法的可执行示例，
 * 而不是主示例复制四份。
 */
export function PresetCard({
  preset,
  index,
  total,
  onChange,
  onMove,
  onRemove,
  onGenerate,
  generating,
}: {
  preset: DocsAdminPreset;
  index: number;
  total: number;
  onChange: (p: DocsAdminPreset) => void;
  onMove: (dir: -1 | 1) => void;
  onRemove: () => void;
  onGenerate: () => void;
  generating: boolean;
}) {
  const patch = (p: Partial<DocsAdminPreset>) => onChange({ ...preset, ...p });
  return (
    <div className="adp-preset">
      <div className="adp-preset__hd">
        <Input
          value={preset.label}
          placeholder="这个用法的名字（如「图生图」）"
          aria-label="调用方式的名字"
          onChange={(e) => patch({ label: e.target.value })}
        />
        <Input
          value={preset.key}
          placeholder="标识（英文短横线）"
          aria-label="调用方式的标识"
          onChange={(e) => patch({ key: e.target.value })}
        />
        <div className="adp-preset__acts">
          <button type="button" className="icon-btn" aria-label="上移" disabled={index === 0} onClick={() => onMove(-1)}>
            <Icon name="chevronUp" size={14} />
          </button>
          <button
            type="button"
            className="icon-btn"
            aria-label="下移"
            disabled={index === total - 1}
            onClick={() => onMove(1)}
          >
            <Icon name="chevronDown" size={14} />
          </button>
          <button type="button" className="icon-btn adp-danger" aria-label="删除这个调用方式" onClick={onRemove}>
            <Icon name="trash" size={14} />
          </button>
        </div>
      </div>

      <div className="adp-grid adp-grid--2">
        <label className="adp-mini">
          <span>一句话说明</span>
          <Input value={preset.summary} onChange={(e) => patch({ summary: e.target.value })} />
        </label>
        <label className="adp-mini">
          <span>备注（显示在示例下面）</span>
          <Input value={preset.note} onChange={(e) => patch({ note: e.target.value })} />
        </label>
      </div>

      <div className="adp-sub">
        <div className="adp-sub__t">请求体</div>
        <KeyValueEditor value={preset.body} onChange={(body) => patch({ body })} />
      </div>

      <div className="adp-grid adp-grid--2">
        <label className="adp-mini">
          <span>涉及字段（一行一个）</span>
          <LinesField value={preset.fields} rows={3} ariaLabel="涉及字段" onChange={(fields) => patch({ fields })} />
        </label>
        <label className="adp-mini">
          <span>需要上传文件的字段（一行一个）</span>
          <LinesField value={preset.uploads} rows={3} ariaLabel="上传字段" onChange={(uploads) => patch({ uploads })} />
        </label>
      </div>

      <SamplesCard
        title="这个用法的示例代码"
        hint="按这个用法的请求体生成"
        value={preset.samples}
        onChange={(s: DocSamples | null) => patch({ samples: s })}
        onGenerate={onGenerate}
        generating={generating}
        onClear={() => patch({ samples: null })}
      />
    </div>
  );
}

/** 预设的默认值（新建时给一个能直接改的壳，而不是空对象）。 */
export function blankPreset(): DocsAdminPreset {
  return {
    key: "",
    label: "",
    summary: "",
    body: [],
    fields: [],
    uploads: [],
    note: "",
    samples: null,
  };
}
