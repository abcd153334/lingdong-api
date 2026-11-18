import { useState } from "react";
import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { StrengthBar } from "@/components/auth/AuthFields";
import { maskEmail, maskPhone } from "@/lib/format";
import { passwordStrength, zodResolver } from "@/lib/form";
import { bindTargetError } from "@/lib/contactChannel";
import {
  changePasswordSchema,
  createAdjustQuotaSchema,
  registerSchema,
  type ChangePasswordForm,
} from "@/lib/schemas";

describe("mask helpers", () => {
  it("邮箱脱敏：首字符 + *** + @域名；空串原样", () => {
    expect(maskEmail("alice@example.com")).toBe("a***@example.com");
    expect(maskEmail("")).toBe("");
    expect(maskEmail("nope")).toBe("nope");
  });

  it("手机号脱敏：前 3 + **** + 后 4；空串原样", () => {
    expect(maskPhone("13800001234")).toBe("138****1234");
    expect(maskPhone("")).toBe("");
    expect(maskPhone("123")).toBe("123");
  });
});

describe("passwordStrength", () => {
  it("空 → 0；不足 8 位封顶为弱", () => {
    expect(passwordStrength("")).toBe(0);
    expect(passwordStrength("Ab1!")).toBe(1);
  });

  it("长度与字符集共同决定等级", () => {
    expect(passwordStrength("abcdefgh")).toBe(1);
    expect(passwordStrength("abcdefgh1")).toBe(2);
    expect(passwordStrength("Abcdefgh1")).toBe(3);
    expect(passwordStrength("Abcdefgh1!")).toBe(3);
  });
});

describe("zodResolver", () => {
  it("superRefine 的无 path issue 落到 root", async () => {
    const schema = z
      .object({ name: z.string() })
      .superRefine((v, ctx) => {
        if (!v.name) ctx.addIssue({ code: z.ZodIssueCode.custom, message: "名称必填" });
      });
    const resolver = zodResolver<{ name: string }>(schema);
    const result = await resolver({ name: "" }, {}, { fields: {}, shouldUseNativeValidation: false });
    expect(result.errors.root?.message).toBe("名称必填");
  });

  it("字段级 issue 按 path 归并", async () => {
    const schema = z.object({ email: z.string().email("邮箱格式不正确") });
    const resolver = zodResolver<{ email: string }>(schema);
    const result = await resolver(
      { email: "bad" },
      {},
      { fields: {}, shouldUseNativeValidation: false },
    );
    expect(result.errors.email?.message).toBe("邮箱格式不正确");
  });
});

describe("schemas 文案", () => {
  it("registerSchema 按原型顺序返回首条 root 文案", () => {
    const base = { username: "ab", target: "", code: "", aff_code: "", password: "x", confirm_password: "x" };
    const first = (v: typeof base) => registerSchema.safeParse(v).success
      ? ""
      : registerSchema.safeParse(v).error!.issues[0]!.message;
    expect(first(base)).toBe("用户名需 3-20 位字母、数字或下划线，且至少含一个字母或下划线");
    expect(first({ ...base, username: "alice" })).toBe("密码至少 8 位");
    expect(first({ ...base, username: "alice", password: "abcdefgh" })).toBe(
      "密码强度不足：请混合大小写 / 数字 / 符号",
    );
    expect(
      first({ ...base, username: "alice", password: "Abcdefgh1", confirm_password: "Abcdefgh2" }),
    ).toBe("两次输入的密码不一致");
  });

  it("createAdjustQuotaSchema 覆盖空额/空原因/负余额", () => {
    // 余额与调整额都以「微」为单位：100_000_000 微 = ¥100。
    const schema = createAdjustQuotaSchema(100_000_000);
    const msg = (a: string, r: string) =>
      schema.safeParse({ amount: a, reason: r }).error?.issues[0]?.message;
    expect(msg("", "补偿")).toBe("请输入合法的调整金额");
    expect(msg("0", "补偿")).toBe("调整金额不能为 0");
    expect(msg("50", "")).toBe("请填写调账原因");
    expect(msg("-200", "扣减")).toBe("调整后余额不能为负");
    expect(schema.safeParse({ amount: "-100", reason: "扣减" }).success).toBe(true);
  });

  it("bindTargetError 校验目标与一致性", () => {
    expect(bindTargetError("email", "", "bad")).toBe("邮箱格式不正确");
    expect(bindTargetError("email", "a@b.com", "a@b.com")).toBe("新邮箱与当前一致");
    expect(bindTargetError("sms", "", "13800001234")).toBe("");
    expect(bindTargetError("sms", "13800001234", "13800001234")).toBe("新手机号与当前一致");
  });
});

function PasswordProbe() {
  const [message, setMessage] = useState("");
  const form = useForm<ChangePasswordForm>({
    resolver: zodResolver<ChangePasswordForm>(changePasswordSchema),
    defaultValues: { old_password: "", new_password: "", confirm_password: "" },
  });
  return (
    <form
      onSubmit={form.handleSubmit(
        () => setMessage("ok"),
        (errors) => setMessage(errors.root?.message ?? "no-root"),
      )}
    >
      <input aria-label="原密码" {...form.register("old_password")} />
      <button type="submit">提交</button>
      <span>{message}</span>
    </form>
  );
}

describe("表单组件渲染（RHF + zodResolver 集成）", () => {
  it("校验失败把 root 文案回传到表单层", async () => {
    const user = userEvent.setup();
    render(<PasswordProbe />);
    await user.click(screen.getByRole("button", { name: "提交" }));
    await waitFor(() => expect(screen.getByText("请输入原密码")).toBeInTheDocument());
  });
});

describe("AuthFields 渲染", () => {
  // 验证码已改为服务端出图（答案只在 Redis），前端不再有可断言的字符；
  // 这里只保留纯展示组件的用例，验证码逻辑由后端 verify/captcha 用例覆盖。
  it("StrengthBar 三段按等级点亮", () => {
    const { container } = render(<StrengthBar value={2} />);
    expect(container.querySelectorAll(".strength i.on2")).toHaveLength(2);
  });
});
