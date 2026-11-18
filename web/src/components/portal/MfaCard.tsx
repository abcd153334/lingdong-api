import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { Field } from "@/components/common/Field";
import { Tip } from "@/components/common/Tip";
import { useMFADisableMutation, useMFARegenerateMutation, useMFAStatusQuery } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";

/**
 * 账户安全 · 两步验证卡片（唯一消费方：账户设置弹窗的「账户安全」分区）。
 *
 * 开启入口只是跳到 `/mfa/setup`（那里才有二维码与备份码），因为绑定是一个
 * 「扫码 → 输码 → 抄备份码」的三步流程，塞进这张卡会把整页撑爆。
 *
 * 关闭/重生成必须当场输入动态码：只凭会话就能关闭 2FA 的话，
 * 拿到一个已登录浏览器的人等于能永久摘掉第二因素。
 *
 * 没有「返回账户安全」链接：它原来服务于整页式用户中心，现在同一个弹窗里
 * 分区切一下就回去了，站内跳转反而会把人从弹窗带走。
 */
export function MfaCard() {
  const navigate = useNavigate();
  const status = useMFAStatusQuery();
  const disable = useMFADisableMutation();
  const regenerate = useMFARegenerateMutation();

  /** 关闭/重生成共用一个抽屉：两者都需要一次动态码。 */
  const [mode, setMode] = useState<"" | "disable" | "regenerate">("");
  const [code, setCode] = useState("");
  const [err, setErr] = useState("");
  const [newCodes, setNewCodes] = useState<string[] | null>(null);

  const st = status.data;
  const enabled = st?.enabled ?? false;
  const required = st?.policy === "required_for_admin";

  const close = () => {
    setMode("");
    setCode("");
    setErr("");
  };

  const onConfirm = () => {
    const value = code.trim();
    if (!value) {
      setErr("请输入动态码或备份码");
      return;
    }
    setErr("");
    if (mode === "disable") {
      disable.mutate(value, {
        onSuccess: () => {
          // 服务端会把全部会话（含当前）作废，所以必须回登录页。
          toast.success("已关闭两步验证，请重新登录");
          close();
          void navigate({ to: "/login" });
        },
        onError: reportErr(setErr),
      });
      return;
    }
    regenerate.mutate(value, {
      onSuccess: (data) => {
        setNewCodes(data.backup_codes ?? []);
        setCode("");
      },
      onError: reportErr(setErr),
    });
  };

  return (
    <div className="card" data-testid="mfa-card">
      <div className="blk-head">
        <span className="blk-title">两步验证</span>
        <span className={enabled ? "tag ok" : "tag"} data-testid="mfa-state">
          {enabled ? "已开启" : "未开启"}
        </span>
      </div>

      {required && !enabled ? (
        <div className="hint warn">站点要求管理员开启两步验证，未开启时无法进入管理台。</div>
      ) : null}

      {enabled ? (
        <>
          <div className="hint">
            每次登录都会要求输入身份验证器上的动态码。剩余备份码{" "}
            <b data-testid="mfa-codes-left">{st?.backup_codes_left ?? 0}</b> 个
            {st && st.backup_codes_left === 0 ? "（已用完，请重新生成）" : ""}。
          </div>
          <div className="ph-acts">
            <button type="button" className="btn" onClick={() => setMode("regenerate")}>
              重新生成备份码
            </button>
            <button type="button" className="btn" onClick={() => setMode("disable")}>
              关闭两步验证
            </button>
          </div>
        </>
      ) : (
        <>
          <div className="hint">开启后，除密码外还需要一次性动态码（TOTP）才能登录。</div>
          <button
            type="button"
            className="btn btn-primary"
            data-testid="mfa-enable-entry"
            onClick={() => void navigate({ to: "/mfa/setup" })}
          >
            去开启
          </button>
        </>
      )}

      {newCodes ? (
        <div className="blk-sub" data-testid="mfa-new-codes">
          <div className="hint">新的备份码（只显示这一次）：</div>
          <div className="code-list mono">
            {newCodes.map((c) => (
              <div key={c}>{c}</div>
            ))}
          </div>
          <button type="button" className="btn" onClick={() => setNewCodes(null)}>
            我已保存
          </button>
        </div>
      ) : null}

      {mode ? (
        <div className="blk-sub" data-testid="mfa-confirm">
          <div className="hint warn">
            {mode === "disable"
              ? "关闭后所有设备都会被登出，且账号只剩密码保护。"
              : "重新生成后，旧的备份码立即全部失效。"}
          </div>
          <Field label="动态码或备份码" req htmlFor="mfa-confirm-code">
            <input
              id="mfa-confirm-code"
              type="text"
              autoComplete="one-time-code"
              placeholder="6 位动态码，或 XXXX-XXXX 备份码"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
          </Field>
          {err ? <div className="hint warn">{err}</div> : null}
          <div className="ph-acts">
            <button type="button" className="btn" onClick={close}>
              取消
            </button>
            <button
              type="button"
              className="btn btn-primary"
              disabled={disable.isPending || regenerate.isPending}
              onClick={onConfirm}
            >
              确认
            </button>
          </div>
          <Tip text="动态码每 30 秒换一次；手机不在手边时可用一个备份码（每个只能用一次）。" />
        </div>
      ) : null}

      {!enabled && !required ? (
        <div className="hint">
          忘记密码时仍需联系站点管理员；两步验证不替代密码找回。
        </div>
      ) : null}
    </div>
  );
}

/** 把后端错误码翻成用户能动手的文案（共用给关闭与重生成）。 */
const reportErr = (setErr: (v: string) => void) => (error: unknown) => {
  if (error instanceof ApiError) {
    switch (error.errorCode) {
      case "mfa_code_invalid":
        setErr("动态码或备份码不正确（请确认手机时间已自动同步）");
        return;
      case "mfa_not_enabled":
        setErr("当前未开启两步验证，请刷新页面");
        return;
      default:
        break;
    }
    setErr(error.message);
    return;
  }
  setErr("操作失败，请重试");
};
