import { useState } from "react";
import { toast } from "sonner";
import { Field } from "@/components/common/Field";
import { Icon } from "@/components/common/Icon";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Tag } from "@/components/common/StatusTag";
import { SwitchRow } from "@/components/common/SwitchRow";
import { PayChannelParams } from "./PayChannelParams";
import { channelMethods, channelNotifyURL, hasLegacyMethod } from "@/lib/payChannel";
import { allowedMethods, channelReadiness, initialMode } from "@/lib/payChannelSchema";
import { copyText } from "@/lib/clipboard";
import type { PayChannel, PayChannelSchema } from "@/types";

interface Props {
  channel: PayChannel;
  /** 在列表中的位置（从 0 开始） */
  index: number;
  total: number;
  /** 协议声明；协议为空（历史占位）时 undefined */
  schema?: PayChannelSchema;
  /** 可选的支付方式（后端 `pay_method_options`） */
  methodItems: { key: string; label: string }[];
  /** 回调地址前缀：`{notifyBase}{渠道ID}/notify` */
  notifyBase: string;
  /** 站点当前结算币种：渠道只收自己声明的币种，不一致时收银台不展示它 */
  siteCurrency: string;
  mask: string;
  onPatch: (patch: Partial<PayChannel>) => void;
  onMove: (delta: number) => void;
  onRemove: () => void;
}

/**
 * 单个支付渠道卡片。
 *
 * 纯展示 + 事件上抛，不持有渠道副本（渠道数组只存在于 `form.pay_channels`）：
 * 卡片化后排序/删除更频繁，一旦各自持有副本，就会和表单状态分叉。
 *
 * **唯一持有的本地状态是 `mode`**（虚拟开关的当前值，目前只有微信的「验签方式」）。
 * 它不落库、不进 `form`，但必须在卡片这一层持有：参数区按它决定显示哪些字段，
 * 卡片自身的「缺 N 项参数」标记也要按它计算 —— 两处各持一份 state 必然不同步
 * （切到「平台证书」后表单只剩 6 个框，卡片却还按公钥模式报「缺 2 项」）。
 *
 * 三条产品规则：
 *  1. 虎皮椒一渠道一方式（两套 APPID）/ 易支付一渠道多方式 → 由 schema.method_mode 决定；
 *  2. 顺序即「同一种支付方式的优先级」→ 卡片头显示 `#序号` + 上移/下移；
 *  3. 运营要贴到上游后台的是**实际回调地址** → 直接展示 + 复制。
 */
export function PayChannelCard({
  channel,
  index,
  total,
  schema,
  methodItems,
  notifyBase,
  siteCurrency,
  mask,
  onPatch,
  onMove,
  onRemove,
}: Props) {
  const methods = channelMethods(channel);
  const single = schema?.method_mode === "single";
  // 只展示该协议**允许**的方式（微信直连只给「微信支付」，支付宝直连只给「支付宝」）。
  const allowedItems = allowedMethods(schema, methodItems);
  const notify = channelNotifyURL(notifyBase, channel.id);
  const noProtocol = !schema;

  const [mode, setMode] = useState(() =>
    schema ? initialMode(schema, channel.params ?? {}) : "",
  );
  const { missing } = channelReadiness(channel, schema, mask, mode);

  /**
   * 渠道声明收哪种币种（空 = 人民币，与后端 payment.channelSettlesIn 同一条回落规则）。
   *
   * 与站点币种不一致时**收银台不会展示本渠道**：这不是提示性的，而是「用户根本看不到
   * 这个支付方式」。徽标必须显式说出来，否则运营只会看到「支付方式无缘无故没了」，
   * 排查方向会跑到密钥/回调地址上去。
   */
  const channelCurrency = String(channel.params?.settle_currency ?? "").trim().toUpperCase() || "CNY";
  const currencyMismatch = channelCurrency !== siteCurrency;

  const copy = async (text: string): Promise<void> => {
    if (await copyText(text)) toast.success("已复制，请粘贴到上游后台");
    else toast.error("复制失败，请手动选中复制");
  };

  return (
    <article className={`pc-card${noProtocol ? " pc-card--muted" : ""}`} data-testid="pay-channel-card">
      <header className="pc-card-hd">
        <div className="pc-card-title">
          <span className="pc-rank">#{index + 1}</span>
          <span className="pn">{channel.name}</span>
          {schema ? <Tag tone="info">{schema.label}</Tag> : <Tag tone="danger">未选协议</Tag>}
        </div>
        <div className="pc-card-acts">
          <SwitchRow
            label="启用"
            checked={channel.enabled}
            disabled={noProtocol}
            ariaLabel={`${channel.name} 启用`}
            tip={noProtocol ? "没有协议，收银台不会展示" : undefined}
            onChange={(checked) => onPatch({ enabled: checked })}
          />
          <button
            type="button"
            className="btn btn-sm"
            disabled={index === 0}
            title="上移（同一种支付方式取配置顺序第一个）"
            onClick={() => onMove(-1)}
          >
            ↑
          </button>
          <button type="button" className="btn btn-sm" disabled={index === total - 1} onClick={() => onMove(1)}>
            ↓
          </button>
          <button type="button" className="btn btn-sm btn-danger" onClick={onRemove}>
            删除
          </button>
        </div>
      </header>

      <div className="pc-card-meta">
        <code className="mono small muted">{channel.id}</code>
        {methods.length > 0 ? (
          methods.map((m) => (
            <Tag key={m} tone="gray">
              {methodItems.find((it) => it.key === m)?.label ?? m}
            </Tag>
          ))
        ) : (
          <Tag tone="gray">未选方式</Tag>
        )}
        {currencyMismatch && (
          <Tag tone="warning">
            仅收 {channelCurrency}（收银台不展示）
          </Tag>
        )}
        {!noProtocol ? (
          missing.length > 0 ? (
            <Tag tone="warning">缺 {missing.length} 项参数</Tag>
          ) : (
            <Tag tone="success">参数完整</Tag>
          )        ) : null}
      </div>

      {noProtocol ? (
        <div className="alert-banner warn">
          <div>该渠道没有协议（历史占位）：收银台不会展示它，也不能启用。请删除后按需重新添加。</div>
        </div>
      ) : (
        <Field label="支付方式" req={channel.enabled} tip={schema.method_help}>
          {single ? (
            <SegmentedChips
              className="pcs-methods"
              items={allowedItems}
              value={methods[0] ?? ""}
              onChange={(key) => onPatch({ methods: [key] })}
            />
          ) : (
            <SegmentedChips
              className="pcs-methods"
              items={allowedItems}
              multiple
              values={methods}
              onChange={(key) =>
                onPatch({
                  methods: methods.includes(key) ? methods.filter((m) => m !== key) : [...methods, key],
                })
              }
            />
          )}
        </Field>
      )}

      {hasLegacyMethod(channel) ? (
        <div className="alert-banner warn">
          <div>
            该渠道仍在使用「聚合支付」（存量配置）：收银台无法据此判断微信还是支付宝。 请改为具体方式并保存；
            两种都要请各建一条渠道。
          </div>
        </div>
      ) : null}

      {channel.enabled && missing.length > 0 ? (
        <div className="alert-banner warn">
          <div>
            已启用但缺少 {missing.map((m) => m.label).join("、")}：保存后收银台会展示该方式， 但用户下单必然失败。
          </div>
        </div>
      ) : null}

      {notify ? (
        <div className="pc-notify">
          <div className="small muted">复制到上游后台</div>
          <div className="flex-between">
            <code className="mono small break-all">{notify}</code>
            <button type="button" className="btn btn-sm" onClick={() => void copy(notify)}>
              <Icon name="copy" size={13} />
              复制
            </button>
          </div>
        </div>
      ) : (
        <div className="alert-banner warn">
          <div>站点第 1 条 API 调用地址未配置，无法生成回调地址：请先在「站点」页签里填写。</div>
        </div>
      )}

      {schema ? (
        <PayChannelParams
          schema={schema}
          params={channel.params ?? {}}
          mask={mask}
          mode={mode}
          onModeChange={setMode}
          onChange={(key, value) => onPatch({ params: { ...channel.params, [key]: value } })}
        />
      ) : null}
    </article>
  );
}
