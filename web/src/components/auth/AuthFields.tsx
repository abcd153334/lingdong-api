import { useCallback, useEffect, useState, type ReactNode } from "react";
import type { UseFormRegisterReturn } from "react-hook-form";
import { Field } from "@/components/common/Field";
import { fetchCaptcha, usePortalConfigQuery } from "@/lib/api";
import type { Strength } from "@/lib/form";
import type { AuthConfig } from "@/types";

/**
 * 图形验证码（唯一实现）。
 *
 * 交互只有两样：一个输入框，图**嵌在输入框最右端**（点图即换一张）。
 * 没有独立的「换一张」按钮 —— 图本身就是刷新入口，少一个控件、少一处错位。
 *
 * 两个关键设计（都是踩过坑之后改的）：
 *   1. 图与 id 在**同一次响应**里返回（dataURL + id），不种 Cookie、不用 `<img src>`。
 *      否则 id 只能落 Cookie（单槽），多标签页/双表单/任意一次重渲染都会让
 *      "图与 id 错位"，表现为"照着图填也永远错"。
 *   2. 图存在 state 里，只有 `refresh()` 才会变。**不许**在渲染期间生成带时间戳的
 *      URL —— 那会让用户每敲一个字符就换一张图。
 */

/** 验证码场景（与后端 authcfg.Scene* 对齐） */
export type CaptchaScene = "register" | "login" | "password_reset";

/**
 * 场景对应的策略。
 *
 * 配置未就绪返回 null（而不是 "always"）：门户配置是异步请求，首屏时必然为空；
 * 返回 "always" 会让「不需要验证码」的站点也白取一张图。
 */
function sceneMode(auth: AuthConfig | undefined, scene: CaptchaScene): string | null {
  if (!auth) return null;
  switch (scene) {
    case "register":
      return auth.captcha_register;
    case "login":
      return auth.captcha_login;
    case "password_reset":
      return auth.captcha_reset;
  }
}

export interface CaptchaState {
  /** 是否渲染控件：always 恒真；after_fails 只有在 `require()` 之后为真 */
  visible: boolean;
  mode: string | null;
  /** 服务端下发的图片 id（提交时回传） */
  id: string;
  /** `data:image/png;base64,...` */
  image: string;
  answer: string;
  setAnswer: (v: string) => void;
  loading: boolean;
  /** 图没取到（429/断网/服务端 5xx） */
  failed: boolean;
  /** 换一张（同时清空输入） */
  refresh: () => void;
  /** 服务端说「要验证」时把控件亮出来并取图 */
  require: () => void;
}

export function useCaptcha(scene: CaptchaScene): CaptchaState {
  const cfg = usePortalConfigQuery();
  const auth = cfg.data?.auth;
  const mode = sceneMode(auth, scene);

  const [forced, setForced] = useState(false);
  const [bump, setBump] = useState(0);
  const [img, setImg] = useState({ id: "", image: "" });
  const [answer, setAnswer] = useState("");
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);

  const visible = mode === "always" || (mode === "after_fails" && forced);

  useEffect(() => {
    // 配置未到：不知道要不要验证，先不取图。
    if (!cfg.isFetched) return;
    if (!visible) {
      // 策略变成 off、或从未要求验证：清掉旧图，避免把上一张的 id 带进请求。
      setImg({ id: "", image: "" });
      setAnswer("");
      setFailed(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setFailed(false);
    fetchCaptcha(scene)
      .then((r) => {
        if (cancelled) return;
        // 新图必须重填：旧答案对应的是上一张。
        setImg(r);
        setAnswer("");
      })
      .catch(() => {
        if (cancelled) return;
        // 失败时清掉 id：宁可让提交被服务端判「缺验证码」，
        // 也不要拿旧 id 配新答案（那会变成"看起来填了却一直错"）。
        setImg({ id: "", image: "" });
        setFailed(true);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [scene, visible, bump, cfg.isFetched]);

  const refresh = useCallback(() => setBump((v) => v + 1), []);
  const require = useCallback(() => setForced(true), []);

  return {
    visible,
    mode,
    id: img.id,
    image: img.image,
    answer,
    setAnswer,
    loading,
    failed,
    refresh,
    require,
  };
}

export interface CaptchaFieldProps {
  id: string;
  label?: string;
  state: CaptchaState;
  disabled?: boolean;
}

/**
 * 图形验证码控件（全站唯一）：输入框 + 内嵌在右端的图。
 *
 * 结构上必须保持 `.captcha-box > input + .captcha-swap`：
 * 布局全靠 `components.css` 里那三条规则，页面侧不要覆盖，
 * 否则三处（登录 / 注册 / 找回）会慢慢长歪成三种样子。
 */
export function CaptchaField({ id, label = "图形验证码", state, disabled }: CaptchaFieldProps) {
  if (!state.visible) return null;

  return (
    <Field label={label} req htmlFor={id}>
      <div className="captcha-box">
        <input
          id={id}
          type="text"
          inputMode="numeric"
          autoComplete="off"
          maxLength={5}
          placeholder="图中 5 位数字"
          value={state.answer}
          disabled={disabled || state.loading}
          onChange={(e) => state.setAnswer(e.target.value.replace(/\D/g, ""))}
        />
        {/*
          图就是刷新入口：点一下换一张。失败态复用同一个热区（显示「重试」），
          这样控件永远只有一块可点区域，不会出现"图挂了就没有刷新入口"。
        */}
        <button
          type="button"
          className="captcha-swap"
          title={state.failed ? "加载失败，点击重试" : "看不清？点击换一张"}
          aria-label="换一张验证码"
          onClick={state.refresh}
          disabled={disabled || state.loading}
        >
          {state.failed ? (
            <span className="captcha-swap-tip">重试</span>
          ) : state.image ? (
            <img
              src={state.image}
              alt="图形验证码"
              onError={state.refresh}
            />
          ) : (
            // 还没有图：不要渲染 src="" —— 浏览器会把它当成"当前页面地址"再请求一遍。
            <span className="captcha-swap-tip">加载中</span>
          )}
        </button>
      </div>
    </Field>
  );
}

/** 密码强度条（3 段；on1/on2/on3 由 components.css 承载） */
export function StrengthBar({ value }: { value: Strength }) {
  return (
    <div className="strength" aria-hidden="true">
      {[1, 2, 3].map((i) => (
        <i key={i} className={i <= value ? `on${value}` : undefined} />
      ))}
    </div>
  );
}

export interface PasswordFieldProps {
  id: string;
  label: string;
  placeholder?: string;
  autoComplete: string;
  disabled?: boolean;
  /** react-hook-form 的 `register(name)` 返回值 */
  registration: UseFormRegisterReturn;
  /** 密码强度（传入即渲染强度条） */
  strength?: Strength;
  /** 强度条下方文案 */
  hint?: ReactNode;
  labelExtra?: ReactNode;
}

/** 密码字段（登录 / 注册 / 重置 / 改密共用；含可选强度条） */
export function PasswordField({
  id,
  label,
  placeholder,
  autoComplete,
  disabled,
  registration,
  strength,
  hint,
  labelExtra,
}: PasswordFieldProps) {
  return (
    <Field label={label} req htmlFor={id} hint={hint} labelExtra={labelExtra}>
      <input
        id={id}
        type="password"
        placeholder={placeholder}
        autoComplete={autoComplete}
        disabled={disabled}
        {...registration}
      />
      {strength !== undefined ? <StrengthBar value={strength} /> : null}
    </Field>
  );
}
