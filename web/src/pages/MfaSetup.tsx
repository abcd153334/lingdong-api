import { useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { Card } from "@/components/common/Card";
import { Field } from "@/components/common/Field";
import { PageHead } from "@/components/common/PageHead";
import { SwitchRow } from "@/components/common/SwitchRow";
import { PayQRCode } from "@/components/portal/PayQRCode";
import { useMFAEnableMutation, useMFASetupMutation, useMFAStatusQuery } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { copyText } from "@/lib/clipboard";

/**
 * 二次验证绑定页（/mfa/setup）。
 *
 * 为什么是一个**独立页面**而不是用户中心里的一个弹窗：
 * 策略 `required_for_admin` 下，未绑定的管理员访问任何 `/admin/**` 都会拿到
 * 403 `mfa_setup_required`，前端据此直接跳到这里。它必须能在「被拦」的状态下
 * 正常渲染，所以它不依赖管理台布局，只依赖门户会话。
 *
 * 备份码只在 `enable` 成功那一次返回：页面必须让用户当场抄下来，
 * 之后的任何接口都不会再给 —— 服务端存的是 sha256。
 */
export function MfaSetup() {
  const navigate = useNavigate();
  const status = useMFAStatusQuery();
  const setup = useMFASetupMutation();
  const enable = useMFAEnableMutation();

  const [secret, setSecret] = useState<{ secret: string; uri: string } | null>(null);
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[] | null>(null);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState("");

  const enabled = status.data?.enabled ?? false;
  const required = status.data?.policy === "required_for_admin";

  const onSetup = () => {
    if (!password) {
      setErr("请先输入当前账户密码");
      return;
    }
    setErr("");
    setup.mutate(password, {
      onSuccess: (data) => setSecret({ secret: data.secret, uri: data.otpauth_uri }),
      onError: (error) => {
        if (error instanceof ApiError && error.errorCode === "mfa_already_enabled") {
          toast.error("二次验证已开启，如需重置请先在用户中心关闭");
          return;
        }
        if (error instanceof ApiError && error.errorCode === "invalid_credential") {
          setErr("账户密码不正确，请重新输入");
          return;
        }
        setErr(error instanceof Error ? error.message : "生成密钥失败，请重试");
      },
    });
  };

  const onEnable = () => {
    const value = code.trim();
    if (!value) {
      setErr("请输入身份验证器上的 6 位动态码");
      return;
    }
    if (!password) {
      setErr("请先输入当前账户密码");
      return;
    }
    setErr("");
    enable.mutate(
      { code: value, password },
      {
        onSuccess: (data) => {
          setCodes(data.backup_codes ?? []);
          toast.success("二次验证已开启");
        },
        onError: (error) => {
          if (error instanceof ApiError && error.errorCode === "mfa_code_invalid") {
            // 动态码只对当前 30 秒窗口有效：错一次多半是手机时间没同步。
            setErr("动态码不正确（请确认手机时间已自动同步，然后用最新一个码重试）");
            return;
          }
          if (error instanceof ApiError && error.errorCode === "invalid_credential") {
            setErr("账户密码不正确，请重新输入");
            return;
          }
          setErr(error instanceof Error ? error.message : "开启失败，请重试");
        },
      },
    );
  };

  const copySecret = async () => {
    if (!secret) return;
    if (await copyText(secret.secret)) toast.success("密钥已复制");
    else toast.error("复制失败，请手动抄写");
  };

  return (
    <>
      <PageHead title="两步验证" desc="为账号再加一道动态码，密码泄露也进不来" />

      {err ? <div className="hint warn">{err}</div> : null}

      {codes ? (
        /* 备份码只出现这一次：先让用户确认已保存，再放他离开。 */
        <Card title="请保存这些备份码" sub="仅显示一次，之后无法再次查看">
          <div className="code-list mono">
            {codes.map((c) => (
              <div key={c}>{c}</div>
            ))}
          </div>
          <div className="hint">
            每个备份码只能用一次：手机丢失时用它登录。请抄到纸上或存进密码管理器。
          </div>
          <SwitchRow label="我已妥善保存这些备份码" checked={saved} onChange={setSaved} />
          <button
            type="button"
            className="btn btn-primary"
            disabled={!saved}
            onClick={() => void navigate({ to: "/home" })}
          >
            完成
          </button>
        </Card>
      ) : enabled ? (
        <Card title="已开启" sub="每次登录都会要求动态码">
          <div className="hint">
            剩余备份码 {status.data?.backup_codes_left ?? 0} 个。需要关闭或重新生成备份码，请到
            <Link to="/settings"> 用户中心 → 账户安全 </Link>
            操作。
          </div>
          <button type="button" className="btn" onClick={() => void navigate({ to: "/home" })}>
            返回工作台
          </button>
        </Card>
      ) : (
        <Card
          title="绑定身份验证器"
          sub="支持 Google Authenticator / 1Password / 微软 Authenticator 等 TOTP 应用"
        >
          {required ? (
            <div className="hint warn">
              站点要求管理员开启两步验证。完成绑定后即可正常进入管理台。
            </div>
          ) : null}

          {/* 密码放在两步之前、常驻显示：绑定验证器等于给自己再加一把钥匙，
              只凭一个已登录的浏览器就能加，等于把一次会话劫持永久化。 */}
          <Field label="当前账户密码" req htmlFor="mfa-password">
            <input
              id="mfa-password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </Field>

          {!secret ? (
            <button type="button" className="btn btn-primary" disabled={setup.isPending} onClick={onSetup}>
              {setup.isPending ? "生成中…" : "生成密钥"}
            </button>
          ) : (
            <>
              <div className="qr-wrap">
                <PayQRCode content={secret.uri} />
              </div>
              <Field label="或手动输入密钥" htmlFor="mfa-secret">
                <input id="mfa-secret" type="text" readOnly value={secret.secret} />
              </Field>
              <button type="button" className="btn" onClick={() => void copySecret()}>
                复制密钥
              </button>
              <Field label="输入应用显示的 6 位动态码" req htmlFor="mfa-code">
                <input
                  id="mfa-code"
                  type="text"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  placeholder="123456"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
              </Field>
              <button
                type="button"
                className="btn btn-primary"
                disabled={enable.isPending}
                onClick={onEnable}
              >
                {enable.isPending ? "验证中…" : "确认开启"}
              </button>
            </>
          )}
        </Card>
      )}
    </>
  );
}
