import { useState } from "react";
import { toast } from "sonner";
import { Drawer } from "@/components/common/Drawer";
import { Field } from "@/components/common/Field";
import { useBindMutation, useContactCodeMutation } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { bindTargetError, channelMeta, contactOf } from "@/lib/contactChannel";
import { CODE_RE } from "@/lib/schemas";
import { sendCodeText } from "@/lib/sendCode";
import { useCountdown } from "@/lib/useCountdown";
import type { ContactChannel } from "@/types";

/** 后端以 error_code=bind_method_disabled 告知该绑定方式未启用（C-06） */
const isBindDisabledError = (error: ApiError) =>
  error.errorCode === "bind_method_disabled" ||
  (error.code === 400 && error.message.includes("绑定方式未启用"));

export interface BindContactDrawerProps {
  channel: ContactChannel;
  /** 当前值来自 `useMeQuery`（未绑定为空串），用于判断「绑定」还是「换绑」 */
  email: string;
  phone: string;
  onClose: () => void;
  /**
   * 发码/绑定被后端判定「该通道未启用」时回调：父级据此把这一行的按钮置灰，
   * 避免运营看着可点、点了必然失败。
   */
  onUnavailable: (channel: ContactChannel) => void;
}

/**
 * 绑定 / 换绑邮箱或手机（用户中心与管理台「账户设置」共用，唯一实现）。
 *
 * 父级必须用 `{ch && <BindContactDrawer key={ch} .../>}` 条件挂载：
 * 换一个通道就是一个新的表单，靠 key 重置比手写三处 setState 更不容易漏。
 */
export function BindContactDrawer({
  channel,
  email,
  phone,
  onClose,
  onUnavailable,
}: BindContactDrawerProps) {
  const [target, setTarget] = useState("");
  const [code, setCode] = useState("");
  const { left, setLeft } = useCountdown();
  const contactCode = useContactCodeMutation();
  const bind = useBindMutation();

  const meta = channelMeta(channel);
  const current = contactOf(channel, email, phone);
  const wasBound = !!current;

  const handleChannelError = (error: unknown) => {
    if (!(error instanceof ApiError) || !isBindDisabledError(error)) return;
    onUnavailable(channel);
  };

  const sendCode = () => {
    if (left > 0) return;
    const error = bindTargetError(channel, current, target);
    if (error) {
      toast.error(error);
      return;
    }
    setLeft(60);
    contactCode.mutate(
      { channel, target: target.trim() },
      {
        onSuccess: (data) => {
          setLeft(data.expires_in > 0 ? data.expires_in : 60);
          toast.success("验证码已发送");
        },
        onError: handleChannelError,
      },
    );
  };

  const confirm = () => {
    const error = bindTargetError(channel, current, target);
    if (error) {
      toast.error(error);
      return;
    }
    if (!CODE_RE.test(code.trim())) {
      toast.error("请输入 6 位数字验证码");
      return;
    }
    bind.mutate(
      { channel, target: target.trim(), code: code.trim() },
      {
        onSuccess: () => {
          toast.success(`${meta.label}${wasBound ? "换绑成功" : "绑定成功"}`);
          onClose();
        },
        onError: handleChannelError,
      },
    );
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={`${wasBound ? "换绑" : "绑定"}${meta.label}`}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            取消
          </button>
          <button type="button" className="btn btn-primary" onClick={confirm}>
            {wasBound ? "确认换绑" : "确认绑定"}
          </button>
        </>
      }
    >
      <Field label={`接收验证码的${meta.label}`} req htmlFor="bind-target">
        <input
          id="bind-target"
          type={meta.inputType}
          placeholder={meta.newPlaceholder}
          value={target}
          onChange={(e) => setTarget(e.target.value)}
        />
      </Field>
      <Field label={meta.codeLabel} req htmlFor="bind-code">
        <div className="code-row">
          <input
            id="bind-code"
            type="text"
            placeholder="6 位验证码"
            maxLength={6}
            inputMode="numeric"
            autoComplete="one-time-code"
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
          <button type="button" className="btn" disabled={left > 0} onClick={sendCode}>
            {sendCodeText(left, contactCode.isPending)}
          </button>
        </div>
      </Field>
    </Drawer>
  );
}
