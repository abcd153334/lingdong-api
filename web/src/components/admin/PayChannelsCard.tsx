import { useState } from "react";
import { Card } from "@/components/common/Card";
import { EmptyState } from "@/components/common/EmptyState";
import { Icon } from "@/components/common/Icon";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { PayChannelCard } from "./PayChannelCard";
import { PayChannelCreateDialog } from "./PayChannelCreateDialog";
import { schemaOf } from "@/lib/payChannelSchema";
import type { PayChannel, PayChannelSchema } from "@/types";

interface Props {
  channels: PayChannel[];
  /** 服务端已落库的渠道（用于「已标记删除，保存后生效」的对比基线） */
  savedChannels: PayChannel[];
  /** 各协议的参数声明（后端 `pay_channel_schemas`，缺失时由调用方回落 FALLBACK_SCHEMAS） */
  schemas: PayChannelSchema[];
  /** 可选支付方式（后端 `pay_method_options`） */
  methodOptions: { value: string; label: string }[];
  /** 回调地址前缀：`{notifyBase}{渠道ID}/notify` */
  notifyBase: string;
  /** 站点当前结算币种（用于标出「不收该币种、收银台不展示」的渠道） */
  siteCurrency: string;
  /** 敏感值回显占位（`******`） */
  mask: string;
  onChange: (next: PayChannel[]) => void;
}

/**
 * 支付渠道配置卡（卡片网格 + 空态 + 弹窗新增）。
 *
 * 渠道数组是**唯一状态源**（`form.pay_channels`），本组件只做增删改与排序；
 * 每张卡片是纯受控展示（见 PayChannelCard），参数表单由协议 schema 驱动
 * （见 PayChannelParams），不再遍历 `params` 里存了哪些键。
 *
 * 关于「已标记删除」条：删除只改表单态，必须点「保存全部配置」才落库。
 * 后端可能因为「有在途/退款中订单」拒绝删除，而 `onError` 只切页签 + toast，
 * 不回滚也不重新拉取 —— 运营看到卡片消失了就以为删成功，刷新才发现还在。
 * 这条常驻提示把「尚未生效」显式说出来，并给一键撤销。
 */
export function PayChannelsCard({
  channels,
  savedChannels,
  schemas,
  methodOptions,
  notifyBase,
  siteCurrency,
  mask,
  onChange,
}: Props) {
  const [creating, setCreating] = useState(false);
  const methodItems = methodOptions.map((o) => ({ key: o.value, label: o.label }));
  const empty = channels.length === 0;

  const patch = (id: string, next: Partial<PayChannel>): void => {
    onChange(channels.map((c) => (c.id === id ? { ...c, ...next } : c)));
  };

  const move = (index: number, delta: number): void => {
    const target = index + delta;
    if (target < 0 || target >= channels.length) return;
    const next = [...channels];
    const [item] = next.splice(index, 1);
    next.splice(target, 0, item);
    onChange(next);
  };

  const remove = async (ch: PayChannel): Promise<void> => {
    const ok = await confirmDialog({
      title: "删除支付渠道",
      body: (
        <div>
          <p>
            确认删除<b>「{ch.name}」</b>？
          </p>
          <p className="muted small">
            保存后才会真正删除。删除后该渠道的历史订单无法再原路退款（只能回退平台余额），且它在上游后台的回调会打到不存在的路径。
            有在途订单或退款中订单时会被拒绝，此时请改用「停用」。
          </p>
        </div>
      ),
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    onChange(channels.filter((c) => c.id !== ch.id));
  };

  /** 本次会话中被删掉、但还没落库的渠道。 */
  const pendingRemoved = savedChannels.filter((s) => !channels.some((c) => c.id === s.id));

  const addButton = (primary: boolean) => (
    <button
      type="button"
      className={primary ? "btn btn-primary" : "btn btn-sm"}
      onClick={() => setCreating(true)}
    >
      <Icon name="plus" size={14} />
      {primary ? "添加支付渠道" : "新增渠道"}
    </button>
  );

  return (
    <Card
      title="支付渠道"
      sub={empty ? undefined : "顺序即优先级（#1 优先）"}
      // 空态时不渲染卡头动作：否则页面上会同时出现两个「添加」按钮
      actions={empty ? undefined : addButton(false)}
    >
      {empty ? (
        <EmptyState
          title="还没有支付渠道"
          desc="按「协议 + 支付方式」添加第一条渠道，填好密钥并启用后，收银台才会出现对应支付方式。"
          action={addButton(true)}
        />
      ) : (
        <div className="pc-grid">
          {channels.map((ch, index) => (
            <PayChannelCard
              key={ch.id}
              channel={ch}
              index={index}
              total={channels.length}
              schema={schemaOf(schemas, ch.protocol)}
              methodItems={methodItems}
              notifyBase={notifyBase}
              siteCurrency={siteCurrency}
              mask={mask}
              onPatch={(next) => patch(ch.id, next)}
              onMove={(delta) => move(index, delta)}
              onRemove={() => void remove(ch)}
            />
          ))}
        </div>
      )}

      {pendingRemoved.length > 0 ? (
        <div className="pc-pending-del" role="status">
          <span>
            已标记删除 {pendingRemoved.length} 条（{pendingRemoved.map((c) => c.name).join("、")}
            ），保存后生效
          </span>
          <button type="button" className="btn btn-sm" onClick={() => onChange(savedChannels)}>
            撤销
          </button>
        </div>
      ) : null}

      {creating ? (
        <PayChannelCreateDialog
          channels={channels}
          schemas={schemas}
          methodItems={methodItems}
          notifyBase={notifyBase}
          siteCurrency={siteCurrency}
          onClose={() => setCreating(false)}
          onCreate={(ch) => onChange([...channels, ch])}
        />
      ) : null}
    </Card>
  );
}
