import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import type { DocsAdminError } from "@/types/docsadmin";

import { move } from "./VariantCard";

/**
 * 错误码表。
 *
 * `source`（seed/ai/manual）**只显示不可编辑**：它记录「这条是出厂还是人写的」，
 * 保存时必须原样带回，否则每次保存都会把出厂错误码改写成 manual，
 * 这条线索就永久丢了。
 *
 * 列头不是装饰：三列都靠 placeholder 提示时，值一填进去就分不清哪个框是
 * 错误码、哪个是说明（这是改造前的真实状态）。列头只在有数据时出现，
 * 空的表不该先摆一行灰字。
 */
export function ErrorTable({
  value,
  onChange,
}: {
  value: DocsAdminError[];
  onChange: (v: DocsAdminError[]) => void;
}) {
  const patch = (i: number, p: Partial<DocsAdminError>) => {
    const next = value.slice();
    next[i] = { ...next[i], ...p };
    onChange(next);
  };
  return (
    <div className="adp-table">
      {value.length === 0 ? (
        <p className="adp-empty">还没有错误码。读者遇到 4xx 时会来这里找原因。</p>
      ) : (
        <div className="adp-thead adp-thead--err">
          <span>状态码</span>
          <span>错误码</span>
          <span>说明</span>
          <span>来源</span>
          <span />
          <span />
        </div>
      )}
      {value.map((e, i) => (
        <div className="adp-trow adp-trow--err" key={i}>
          <Input
            type="number"
            className="adp-cell-st"
            value={e.status}
            aria-label={`第 ${i + 1} 条错误码的状态码`}
            onChange={(ev) => patch(i, { status: Number(ev.target.value) || 400 })}
          />
          <Input
            className="adp-cell-code adp-mono"
            value={e.code}
            placeholder="如 invalid_request"
            aria-label={`第 ${i + 1} 条错误码`}
            onChange={(ev) => patch(i, { code: ev.target.value })}
          />
          <Input
            className="adp-cell-desc"
            value={e.desc}
            placeholder="什么情况下出现、怎么处理"
            aria-label={`第 ${i + 1} 条错误码的说明`}
            onChange={(ev) => patch(i, { desc: ev.target.value })}
          />
          <span className="adp-err__src" title="来源：出厂 / AI / 人工">
            {e.source === "seed" ? "出厂" : e.source === "ai" ? "AI" : "人工"}
          </span>
          <button
            type="button"
            className="icon-btn adp-cell-up"
            aria-label="上移"
            disabled={i === 0}
            onClick={() => onChange(move(value, i, i - 1))}
          >
            <Icon name="chevronUp" size={14} />
          </button>
          <button
            type="button"
            className="icon-btn adp-danger adp-cell-del"
            aria-label="删除这条错误码"
            onClick={() => onChange(value.filter((_, k) => k !== i))}
          >
            <Icon name="trash" size={14} />
          </button>
        </div>
      ))}
      <button
        type="button"
        className="btn btn-ghost btn-sm"
        onClick={() => onChange([...value, { status: 400, code: "", desc: "", source: "manual" }])}
      >
        <Icon name="plus" size={14} />
        加一条错误码
      </button>
    </div>
  );
}
