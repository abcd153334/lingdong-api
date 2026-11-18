import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { Textarea } from "@/components/common/Textarea";
import type {
  DocsAdminBlock,
  DocsAdminNode,
  DocsAdminNodeData,
  DocsAdminSection,
} from "@/types/docsadmin";

import { Card, LinesField, ToggleLine } from "./Bits";
import { move } from "./VariantCard";

const BLOCK_TYPES: [string, string][] = [
  ["para", "段落"],
  ["note", "提示框"],
  ["steps", "步骤"],
  ["bullets", "要点"],
  ["code", "代码"],
];

/** 节点编辑：标题 / 摘要 / 板块 / 隐藏 / 顺序（+ 说明页的正文块）。 */
export function NodeForm({
  node,
  sections,
  data,
  onChange,
}: {
  node: DocsAdminNode;
  sections: DocsAdminSection[];
  data: DocsAdminNodeData;
  onChange: (d: DocsAdminNodeData) => void;
}) {
  const patch = (p: Partial<DocsAdminNodeData>) => onChange({ ...data, ...p });
  const patchBlock = (i: number, b: Partial<DocsAdminBlock>) => {
    const next = data.blocks.slice();
    next[i] = { ...next[i], ...b };
    patch({ blocks: next });
  };

  return (
    <>
      <Card title="基本信息" icon="doc">
        <label className="adp-mini">
          <span>标题</span>
          <Input value={data.title} onChange={(e) => patch({ title: e.target.value })} />
        </label>
        <label className="adp-mini">
          <span>摘要（卡片上那一句）</span>
          <Textarea rows={2} value={data.summary} onChange={(e) => patch({ summary: e.target.value })} />
        </label>
        <div className="adp-grid adp-grid--2">
          <label className="adp-mini">
            <span>所属板块</span>
            <Select
              items={sections.map((s) => [s.key, s.name] as [string, string])}
              value={data.section}
              onChange={(e) => patch({ section: e.target.value })}
            />
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
          label="隐藏这个节点"
          checked={data.hidden}
          onChange={(hidden) => patch({ hidden })}
        />
      </Card>

      {node.kind === "guide" ? (
        <Card title="正文" icon="book" tip="说明页没有接口，正文就是这一页的全部内容。">
          {data.blocks.length === 0 ? (
            <p className="adp-empty">还没有内容。说明页至少要有一段正文才能发布。</p>
          ) : null}
          {data.blocks.map((b, i) => (
            <div className="adp-block" key={i}>
              <div className="adp-block__hd">
                <Select
                  items={BLOCK_TYPES.map(([v, l]) => [v, l] as [string, string])}
                  value={b.type}
                  aria-label={`第 ${i + 1} 段的类型`}
                  onChange={(e) => patchBlock(i, { type: e.target.value as DocsAdminBlock["type"] })}
                />
                <div className="adp-block__acts">
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label="上移"
                    disabled={i === 0}
                    onClick={() => patch({ blocks: move(data.blocks, i, i - 1) })}
                  >
                    <Icon name="chevronUp" size={14} />
                  </button>
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label="下移"
                    disabled={i === data.blocks.length - 1}
                    onClick={() => patch({ blocks: move(data.blocks, i, i + 1) })}
                  >
                    <Icon name="chevronDown" size={14} />
                  </button>
                  <button
                    type="button"
                    className="icon-btn adp-danger"
                    aria-label="删除这一段"
                    onClick={() => patch({ blocks: data.blocks.filter((_, k) => k !== i) })}
                  >
                    <Icon name="trash" size={14} />
                  </button>
                </div>
              </div>

              {b.type === "steps" || b.type === "bullets" ? (
                <>
                  {b.type === "steps" ? (
                    <Input
                      value={b.title ?? ""}
                      placeholder="这一组的标题（可空）"
                      aria-label="这一组的标题"
                      onChange={(e) => patchBlock(i, { title: e.target.value })}
                    />
                  ) : null}
                  <LinesField
                    value={b.items ?? []}
                    rows={4}
                    ariaLabel="每行一条"
                    placeholder="一行一条"
                    onChange={(items) => patchBlock(i, { items })}
                  />
                </>
              ) : b.type === "code" ? (
                <div className="adp-grid adp-grid--2">
                  <label className="adp-mini">
                    <span>语言</span>
                    <Input
                      value={b.lang ?? ""}
                      placeholder="bash / json"
                      onChange={(e) => patchBlock(i, { lang: e.target.value })}
                    />
                  </label>
                  <label className="adp-mini">
                    <span>代码</span>
                    <Textarea
                      className="adp-code"
                      rows={6}
                      value={b.code ?? ""}
                      onChange={(e) => patchBlock(i, { code: e.target.value })}
                    />
                  </label>
                </div>
              ) : (
                <Textarea
                  rows={4}
                  value={b.text ?? ""}
                  placeholder={b.type === "note" ? "提示框里的那句话" : "段落文字（可用 `反引号` 强调）"}
                  aria-label="段落文字"
                  onChange={(e) => patchBlock(i, { text: e.target.value })}
                />
              )}
            </div>
          ))}
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() => patch({ blocks: [...data.blocks, { type: "para", text: "" }] })}
          >
            <Icon name="plus" size={14} />
            加一段
          </button>
        </Card>
      ) : null}

      <p className="adp-note">
        <Icon name="alert" size={14} />
        节点标识 <code>{node.key}</code> 与类型（{node.kind}）建好后不能改。
      </p>
    </>
  );
}
