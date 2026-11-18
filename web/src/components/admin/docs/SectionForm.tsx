import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { Textarea } from "@/components/common/Textarea";
import type { DocsAdminSection, DocsAdminSectionData } from "@/types/docsadmin";

import { Card, ToggleLine } from "./Bits";

/** 板块编辑：名字 / 英文名 / 描述 / 颜色 / 顺序 / 隐藏。 */
const SWATCHES = ["#6366f1", "#3b82f6", "#06b6d4", "#10b981", "#f59e0b", "#ec4899", "#8b5cf6"];

export function SectionForm({
  section,
  data,
  onChange,
}: {
  section: DocsAdminSection;
  data: DocsAdminSectionData;
  onChange: (d: DocsAdminSectionData) => void;
}) {
  const patch = (p: Partial<DocsAdminSectionData>) => onChange({ ...data, ...p });
  return (
    <>
      <Card title="板块" icon="grid" tip="板块是左栏第一层；改名后公开站立刻跟着变。">
        <div className="adp-grid adp-grid--2">
          <label className="adp-mini">
            <span>名称</span>
            <Input value={data.name} onChange={(e) => patch({ name: e.target.value })} />
          </label>
          <label className="adp-mini">
            <span>英文名（标题括号里那个，可空）</span>
            <Input value={data.en} onChange={(e) => patch({ en: e.target.value })} />
          </label>
        </div>
        <label className="adp-mini">
          <span>描述（公开站板块首屏那一句，可空）</span>
          <Textarea rows={3} value={data.desc} onChange={(e) => patch({ desc: e.target.value })} />
        </label>
        <div className="adp-grid adp-grid--2">
          <label className="adp-mini">
            <span>颜色（左栏小圆点）</span>
            <span className="adp-color">
              <Input
                value={data.color}
                placeholder="#6366f1"
                onChange={(e) => patch({ color: e.target.value })}
              />
              {SWATCHES.map((c) => (
                <button
                  key={c}
                  type="button"
                  className="adp-swatch"
                  style={{ background: c }}
                  aria-label={`使用颜色 ${c}`}
                  onClick={() => patch({ color: c })}
                />
              ))}
            </span>
          </label>
          <label className="adp-mini">
            <span>顺序（小的在上）</span>
            <Input
              type="number"
              value={data.sort}
              onChange={(e) => patch({ sort: Number(e.target.value) || 0 })}
            />
          </label>
        </div>
        <ToggleLine
          label="隐藏这个板块"
          checked={data.hidden}
          onChange={(hidden) => patch({ hidden })}
        />
      </Card>

      <p className="adp-note">
        <Icon name="alert" size={14} />
        板块标识 <code>{section.key}</code> 是身份，建好后不能改（节点的归属指向它）。
      </p>
    </>
  );
}
