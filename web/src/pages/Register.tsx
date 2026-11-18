import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useForm, type SubmitErrorHandler } from "react-hook-form";
import { toast } from "sonner";
import { CaptchaField, PasswordField, useCaptcha } from "@/components/auth/AuthFields";
import { sendCodeText } from "@/lib/sendCode";
import { useCountdown } from "@/lib/useCountdown";
import { Field } from "@/components/common/Field";
import { Icon } from "@/components/common/Icon";
import { SiteLogo } from "@/components/common/SiteLogo";
import { ThemeToggle } from "@/components/chrome/ThemeToggle";
import {
  usePortalConfigQuery,
  useRegisterContactCodeMutation,
  useRegisterMutation,
} from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { channelFormatError, channelMeta, useContactChannelPicker } from "@/lib/contactChannel";
import { STRENGTH_LABELS, passwordStrength, zodResolver } from "@/lib/form";
import { registerSchema, type RegisterForm } from "@/lib/schemas";
import { useSite } from "@/lib/site";

/** 注册错误分支展示文案（C-06：按后端 error_code 机器可判读） */
function registerErrorText(error: ApiError): string {
  switch (error.errorCode) {
    case "username_taken":
      return "用户名已被占用，请换一个";
    case "email_taken":
      return "该邮箱/手机号已被占用，请更换";
    case "agreement_required":
      return "请先阅读并同意用户协议";
    case "register_closed":
      return "本站已关闭注册";
    case "invite_required":
      return "本站仅限邀请注册，请填写有效邀请码";
    case "contact_required":
      return "请填写并验证联系方式";
    case "contact_verify_required":
      return "请先完成联系方式验证";
    case "verify_code_invalid":
      return "验证码不正确或已过期";
    case "captcha_invalid":
      return "人机验证未通过，请重试";
    case "captcha_required":
      return "请先完成人机验证";
    case "bind_method_disabled":
      return "该注册方式未启用，请换一种";
    default:
      break;
  }
  return error.message || "注册失败，请稍后重试";
}

/**
 * 注册页（S2）
 *
 * 四档准入（`auth_register.mode` × `contact_verify`）全部由服务端配置驱动：
 * - closed：直接不出表单；
 * - invite_only：邀请码变必填；
 * - contact_verify=required：联系方式与验证码变必填（且渠道必须真的配置好，
 *   否则发码必然失败 —— 服务端启动时就会拦下这种自相矛盾的配置）。
 */
export function Register() {
  const navigate = useNavigate();
  const cardRef = useRef<HTMLDivElement>(null);
  const site = useSite();
  const portalCfg = usePortalConfigQuery();
  const auth = portalCfg.data?.auth;
  const registerMutation = useRegisterMutation();
  const sendCode = useRegisterContactCodeMutation();
  const captcha = useCaptcha("register");

  const inviteOnly = auth?.register_mode === "invite_only";
  const closed = auth?.register_mode === "closed";
  const verifyRequired = auth?.contact_verify === "required";
  // 渠道由服务端下发的「生效集合」决定（策略 ∩ 就绪，按优先级排序，首项即默认）。
  // 不在页面里写死默认值：只开短信的站点过去会渲染邮箱框，发码直接被后端拒绝。
  const { channels, meta, select, blocked } = useContactChannelPicker();

  const [affInitial] = useState(
    () => new URLSearchParams(window.location.search).get("aff") ?? "",
  );
  const [agreed, setAgreed] = useState(false);
  const [errMsg, setErrMsg] = useState("");
  const [shake, setShake] = useState(0);
  const { left, setLeft } = useCountdown();

  const form = useForm<RegisterForm>({
    resolver: zodResolver<RegisterForm>(registerSchema),
    defaultValues: {
      username: "",
      target: "",
      code: "",
      aff_code: affInitial,
      password: "",
      confirm_password: "",
    },
  });
  const username = form.watch("username");
  const password = form.watch("password");

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

  /**
   * 配置自愈：界面上的渠道来自 30s 缓存，运营可能在别处刚改过。
   * 命中 bind_method_disabled 时重新拉配置，让表单按新渠道重建 ——
   * 否则界面只给了这一个渠道，用户根本没有"换一种"的出路。
   */
  const refreshChannels = () => void portalCfg.refetch();

  const sendContactCode = () => {
    if (left > 0 || !meta) return;
    const target = form.getValues("target").trim();
    const formatError = channelFormatError(meta.value, target);
    if (formatError) {
      showErr(formatError);
      return;
    }
    // 注册发码**不带**图形验证码：后端这一步不校验。
    // 图形码是一次性的，发码用掉一张，注册提交就必然拿不到 —— 用户得填两次。
    sendCode.mutate(
      { channel: meta.value, target },
      {
        onSuccess: (data) => {
          setLeft(data.expires_in > 0 ? data.expires_in : 60);
          toast.success("验证码已发送");
        },
        onError: (error) => {
          if (error instanceof ApiError && error.errorCode === "bind_method_disabled") {
            refreshChannels();
          }
          showErr(error instanceof ApiError ? error.message : "验证码发送失败");
        },
      },
    );
  };

  const submit = (values: RegisterForm) => {
    if (!agreed) {
      showErr("请先阅读并同意用户协议");
      return;
    }
    const target = values.target.trim();
    if (verifyRequired && !target) {
      showErr("请填写并验证联系方式");
      return;
    }
    // 格式校验必须在这里显式做：schema 只管「填了 target 就要有 6 位码」，
    // 不判邮箱/手机号形态（它不知道当前渠道）。放在图形码校验之前，
    // 否则用户会先看到「请输入图中 5 位数字」这种答非所问的提示。
    if (target && meta) {
      const formatError = channelFormatError(meta.value, target);
      if (formatError) {
        showErr(formatError);
        return;
      }
    }
    setErrMsg("");
    if (captcha.visible && captcha.answer.trim().length !== 5) {
      showErr("请输入图中 5 位数字");
      return;
    }
    registerMutation.mutate(
      {
        username: values.username.trim(),
        channel: target && meta ? meta.value : undefined,
        target: target || undefined,
        code: target ? values.code.trim() : undefined,
        password: values.password,
        confirm_password: values.confirm_password,
        aff_code: values.aff_code.trim() || undefined,
        agreed,
        captcha_id: captcha.visible ? captcha.id || undefined : undefined,
        captcha_answer: captcha.visible ? captcha.answer.trim() || undefined : undefined,
      },
      {
        onSuccess: (outcome) => {
          if (outcome.ok) {
            toast.success("注册成功，请登录");
            void navigate({ to: "/login" });
            return;
          }
          if (outcome.error.errorCode === "captcha_required") {
            captcha.require();
          } else if (outcome.error.errorCode === "captcha_invalid") {
            captcha.refresh();
          }
          showErr(registerErrorText(outcome.error));
        },
      },
    );
  };

  // 校验失败的根文案取自 RHF 传入的 errors（formState 此时尚未同步）
  const onInvalid: SubmitErrorHandler<RegisterForm> = (errors) => {
    const message = errors.root?.message;
    if (message) showErr(message);
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
          <div className="sub-title">创建用户门户账号</div>

          {closed ? (
            <div className="alert-banner warn">
              本站当前已关闭注册。如需账号，请联系管理员。
            </div>
          ) : blocked ? (
            /* 要求验证联系方式、却没有任何可用渠道 = 运营配置自相矛盾。
               此时画出表单只会让用户填完必然失败，直接说清原因。
               顺序不能反：注册已关闭比渠道未配置更重要。 */
            <div className="alert-banner warn">
              本站要求验证联系方式，但管理员尚未配置可用的发送通道，暂时无法注册。
            </div>
          ) : (
            <form onSubmit={form.handleSubmit(submit, onInvalid)}>
              <Field
                label="用户名"
                req
                htmlFor="rg-username"
                labelExtra={<span className="cnt">{username.length} / 20</span>}
              >
                <input
                  id="rg-username"
                  type="text"
                  placeholder="3-20 位字母、数字或下划线"
                  maxLength={20}
                  autoComplete="username"
                  {...form.register("username")}
                />
              </Field>

              {meta && (
                <>
                  {channels && channels.length > 1 && (
                    <Field label="联系方式" req={verifyRequired}>
                      <div className="link-row">
                        {channels.map((ch) => (
                          <label key={ch}>
                            <input
                              type="radio"
                              name="rg-channel"
                              checked={meta.value === ch}
                              onChange={() => select(ch)}
                            />
                            {" " + channelMeta(ch).label}
                          </label>
                        ))}
                      </div>
                    </Field>
                  )}
                  <Field
                    label={meta.label}
                    req={verifyRequired}
                    htmlFor="rg-target"
                    labelExtra={<span className="hint hint-inline">（用于找回密码）</span>}
                  >
                    <input
                      id="rg-target"
                      type={meta.inputType}
                      placeholder={meta.placeholder}
                      autoComplete={meta.autoComplete}
                      {...form.register("target")}
                    />
                  </Field>
                  {/* 与下方的「图形验证码」区分开：同页两个都叫「验证码」时，
                      用户分不清哪个填 6 位、哪个填 5 位（真实反馈过的困惑）。 */}
                  <Field
                    label={meta.codeLabel}
                    req={verifyRequired}
                    htmlFor="rg-code"
                  >
                    <div className="code-row">
                      <input
                        id="rg-code"
                        type="text"
                        placeholder="6 位验证码"
                        maxLength={6}
                        inputMode="numeric"
                        autoComplete="one-time-code"
                        {...form.register("code")}
                      />
                      <button
                        type="button"
                        className="btn"
                        disabled={left > 0 || sendCode.isPending}
                        onClick={sendContactCode}
                      >
                        {sendCodeText(left, sendCode.isPending)}
                      </button>
                    </div>
                  </Field>
                </>
              )}

              <Field
                label="邀请码"
                req={inviteOnly}
                htmlFor="rg-aff"
                labelExtra={
                  <span className="hint hint-inline">
                    {inviteOnly ? "（本站仅限邀请注册）" : "（选填：好友邀请码）"}
                  </span>
                }
              >
                <input
                  id="rg-aff"
                  type="text"
                  placeholder="例如 LD-AFF-8K2M"
                  autoComplete="off"
                  {...form.register("aff_code")}
                />
              </Field>

              <PasswordField
                id="rg-password"
                label="密码"
                placeholder="至少 8 位，建议混合大小写 / 数字 / 符号"
                autoComplete="new-password"
                registration={form.register("password")}
                strength={passwordStrength(password)}
                hint={`密码强度：${STRENGTH_LABELS[passwordStrength(password)]}`}
              />

              <PasswordField
                id="rg-password2"
                label="确认密码"
                placeholder="再次输入密码"
                autoComplete="new-password"
                registration={form.register("confirm_password")}
              />



              <CaptchaField id="rg-captcha" state={captcha} disabled={registerMutation.isPending} />

              {errMsg && (
                <div className="err-line missing">
                  <Icon name="close" size={14} />
                  {errMsg}
                </div>
              )}

              <label className="link-row">
                <input
                  type="checkbox"
                  checked={agreed}
                  onChange={(e) => setAgreed(e.target.checked)}
                />
                我已阅读并同意 <Link to="/agreement">《用户协议》</Link>
              </label>

              <button
                type="submit"
                className="btn btn-primary"
                disabled={registerMutation.isPending}
              >
                {registerMutation.isPending ? "注册中…" : "注 册"}
              </button>
            </form>
          )}

          <div className="alt">
            已有账号？<Link to="/login">直接登录</Link>
          </div>
        </div>
      </div>
    </div>
  );
}
