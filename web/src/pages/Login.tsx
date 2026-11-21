import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useForm, type SubmitErrorHandler } from "react-hook-form";
import { toast } from "sonner";
import { CaptchaField, PasswordField, useCaptcha } from "@/components/auth/AuthFields";
import { sendCodeText } from "@/lib/sendCode";
import { useCountdown } from "@/lib/useCountdown";
import { Field } from "@/components/common/Field";
import { Drawer } from "@/components/common/Drawer";
import { Icon } from "@/components/common/Icon";
import { SiteLogo } from "@/components/common/SiteLogo";
import { ThemeToggle } from "@/components/chrome/ThemeToggle";
import {
  useLoginMFAMutation,
  useLoginMutation,
  usePortalConfigQuery,
  useResetContactCodeMutation,
  useResetPasswordMutation,
  useResetVerifyMutation,
} from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { channelFormatError, channelMeta, useContactChannelPicker } from "@/lib/contactChannel";
import { passwordStrength, zodResolver } from "@/lib/form";
import { loadPortalMode } from "@/lib/portalMode";
import {
  loginSchema,
  resetPasswordSchema,
  type LoginForm,
  type ResetPasswordForm,
} from "@/lib/schemas";
import { useSite } from "@/lib/site";
import { useAuthStore } from "@/stores/auth";

/**
 * 登录错误分支展示文案。
 *
 * 按 `error_code` 判读（机器可判读），**不再按状态码翻译**：
 * 403 在系统里有多个来源（来源校验失败、注册关闭、邀请码、二次验证未绑定、
 * 账号禁用），曾经统一兜底成「账号已被禁用」，于是「请求来源校验失败」
 * 这种刷新一下就能解决的问题被显示成「你的号被封了」。
 * 未识别的码直接透出服务端文案 —— 它比前端猜的准。
 */
function loginErrorText(error: ApiError): string {
  switch (error.errorCode) {
    case "invalid_credential":
      return "账号或密码错误";
    case "user_disabled":
      return "账号已被禁用，请联系管理员";
    case "captcha_invalid":
      return "验证码不正确，请重新输入";
    case "captcha_required":
      return "请先完成人机验证";
    case "rate_limited":
      return "尝试过于频繁，请稍后再试";
    case "auth_unavailable":
      return "认证服务暂不可用，请稍后重试";
    case "csrf_rejected":
      return error.message || "页面已过期，请刷新后重试";
    case "portal_disabled":
      return "门户端已关闭，仅管理员可登录";
    default:
      break;
  }
  return error.message || "登录失败，请稍后重试";
}

const RESET_FORM_ID = "ld-reset-form";

/** 找回密码三步：发码 → 校验换凭据 → 设置新密码 */
type ResetStep = 1 | 2 | 3;

/**
 * 登录页（S2）
 *
 * 人机验证由服务端出图/校验（策略：登录默认「连续失败后要」）；
 * 失败不再前端锁账号倒计时 —— 账号维度的硬锁是攻击者锁死他人账号的开关，
 * 服务端改为「要求验证码 + 随机退避」，只有单 IP 维度才会 429。
 */
export function Login() {
  const navigate = useNavigate();
  const cardRef = useRef<HTMLDivElement>(null);
  const login = useLoginMutation();
  const site = useSite();
  const portalCfg = usePortalConfigQuery();
  const portalConfig = portalCfg.data;
  const auth = portalConfig?.auth;
  /** 运营模式：缺字段 = 开启（与后端 / 守卫同一判据） */
  const portalEnabled = portalConfig?.portal_enabled !== false;
  const captcha = useCaptcha("login");

  const [errMsg, setErrMsg] = useState("");
  const [shake, setShake] = useState(0);
  /**
   * 二次验证挑战。
   *
   * 非空表示「密码已通过、还差第二因素」：此时服务端**没有下发任何 Cookie**，
   * 所以页面必须留在原地并要求输入动态码 —— 不能跳 /home（会被守卫弹回来）。
   */
  const [challenge, setChallenge] = useState("");
  const [otpCode, setOtpCode] = useState("");
  const loginMFA = useLoginMFAMutation();

  const form = useForm<LoginForm>({
    resolver: zodResolver<LoginForm>(loginSchema),
    defaultValues: { account: "", password: "" },
  });

  // 错误行抖动（对齐原型 shake）
  useEffect(() => {
    if (!errMsg) return;
    const el = cardRef.current;
    if (!el) return;
    el.classList.remove("shake");
    void el.offsetWidth;
    el.classList.add("shake");
  }, [shake, errMsg]);

  const showErr = (message: string) => {
    setErrMsg(message);
    setShake((s) => s + 1);
  };

  const onSubmit = (values: LoginForm) => {
    setErrMsg("");
    if (captcha.visible && captcha.answer.trim().length !== 5) {
      showErr("请输入图中 5 位数字");
      return;
    }
    login.mutate(
      {
        account: values.account.trim(),
        password: values.password,
        captcha_id: captcha.visible ? captcha.id || undefined : undefined,
        captcha_answer: captcha.visible ? captcha.answer.trim() || undefined : undefined,
      },
      {
        onSuccess: (outcome) => {
          if (outcome.ok) {
            if ("mfa_required" in outcome.data) {
              // 第一因素通过，转入第二因素：不提示「登录成功」（还没登录完）。
              setChallenge(outcome.data.challenge);
              setOtpCode("");
              setErrMsg("");
              return;
            }
            void finishLogin();
            return;
          }
          // 服务端要求验证码（after_fails 达阈值）→ 把控件亮出来并取图；
          // 答案不对/过期 → 换一张，让用户重填。
          if (outcome.error.errorCode === "captcha_required") {
            captcha.require();
          } else if (outcome.error.errorCode === "captcha_invalid") {
            captcha.refresh();
          }
          showErr(loginErrorText(outcome.error));
        },
      },
    );
  };

  /** 登录完成后的统一收尾：提示 + 落页（关闭门户时只去管理台） */
  const finishLogin = async () => {
    const me = useAuthStore.getState().user;
    // 强制刷新模式：刚把门户关掉时缓存可能仍是 true，
    // 否则会走成「登录 → /home → 403 → 回登录」。
    const open = await loadPortalMode(true);

    // 出厂口令（内置 admin123，写在 README 里）必须先改：
    // 后端对所有业务端点返 403 password_change_required，这里直接送去改密页，
    // 免得用户登录后看到一片"接口报错"。
    if (me?.must_change_password) {
      toast.warning("请先修改初始口令");
      void navigate({ to: open ? "/settings" : "/admin/account" });
      return;
    }
    toast.success("登录成功");
    // 回跳：只接受站内相对路径（`//evil.com` 是协议相对 URL，必须挡掉）。
    // 关闭门户时只认管理台内的回跳 —— `/home` 之类是门户守卫塞进来的，
    // 落地必被守卫再弹一次。
    const next = new URLSearchParams(window.location.search).get("redirect");
    const accepted = open
      ? next && next.startsWith("/") && !next.startsWith("//")
      : next && next.startsWith("/admin/");
    if (accepted && next) {
      window.location.assign(next);
      return;
    }
    void navigate({ to: open ? "/home" : "/admin/dashboard" });
  };

  /** 第二步：拿动态码/备份码换会话 */
  const onSubmitMFA = () => {
    if (!challenge) return;
    const code = otpCode.trim();
    if (!code) {
      showErr("请输入身份验证器上的 6 位动态码，或一个备份码");
      return;
    }
    setErrMsg("");
    loginMFA.mutate(
      { challenge, code },
      {
        onSuccess: (outcome) => {
          if (outcome.ok) {
            setChallenge("");
            void finishLogin();
            return;
          }
          // 挑战过期/试错超限后无法继续：清空并让用户重新走第一步。
          if (outcome.error.errorCode === "mfa_challenge_expired") {
            setChallenge("");
            showErr("验证已超时或尝试次数过多，请重新输入账号密码");
            return;
          }
          showErr(
            outcome.error.errorCode === "mfa_code_invalid"
              ? "动态码或备份码不正确"
              : outcome.error.message || "验证失败，请重试",
          );
        },
      },
    );
  };

  // 校验失败的根文案取自 RHF 传入的 errors（formState 此时尚未同步）
  const onInvalid: SubmitErrorHandler<LoginForm> = (errors) => {
    const message = errors.root?.message;
    if (message) showErr(message);
  };

  /* ------------------------------- 忘记密码 ------------------------------- */
  const [resetOpen, setResetOpen] = useState(false);
  const [step, setStep] = useState<ResetStep>(1);
  const { left, setLeft } = useCountdown();
  const [ticket, setTicket] = useState("");
  // 找回渠道与注册页同一套解析：生效集合（策略 ∩ 就绪）+ 后端给定的优先级。
  // 不再硬编码 "email" —— 那会让「只开短信」的站点默认发到一条走不通的渠道。
  const { channels, meta, select } = useContactChannelPicker();
  const resetCaptcha = useCaptcha("password_reset");
  const sendCode = useResetContactCodeMutation();
  const verifyCode = useResetVerifyMutation();
  const resetPassword = useResetPasswordMutation();

  const resetForm = useForm<ResetPasswordForm>({
    resolver: zodResolver<ResetPasswordForm>(resetPasswordSchema),
    defaultValues: { target: "", code: "", new_password: "", confirm_password: "" },
  });
  const newResetPassword = resetForm.watch("new_password");

  const openReset = () => {
    // 渠道默认值已由 useContactChannelPicker 按配置收敛，这里不再自己 clamp
    setStep(1);
    setResetOpen(true);
  };

  const targetValue = () => resetForm.getValues("target").trim();

  const onSendCode = () => {
    if (left > 0 || !meta) return;
    const target = targetValue();
    const formatError = channelFormatError(meta.value, target);
    if (formatError) {
      toast.error(formatError);
      return;
    }
    sendCode.mutate(
      {
        channel: meta.value,
        target,
        captcha_id: resetCaptcha.visible ? resetCaptcha.id || undefined : undefined,
        captcha_answer: resetCaptcha.visible ? resetCaptcha.answer.trim() || undefined : undefined,
      },
      {
        onSuccess: (data) => {
          setLeft(data.expires_in > 0 ? data.expires_in : 60);
          toast.success("验证码已发送");
          setStep(2);
        },
        onError: (error) => {
          if (error instanceof ApiError && error.errorCode === "captcha_invalid") {
            resetCaptcha.refresh();
          }
          // 配置可能在别处刚改过：拉一次，让界面按新渠道重建，
          // 否则抽屉里只摆着一个走不通的渠道，用户无路可退。
          if (error instanceof ApiError && error.errorCode === "bind_method_disabled") {
            void portalCfg.refetch();
          }
          toast.error(error instanceof ApiError ? error.message : "验证码发送失败");
        },
      },
    );
  };

  /** 第二步：校验验证码，换一次性凭据（服务端只校验不消费，密码不合格不用重收码） */
  const onVerifyCode = () => {
    if (!meta) return;
    const code = resetForm.getValues("code").trim();
    if (!/^\d{6}$/.test(code)) {
      toast.error("请输入 6 位数字验证码");
      return;
    }
    verifyCode.mutate(
      { channel: meta.value, target: targetValue(), code },
      {
        onSuccess: (data) => {
          setTicket(data.reset_ticket);
          setStep(3);
        },
        onError: (error) => {
          toast.error(error instanceof ApiError ? error.message : "验证码校验失败");
        },
      },
    );
  };

  /** 第三步：用一次性凭据设置新密码（凭据只能兑换一次） */
  const submitReset = (values: ResetPasswordForm) => {
    resetPassword.mutate(
      { reset_ticket: ticket, new_password: values.new_password },
      {
        onSuccess: () => {
          toast.success("密码已重置，请使用新密码登录");
          setResetOpen(false);
          setStep(1);
          setTicket("");
          resetForm.reset();
          setLeft(0);
        },
        onError: (error) => {
          // 凭据过期/已用：回到第 1 步重新发码，否则用户会卡在一个永远失败的按钮上
          if (error instanceof ApiError && error.errorCode === "reset_ticket_invalid") {
            toast.error("重置凭据已失效，请重新获取验证码");
            setTicket("");
            setStep(1);
            return;
          }
          toast.error(error instanceof ApiError ? error.message : "密码重置失败");
        },
      },
    );
  };

  const resetInvalid: SubmitErrorHandler<ResetPasswordForm> = (errors) => {
    const message = errors.root?.message;
    if (message) toast.error(message);
  };

  const resetFooter = () => {
    if (step === 1) {
      return (
        <button
          type="button"
          className="btn btn-primary"
          disabled={sendCode.isPending || !meta}
          onClick={onSendCode}
        >
          {sendCode.isPending ? "发送中…" : "发送验证码"}
        </button>
      );
    }
    if (step === 2) {
      return (
        <button type="button" className="btn btn-primary" disabled={verifyCode.isPending} onClick={onVerifyCode}>
          {verifyCode.isPending ? "校验中…" : "下一步"}
        </button>
      );
    }
    return (
      <button type="submit" className="btn btn-primary" form={RESET_FORM_ID} disabled={resetPassword.isPending}>
        重置密码
      </button>
    );
  };

  return (
    <div className="aurora">
      <span className="icon-corner">
        <ThemeToggle />
      </span>
      <div>
        <div ref={cardRef} className="auth-card">
          <div className="brand">
            <SiteLogo />
            {site.name}
          </div>
          <div className="sub-title">
            {portalEnabled ? "多模态模型聚合平台 · 用户门户" : "服务端模式 · 管理控制台登录"}
          </div>
          {!portalEnabled && !challenge ? (
            <div className="small muted">当前为纯服务端模式，仅管理员可登录</div>
          ) : null}

          {challenge ? (
            /* 第二步：密码已通过，等第二因素。此时没有任何 Cookie —— 刷新页面等于放弃这次登录。 */
            <form
              onSubmit={(e) => {
                e.preventDefault();
                onSubmitMFA();
              }}
            >
              <div className="sub-title">两步验证</div>
              <Field label="动态码 / 备份码" req htmlFor="ld-otp">
                <input
                  id="ld-otp"
                  type="text"
                  inputMode="text"
                  autoComplete="one-time-code"
                  autoFocus
                  placeholder="6 位动态码，或 XXXX-XXXX 备份码"
                  value={otpCode}
                  onChange={(e) => setOtpCode(e.target.value)}
                />
              </Field>
              {errMsg && (
                <div className="err-line missing">
                  <Icon name="close" size={14} />
                  {errMsg}
                </div>
              )}
              <button type="submit" className="btn btn-primary" disabled={loginMFA.isPending}>
                {loginMFA.isPending ? "验证中…" : "验 证"}
              </button>
              <div className="alt">
                <a
                  href="#"
                  onClick={(e) => {
                    e.preventDefault();
                    setChallenge("");
                    setOtpCode("");
                    setErrMsg("");
                  }}
                >
                  返回重新登录
                </a>
              </div>
            </form>
          ) : (
          <form onSubmit={form.handleSubmit(onSubmit, onInvalid)}>
            <Field label="账号" req htmlFor="ld-account">
              <input
                id="ld-account"
                type="text"
                placeholder="用户名 / 邮箱 / 手机号"
                autoComplete="username"
                {...form.register("account")}
              />
            </Field>

            <PasswordField
              id="ld-password"
              label="密码"
              placeholder="密码"
              autoComplete="current-password"
              registration={form.register("password")}
            />

            <CaptchaField id="ld-captcha" state={captcha} disabled={login.isPending} />

            {errMsg && (
              <div className="err-line missing">
                <Icon name="close" size={14} />
                {errMsg}
              </div>
            )}

            <button
              type="submit"
              className="btn btn-primary"
              disabled={login.isPending}
            >
              {login.isPending ? "登录中…" : "登 录"}
            </button>
          </form>
          )}

          <div className="alt">
            {/* 没配联系邮箱就不显示这一行：曾经兜底成平台自己的邮箱，
                自建部署会把「问题反馈」指向别人的地址（用户发了也没人管）。 */}
            {site.contactEmail ? (
              <>
                问题反馈：{site.contactEmail}
                <br />
              </>
            ) : null}
            <a
              href="#"
              onClick={(e) => {
                e.preventDefault();
                openReset();
              }}
            >
              忘记密码？
            </a>
            {/* 门户关闭时整段收起：此时系统内没有注册入口，链接点了也只会被守卫弹回。 */}
            {portalEnabled ? (
              <>
                <br />
                还没有账号？
                {auth?.register_mode === "closed" ? (
                  <span className="small muted">本站已关闭注册</span>
                ) : (
                  <Link to="/register">立即注册</Link>
                )}
              </>
            ) : null}
          </div>
        </div>
      </div>

      <Drawer
        open={resetOpen}
        onClose={() => setResetOpen(false)}
        title="重置密码"
        footer={
          <>
            <button type="button" className="btn" onClick={() => setResetOpen(false)}>
              取消
            </button>
            {resetFooter()}
          </>
        }
      >
        {/* channels 为 null 表示配置还没回来：此时不能断言"管理员未启用"。 */}
        {channels !== null && channels.length === 0 && (
          <div className="alert-banner warn">
            管理员未启用任何联系方式通道（邮箱/短信），请联系管理员重置密码。
          </div>
        )}

        <form id={RESET_FORM_ID} onSubmit={resetForm.handleSubmit(submitReset, resetInvalid)}>
          {step === 1 && meta && (
            <>
              {channels && channels.length > 1 && (
                <Field label="接收方式" req>
                  <div className="link-row">
                    {channels.map((ch) => (
                      <label key={ch}>
                        <input
                          type="radio"
                          name="reset-channel"
                          checked={meta.value === ch}
                          onChange={() => select(ch)}
                        />
                        {" " + channelMeta(ch).shortLabel}
                      </label>
                    ))}
                  </div>
                </Field>
              )}
              <Field label={meta.drawerLabel} req htmlFor="rst-target">
                <input
                  id="rst-target"
                  type={meta.inputType}
                  placeholder={meta.boundPlaceholder}
                  autoComplete={meta.autoComplete}
                  {...resetForm.register("target")}
                />
              </Field>

              {/* 顺序固定为：先填目标（邮箱/手机号），再填图形验证码，最后发送。
                  图形码是「发送」的闸门，它必须在被保护的动作之前、在目标字段之后。 */}
              <CaptchaField id="rst-captcha" state={resetCaptcha} disabled={sendCode.isPending} />
            </>
          )}

          {step === 2 && (
            <Field
              label={meta?.codeLabel ?? "验证码"}
              req
              htmlFor="rst-code"
            >
              <div className="code-row">
                <input
                  id="rst-code"
                  type="text"
                  placeholder="6 位验证码"
                  maxLength={6}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  {...resetForm.register("code")}
                />
                <button type="button" className="btn" disabled={left > 0} onClick={onSendCode}>
                  {sendCodeText(left, sendCode.isPending, "重新发送")}
                </button>
              </div>
            </Field>
          )}

          {step === 3 && (
            <>
              <PasswordField
                id="rst-pass"
                label="新密码"
                autoComplete="new-password"
                registration={resetForm.register("new_password")}
                strength={passwordStrength(newResetPassword)}
              />
              <PasswordField
                id="rst-pass2"
                label="确认新密码"
                autoComplete="new-password"
                registration={resetForm.register("confirm_password")}
              />
            </>
          )}
        </form>
      </Drawer>
    </div>
  );
}
