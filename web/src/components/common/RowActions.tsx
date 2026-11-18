import { cn } from "@/lib/utils";
import { Icon, type IconName } from "./Icon";

/**
 * 行内动作（表格「操作」列的唯一实现）。
 *
 * 为什么必须收敛成组件：全站曾有 3 套容器（`.row-acts` 换行 / `.ph-acts` 不换行但按钮会被压 /
 * `.ph-acts--wrap`）+ 4 种按钮样式（带框 / 文字链 / ghost-danger / 两者混用），同一个「删除」
 * 在不同页面长相不同，窄屏还会挤成多行。图标是否、尺寸、禁用原因、行点击隔离这些语义
 * 只能在**一处**定义。
 *
 * 四条硬约束（改动前先读）：
 *  1. `label` 必须**逐字等于**原按钮文案：全站 89 处 e2e 用 `getByRole("button",{name})` 断言，
 *     这也是可访问名的唯一来源（`aria-label`）。换图标不换文案。
 *  2. 每行动作 ≤4：动作列宽 = 4×28 + 3×6 + td 左右 padding 28 = 158px。第 5 个动作会把表格
 *     撑宽并挤掉相邻列，故不做「更多」浮层（浮层要处理焦点/滚动/移动端点按，成本远超收益）。
 *  3. 禁用不用原生 `disabled` 的默认表现去挡 hover：`.btn[disabled]{pointer-events:none}` 会让
 *     `title` 永远不显示（"仍被 N 个渠道引用"这类解释等于没写），故本组件给 `[disabled]` 单独
 *     放开 pointer-events（见 components.css `.t-acts .t-act[disabled]`）。原生 disabled 仍
 *     不派发 click，所以放开命中区不会误触发。
 *  4. 点击必须 `stopPropagation`：表格支持 `onRowClick`（打开详情抽屉），不拦就会出现
 *     「点取消 → 任务被取消 + 抽屉被打开」。
 */
export interface RowAction {
  /** 语义图标（IconName 缺字形先在 `Icon.tsx` 补，不要拿近似图标硬凑） */
  icon: IconName;
  /** 动作文案：可访问名 + 悬停提示，必须与旧按钮文案逐字一致 */
  label: string;
  onClick: () => void;
  /** 破坏性动作（删除/作废）：悬停用危险色 */
  tone?: "danger";
  disabled?: boolean;
  /** 禁用原因（取代原来的 title）：解释为什么点不了 */
  disabledReason?: string;
  /**
   * 悬停提示覆盖（按钮仍可点）。用于「点了会被后端拒、但前端不该直接禁用」的动作：
   * 禁用后用户不知道为什么，点击后给理由又太晚 —— 先悬停说明，再允许点。
   */
  hint?: string;
  /** 请求进行中：禁用 + 提示「<label>中…」，避免重复提交 */
  busy?: boolean;
  /** 按行状态隐藏（如 queued 才显示「取消」） */
  hidden?: boolean;
}

/** 动作上限：超出会把动作列撑宽，开发期直接喊出来（生产静默裁剪到前 4 个会隐藏功能） */
const MAX_ACTIONS = 4;

export function RowActions({ actions }: { actions: RowAction[] }) {
  const list = actions.filter((a) => !a.hidden);
  if (!list.length) return null;
  if (import.meta.env.DEV && list.length > MAX_ACTIONS) {
    console.warn(
      `[RowActions] 一行有 ${list.length} 个动作（上限 ${MAX_ACTIONS}）：${list
        .map((a) => a.label)
        .join("/")}。动作列会被撑宽，请合并或改为主从结构。`,
    );
  }
  return (
    <span className="t-acts">
      {list.map((a) => {
        const off = Boolean(a.disabled || a.busy);
        // 禁用原因优先于忙碌态：能解释「为什么不让点」比「正在忙」更有用
        const tip = a.disabled
          ? (a.disabledReason ?? a.hint ?? a.label)
          : a.busy
            ? `${a.label}中…`
            : (a.hint ?? a.label);
        return (
          <button
            key={a.label}
            type="button"
            className={cn("t-act", a.tone === "danger" && "danger")}
            aria-label={a.label}
            title={tip}
            disabled={off}
            onClick={(e) => {
              e.stopPropagation();
              a.onClick();
            }}
          >
            <Icon name={a.icon} size={15} />
          </button>
        );
      })}
    </span>
  );
}
