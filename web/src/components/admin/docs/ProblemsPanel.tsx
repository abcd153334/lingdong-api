import { Icon } from "@/components/common/Icon";
import type { DocsAdminProblem } from "@/types/docsadmin";

/**
 * 体检结论。
 *
 * 两条硬约束：
 *  1. **必须能渲染空数组**：这是最常见状态。用「长度为零就整块不渲染」的写法，
 *     会在「刚好没有问题时」把整个面板吞掉，而运营以为体检没跑。
 *  2. 阻断项在前、非阻断在后，各自标明能不能发布 —— 混在一起会让人以为全都拦发布。
 */
export function ProblemsPanel({ problems }: { problems: DocsAdminProblem[] }) {
  const list = [...(problems ?? [])].sort((a, b) => Number(b.blocking) - Number(a.blocking));
  const blocking = list.filter((p) => p.blocking).length;
  return (
    <div className="adp-probs">
      <div className="adp-probs__hd">
        <Icon name={blocking ? "alert" : "check"} size={15} className={blocking ? "adp-danger" : "adp-ok"} />
        <span>{blocking ? `${blocking} 项需要修复，修好才能发布` : "没有阻断项"}</span>
        {list.length > blocking ? (
          <span className="adp-probs__rest">{list.length - blocking} 项建议</span>
        ) : null}
      </div>
      {list.length === 0 ? null : (
        <ul className="adp-probs__list">
          {list.map((p, i) => (
            <li key={`${p.code}-${i}`} className={p.blocking ? "is-block" : undefined}>
              <code>{p.code}</code>
              <span className="adp-probs__field">{p.field}</span>
              <span>{p.message}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
