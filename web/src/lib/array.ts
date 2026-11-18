/**
 * asArray 把「接口可能下发 null 的数组」收敛成数组。
 *
 * 为什么需要它：后端约定「对外数组必须是 []」，但历史上有过 null；一旦前端
 * 直接 `.map()` / `.length` 就是整页白屏（本项目已在文档组栽过两次）。
 *
 * 为什么不放在 apiClient 里统一归一：`data: null`（删除类接口）与
 * `used_at: string | null` 都有语义，浅层归一要么误伤、要么只覆盖第一层；
 * 而且会把「接口少给数组」变成无声通过，问题暴露点更远。归一放在渲染边界，
 * 类型不撒谎、出错能定位到具体字段。
 */
export function asArray<T>(v: readonly T[] | null | undefined): T[] {
  return Array.isArray(v) ? (v as T[]) : [];
}
