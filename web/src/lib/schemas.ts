import { z } from "zod";

import { microOf } from "@/lib/money";
import { passwordStrength } from "@/lib/form";

export const USERNAME_RE = /^[A-Za-z0-9_]{3,20}$/;
export const CODE_RE = /^\d{6}$/;

const rootIssue = (ctx: z.RefinementCtx, message: string) =>
  ctx.addIssue({ code: z.ZodIssueCode.custom, message });

/**
 * 登录：只判非空。
 *
 * 账号可以是用户名 / 邮箱 / 手机号三种形态，前端**不判定**它属于哪一种
 * （判定在服务端 findByLogin 一处做）；密码强度之类的规则也不在这里判 ——
 * 登录只负责把用户输入送到服务端，多一套规则只会产生「前端先报错、
 * 服务端本来能过」的假失败。
 */
export const loginSchema = z
  .object({ account: z.string(), password: z.string() })
  .superRefine((v, ctx) => {
    if (!v.account.trim() || !v.password) rootIssue(ctx, "请输入账号和密码");
  });
export type LoginForm = z.infer<typeof loginSchema>;

/**
 * 注册：用户名 / 联系方式 / 密码 / 确认密码 / 邀请码。
 *
 * 这里的规则是**提交前的快速反馈**，权威判定在服务端 service/auth：
 * - 用户名形态（3-20 位、至少一个字母或下划线）与服务端同一规则；
 * - 密码长度与强度同服务端 ValidatePassword（8–72 字节、四类里命中两类）。
 * 唯一不做前端判断的是「邮箱是否已被占用」「邀请码是否有效」这类需要查库的规则。
 */
export const registerSchema = z
  .object({
    username: z.string(),
    target: z.string(),
    code: z.string(),
    aff_code: z.string(),
    password: z.string(),
    confirm_password: z.string(),
  })
  .superRefine((v, ctx) => {
    if (!USERNAME_RE.test(v.username.trim())) {
      rootIssue(ctx, "用户名需 3-20 位字母、数字或下划线，且至少含一个字母或下划线");
    } else if (v.target.trim() && !CODE_RE.test(v.code.trim())) {
      rootIssue(ctx, "请填写联系方式收到的 6 位验证码");
    } else if (v.password.length < 8) {
      rootIssue(ctx, "密码至少 8 位");
    } else if (passwordStrength(v.password) < 2) {
      rootIssue(ctx, "密码强度不足：请混合大小写 / 数字 / 符号");
    } else if (v.password !== v.confirm_password) {
      rootIssue(ctx, "两次输入的密码不一致");
    }
  });
export type RegisterForm = z.infer<typeof registerSchema>;

/**
 * 忘记密码（三步共用一份表单状态）。
 *
 * 第 1 步填 target（邮箱/手机号）、第 2 步填 code、第 3 步填新密码；
 * 每步只校验自己那几个字段，因此这里不做「全部必填」式的整表校验 ——
 * 那会让第 1 步的提交按钮永远处于校验失败状态。
 */
export const resetPasswordSchema = z
  .object({
    target: z.string(),
    code: z.string(),
    new_password: z.string(),
    confirm_password: z.string(),
  })
  .superRefine((v, ctx) => {
    const target = v.target.trim();
    const touchingStep3 = v.new_password !== "" || v.confirm_password !== "";
    if (!touchingStep3) return;
    if (!target) {
      rootIssue(ctx, "请先填写接收验证码的邮箱或手机号");
    } else if (!CODE_RE.test(v.code.trim())) {
      rootIssue(ctx, "请输入 6 位数字验证码");
    } else if (v.new_password.length < 8) {
      rootIssue(ctx, "新密码至少 8 位");
    } else if (passwordStrength(v.new_password) < 1) {
      rootIssue(ctx, "新密码强度不足");
    } else if (v.new_password !== v.confirm_password) {
      rootIssue(ctx, "两次输入的新密码不一致");
    }
  });
export type ResetPasswordForm = z.infer<typeof resetPasswordSchema>;

/** 修改密码（用户中心抽屉） */
export const changePasswordSchema = z
  .object({
    old_password: z.string(),
    new_password: z.string(),
    confirm_password: z.string(),
  })
  .superRefine((v, ctx) => {
    if (!v.old_password) {
      rootIssue(ctx, "请输入原密码");
    } else if (v.new_password.length < 8) {
      rootIssue(ctx, "新密码至少 8 位");
    } else if (passwordStrength(v.new_password) < 2) {
      rootIssue(ctx, "新密码强度不足");
    } else if (v.new_password !== v.confirm_password) {
      rootIssue(ctx, "两次输入的新密码不一致");
    }
  });
export type ChangePasswordForm = z.infer<typeof changePasswordSchema>;

/**
 * 调账（金额支持负数；结果不得为负）。
 *
 * 入参是**金额文本**（用户按元/美元填），用与后端 `money.Parse` 同规则的
 * `microOf` 解析成微 —— 不接受 `Number()`：那会让 "1.9999999" 静默变成
 * 一个后端解析不了的值，用户看到"调账成功"但余额没有变化。
 */
export function createAdjustQuotaSchema(currentBalance: number) {
  return z
    .object({ amount: z.string(), reason: z.string() })
    .superRefine((v, ctx) => {
      const raw = v.amount.trim();
      const negative = raw.startsWith("-");
      const micro = raw === "" ? null : microOf(raw.replace(/^[-+]/, ""));
      if (micro === null || micro === 0) {
        rootIssue(ctx, micro === null ? "请输入合法的调整金额" : "调整金额不能为 0");
        return;
      }
      if (!v.reason.trim()) {
        rootIssue(ctx, "请填写调账原因");
        return;
      }
      if (currentBalance + (negative ? -micro : micro) < 0) {
        rootIssue(ctx, "调整后余额不能为负");
      }
    });
}
export type AdjustQuotaForm = z.infer<ReturnType<typeof createAdjustQuotaSchema>>;
