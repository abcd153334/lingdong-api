import type { FieldErrors, FieldValues, Resolver } from "react-hook-form";
import type { ZodType, ZodTypeDef } from "zod";

/**
 * 最小 zod → react-hook-form resolver 适配（替代 @hookform/resolvers，不新增依赖）。
 * 仅走 safeParse 同步路径，把 zod issues 归并为 { fieldPath: { message } }；
 * 无 path 的 issue（superRefine 的整表校验）落到 RHF 的 `root` 键。
 */
export function zodResolver<TValues extends FieldValues>(
  schema: ZodType<TValues, ZodTypeDef, unknown>,
): Resolver<TValues> {
  return async (values) => {
    const parsed = schema.safeParse(values);
    if (parsed.success) {
      return { values: parsed.data as TValues, errors: {} };
    }
    const errors: Record<string, { type: string; message: string }> = {};
    for (const issue of parsed.error.issues) {
      const key = issue.path.length ? issue.path.map(String).join(".") : "root";
      if (!errors[key]) errors[key] = { type: issue.code, message: issue.message };
    }
    return { values: {}, errors: errors as unknown as FieldErrors<TValues> };
  };
}

/** 密码强度等级：0=空，1=弱，2=中，3=强 */
export type Strength = 0 | 1 | 2 | 3;

export const STRENGTH_LABELS = ["—", "弱", "中", "强"] as const;

/** 密码强度评分（对齐原型 strength()：不足 8 位封顶为「弱」） */
export function passwordStrength(password: string): Strength {
  const p = password ?? "";
  if (!p) return 0;
  let score = 0;
  if (p.length >= 8) score += 1;
  if (/[A-Z]/.test(p) && /[a-z]/.test(p)) score += 1;
  if (/\d/.test(p)) score += 1;
  if (/[^A-Za-z0-9]/.test(p)) score += 1;
  const level = p.length < 8 ? Math.max(1, score - 2) : score;
  return Math.min(3, Math.max(1, level)) as Strength;
}
