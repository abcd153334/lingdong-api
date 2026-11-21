import { EntitySelect, type EntityOption } from "@/components/common/EntitySelect";
import type { FilterField } from "@/components/common/FilterBar";
import type { AdminAppOption } from "@/types";

/**
 * 「按应用筛选」的**唯一实现**：四个列表页（订单/日志/任务/用户）共用。
 *
 * 为什么收成一个函数而不是每页写三行：这个筛选有三处容易各自漂移的细节 ——
 *   1. `app_id=0` 不是一个应用，而是「平台直连」（门户自充订单 /
 *      用自建密钥产生的调用与任务）。它的**值必须原样传 "0"**，
 *      被当成空串吞掉就会静默变成"筛全量"；
 *   2. 停用应用也要出现在选项里（历史数据指向它，筛不出来等于对账断链），
 *      但必须标注「（已停用）」，否则运营会以为筛了个空应用；
 *   3. 已生效条件 chip 要显示应用名而不是裸 id（`FilterField.display`）。
 * 任何一页自己抄一遍，这三条里总有一条会漏。
 */

/** 「平台直连」的筛选值（后端 app_id=0）。 */
export const APP_DIRECT_VALUE = "0";
/** 「平台直连」的展示名。 */
export const APP_DIRECT_LABEL = "平台直连";

/** 应用列表 → 选择器选项（对外品牌名优先，停用加后缀）。 */
export function appOptionsOf(items: AdminAppOption[]): EntityOption[] {
  return items.map((a) => ({
    value: String(a.id),
    label: a.display_name || a.name,
    hint: a.status === 2 ? `${a.slug}（已停用）` : a.slug,
  }));
}

/** 筛选值 → 展示名：0 = 平台直连；查不到回落 `#id`（应用被删/选项未加载）。 */
export function appLabelOf(items: AdminAppOption[], value: string): string {
  if (value === APP_DIRECT_VALUE) return APP_DIRECT_LABEL;
  const hit = items.find((a) => String(a.id) === value);
  return hit ? hit.display_name || hit.name : `#${value}`;
}

/**
 * 列表页共用的「应用」筛选项。
 *
 * `withDirect=false` 用于用户列表：用户是全局池，不存在"属于平台直连的用户"，
 * 后端会对 app_id=0 直接 400 —— 下拉里就不该出现这个选项。
 */
export function appFilterField(
  items: AdminAppOption[],
  opts: { withDirect?: boolean } = {},
): FilterField {
  const { withDirect = true } = opts;
  const options: EntityOption[] = withDirect
    ? [{ value: APP_DIRECT_VALUE, label: APP_DIRECT_LABEL }, ...appOptionsOf(items)]
    : appOptionsOf(items);
  return {
    // t 只作为"这是个下拉"的占位（样式与 Sheet 内的 label 关联用它），
    // 真正的控件由 renderer 给：原生 select 放不下图片/搜索，也过不了 M7。
    t: "select",
    id: "fApp",
    label: "应用",
    allLabel: "全部应用",
    renderer: (value, onChange, id) => (
      <EntitySelect
        id={id}
        value={value}
        onChange={onChange}
        options={options}
        searchable
        allLabel="全部应用"
        ariaLabel="应用"
        placeholder="全部应用"
      />
    ),
    display: (v) => appLabelOf(items, v),
  };
}

/**
 * 读取筛选值 → 查询参数。
 *
 * **必须显式判空串**：`Number("") || undefined` 与 `Number(x) || undefined` 都会把
 * `"0"`（平台直连）变成 `undefined`，于是"筛平台直连"静默等于"不筛"。
 */
export function appIdParam(value: string | undefined): number | undefined {
  if (!value) return undefined;
  const n = Number(value);
  return Number.isFinite(n) ? n : undefined;
}
