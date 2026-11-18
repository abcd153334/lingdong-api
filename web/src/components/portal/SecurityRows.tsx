import { Tip } from "@/components/common/Tip";
import { channelMeta, contactOf } from "@/lib/contactChannel";
import type { ContactChannel } from "@/types";

/**
 * 账户安全区的两行展示（纯展示，不持抽屉状态）。
 *
 * 抽出来是为了让「用户中心页」与「管理台账户弹窗」用同一份 DOM 与文案：
 * 两处各写一遍 `.uc-row` 的结局是一处补了提示、另一处还留着旧说法。
 */

export interface PasswordRowProps {
  mustChange: boolean;
  onEdit: () => void;
}

export function PasswordRow({ mustChange, onEdit }: PasswordRowProps) {
  return (
    <div className="uc-row">
      <div className="uc-row-main">
        <div className="rl">登录密码</div>
        <div className="rs">
          {mustChange ? "初始口令待修改" : "已设置"}
          {mustChange && (
            <Tip text="临时口令只能用于改密。改完才能使用换绑、二次验证、设备管理与充值等功能" />
          )}
        </div>
      </div>
      <button type="button" className="btn btn-sm" onClick={onEdit}>
        修改
      </button>
    </div>
  );
}

export interface ContactRowProps {
  channel: ContactChannel;
  email: string;
  phone: string;
  available: boolean;
  onBind: () => void;
}

export function ContactRow({ channel, email, phone, available, onBind }: ContactRowProps) {
  const meta = channelMeta(channel);
  const value = contactOf(channel, email, phone);
  return (
    <div className="uc-row">
      <div className="uc-row-main">
        <div className="rl">{meta.rowLabel}</div>
        <div className="rs">
          {!available ? (
            meta.disabledText
          ) : value ? (
            <>
              当前绑定：<span className="mono">{meta.mask(value)}</span>
            </>
          ) : (
            "未绑定"
          )}
        </div>
      </div>
      <div className="act">
        <button
          type="button"
          className="btn btn-sm"
          disabled={!available}
          title={available ? undefined : meta.disabledText}
          onClick={onBind}
        >
          {value ? "换绑" : "绑定"}
        </button>
      </div>
    </div>
  );
}
