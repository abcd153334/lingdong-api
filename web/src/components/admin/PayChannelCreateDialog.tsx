import { useState } from "react";
import { toast } from "sonner";
import { Dialog } from "@/components/common/Dialog";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { CHANNEL_ID_PATTERN, channelNotifyURL, suggestChannelID } from "@/lib/payChannel";
import { allowedMethods, fillDefaults, schemaOf } from "@/lib/payChannelSchema";
import type { PayChannel, PayChannelSchema } from "@/types";

interface Props {
  channels: PayChannel[];
  schemas: PayChannelSchema[];
  methodItems: { key: string; label: string }[];
  notifyBase: string;
  /** 站点当前结算币种：新渠道的「结算币种」默认跟随它，避免建出来就不收钱 */
  siteCurrency: string;
  onClose: () => void;
  onCreate: (channel: PayChannel) => void;
}

interface Draft {
  protocol: string;
  name: string;
  id: string;
  methods: string[];
}

/**
 * 新增支付渠道弹窗。
 *
 * 只收「协议 + 名称 + ID + 支付方式」四样：参数在卡片里填（此时卡片已存在，
 * 运营能同时看到回调地址与必填项）。新建一律 `enabled: false` —— 还没填密钥就启用，
 * 收银台会立刻出现一个必然失败的选项。
 *
 * 由调用方条件渲染（关闭即卸载），所以每次打开都是全新的草案，无需额外重置逻辑。
 */
export function PayChannelCreateDialog({
  channels,
  schemas,
  methodItems,
  notifyBase,
  siteCurrency,
  onClose,
  onCreate,
}: Props) {
  const usable = schemas.filter((s) => s.implemented);
  const first = usable[0];
  const initialMethods = (p: string): string[] => {
    // 单方式协议（微信直连/支付宝直连）没有「选什么」的余地：
    // 自动预选唯一值，避免运营忘点 chips 后被后端「启用必须选方式」拦死。
    const s = schemaOf(schemas, p);
    return s?.methods?.length === 1 ? [s.methods[0]] : [];
  };
  const [draft, setDraft] = useState<Draft>(() => ({
    protocol: first?.protocol ?? "",
    name: "",
    id: first ? suggestChannelID(first.protocol, channels.map((c) => c.id)) : "",
    methods: first ? initialMethods(first.protocol) : [],
  }));

  const schema = schemaOf(schemas, draft.protocol);
  const single = schema?.method_mode === "single";
  const methodChoices = allowedMethods(schema, methodItems);

  const switchProtocol = (protocol: string): void => {
    // 换协议时重新给 ID 建议，避免 xunhupay 的 ID 挂在 epay 上（ID 创建后不可改）。
    setDraft((d) => ({
      ...d,
      protocol,
      id: suggestChannelID(protocol, channels.map((c) => c.id)),
      methods: initialMethods(protocol),
    }));
  };

  const submit = (): void => {
    const name = draft.name.trim();
    const id = draft.id.trim();
    if (!schema) {
      toast.error("请选择支付协议");
      return;
    }
    if (!name) {
      toast.error("请填写渠道名称");
      return;
    }
    if (name.includes("-")) {
      toast.error("渠道名称不能包含中划线（-）：订单快照按它拆分渠道与支付方式");
      return;
    }
    if (channels.some((c) => c.name === name)) {
      // 后端不强制名称唯一（存量库可能有同名），但新增时拦住，避免对账时分不清。
      toast.error("已存在同名渠道，请换一个名称");
      return;
    }
    if (!CHANNEL_ID_PATTERN.test(id)) {
      toast.error("渠道 ID 只能用小写字母/数字/下划线/中划线，长度 1–32");
      return;
    }
    if (channels.some((c) => c.id === id)) {
      toast.error("渠道 ID 已存在");
      return;
    }
    if (draft.methods.length === 0) {
      toast.error("请至少选择一种支付方式");
      return;
    }
    if (single && draft.methods.length > 1) {
      toast.error(`${schema.label}一条渠道只能选一种支付方式`);
      return;
    }
    if (methodChoices.length > 0 && !draft.methods.every((m) => methodChoices.some((c) => c.key === m))) {
      toast.error(`${schema.label}不支持所选的支付方式`);
      return;
    }
    // 结算币种默认跟随站点：schema 的静态默认值是 CNY，美元站点上新建渠道若原样
    // 保留它，渠道会建出来就「不收本站币种」——收银台看不到它，且没有任何报错。
    const params = fillDefaults(schema);
    if ("settle_currency" in params) params.settle_currency = siteCurrency;
    onCreate({
      id,
      name,
      protocol: draft.protocol,
      enabled: false,
      methods: draft.methods,
      params,
    });
    toast.success("渠道已新增（停用状态），填写参数后请打开「启用」");
    onClose();
  };

  return (
    <Dialog
      open
      onClose={onClose}
      title="新增支付渠道"
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            取消
          </button>
          <button type="button" className="btn btn-primary" onClick={submit}>
            新增
          </button>
        </>
      }
    >
      <Field label="支付协议" req tip="决定签名与回调解析走哪套实现；创建后不可修改">
        <SegmentedChips
          items={usable.map((s) => ({ key: s.protocol, label: s.label }))}
          value={draft.protocol}
          onChange={switchProtocol}
        />
      </Field>
      <Field label="渠道名称" req tip="收银台会把它作为支付方式的说明小字；不能含中划线">
        <Input
          type="text"
          value={draft.name}
          placeholder={single ? "虎皮椒（支付宝）" : "易支付二号"}
          onChange={(e) => setDraft({ ...draft, name: e.target.value })}
        />
      </Field>
      <Field
        label="渠道 ID"
        req
        tip="回调地址与历史订单都靠它定位，创建后不可修改；小写字母/数字/下划线/中划线"
      >
        <Input
          type="text"
          className="mono"
          value={draft.id}
          onChange={(e) => setDraft({ ...draft, id: e.target.value })}
        />
      </Field>
      <Field
        label="支付方式"
        req
        tip={
          schema?.method_help ??
          (single
            ? `${schema?.label ?? ""}：一条渠道只能选一种支付方式`
            : `${schema?.label ?? ""}可同时支持两种`)
        }
      >
        {single ? (
          <SegmentedChips
            className="pcs-methods"
            items={methodChoices}
            value={draft.methods[0] ?? ""}
            onChange={(key) => setDraft({ ...draft, methods: [key] })}
          />
        ) : (
          <SegmentedChips
            className="pcs-methods"
            items={methodChoices}
            multiple
            values={draft.methods}
            onChange={(key) =>
              setDraft({
                ...draft,
                methods: draft.methods.includes(key)
                  ? draft.methods.filter((m) => m !== key)
                  : [...draft.methods, key],
              })
            }
          />
        )}
      </Field>
      <div className="alert-banner info">
        <div>
          新增后为<b>停用</b>状态：先在卡片里填好密钥，把回调地址
          <code className="mono small"> {channelNotifyURL(notifyBase, draft.id) || "（需先配置站点地址）"}</code>
          填到上游后台，再打开「启用」。
        </div>
      </div>
    </Dialog>
  );
}
