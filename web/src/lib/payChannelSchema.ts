import type { PayChannel, PayChannelParamSpec, PayChannelSchema, PayChannelSwitch } from "@/types";

/**
 * 后端未下发 schema 时的兜底声明。
 *
 * **必须与 `backend/internal/service/payment/schema.go` 保持一致**：
 * 后端有 `TestSchemaSnapshot` 锁形状，改一处必须同步三处
 * （本文件 / e2e `fixtures/payChannelSchemas.ts`）。
 * 保留兜底的原因：滚动发布期间新前端可能连到旧后端（旧后端只下发 templates）。
 */
export const FALLBACK_SCHEMAS: PayChannelSchema[] = [
  {
    protocol: "epay",
    label: "易支付",
    implemented: true,
    method_mode: "multi",
    methods: ["wechat", "alipay"],
    method_help: "一个商户号可同时收微信与支付宝",
    params: [
      {
        key: "pay_address",
        label: "网关地址",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        placeholder: "https://pay.example.com/",
        tip: "只填域名会自动补 /submit.php",
      },
      {
        key: "pid",
        label: "商户 PID",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        placeholder: "1000",
      },
      {
        key: "key",
        label: "商户密钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        tip: "与上游一致；留空即清空",
      },
    ],
  },
  {
    protocol: "xunhupay",
    label: "虎皮椒",
    implemented: true,
    method_mode: "single",
    methods: ["wechat", "alipay"],
    method_help: "微信与支付宝各需一条渠道",
    params: [
      {
        key: "pay_address",
        label: "网关地址",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        default: "https://api.xunhupay.com/payment/do.html",
        placeholder: "https://api.xunhupay.com/payment/do.html",
      },
      {
        key: "app_id",
        label: "APPID",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        tip: "一条渠道只能对应一套 APPID",
      },
      {
        key: "app_secret",
        label: "APP SECRET",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        tip: "与上游一致；留空即清空",
      },
    ],
  },
  // 官方直连**追加在末尾**：新增弹窗取 usable[0] 作默认协议，e2e 也依赖 schemas[0] === epay。
  {
    protocol: "wxpay",
    label: "微信直连",
    implemented: true,
    method_mode: "single",
    methods: ["wechat"],
    method_help: "仅收微信；多渠道按顺序取第一条",
    switches: [
      {
        key: "verify_mode",
        label: "验签方式",
        default: "pubkey",
        options: [
          { value: "pubkey", label: "微信支付公钥" },
          { value: "cert", label: "平台证书" },
        ],
      },
    ],
    params: [
      {
        key: "app_id",
        label: "AppID",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        tip: "已绑定该商户号的公众号/小程序",
      },
      {
        key: "mch_id",
        label: "商户号",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        placeholder: "1600000000",
        tip: "商户平台的纯数字商户号",
      },
      {
        key: "cert_serial_no",
        label: "证书序列号",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        placeholder: "5157F09EFDC096DE15EBE81A47057A7232F1B8E1",
        tip: "apiclient_cert.pem 里的 serial",
      },
      {
        key: "api_v3_key",
        label: "APIv3 密钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        tip: "32 位，用于解密回调",
      },
      {
        key: "private_key",
        label: "商户私钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        multiline: true,
        tip: "整段粘贴；回显 ****** 表示已保存",
      },
      {
        key: "public_key",
        label: "微信支付公钥",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        multiline: true,
        show_when: { field: "verify_mode", value: "pubkey" },
        tip: "与公钥 ID 必须成对填写",
      },
      {
        key: "public_key_id",
        label: "公钥 ID",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        show_when: { field: "verify_mode", value: "pubkey" },
        placeholder: "PUB_KEY_ID_0000000000000000000000000000000000",
        tip: "PUB_KEY_ID_ 开头加 32 位数字",
      },
      {
        key: "platform_cert",
        label: "平台证书",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        multiline: true,
        show_when: { field: "verify_mode", value: "cert" },
        tip: "换证期间可新旧两张一起粘贴",
      },
    ],
  },
  {
    protocol: "alipay",
    label: "支付宝直连",
    implemented: true,
    method_mode: "single",
    methods: ["alipay"],
    method_help: "仅收支付宝；多渠道按顺序取第一条",
    params: [
      {
        key: "app_id",
        label: "应用 AppID",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        placeholder: "2021000000000000",
        tip: "开放平台的 16 位应用 ID",
      },
      {
        key: "pay_address",
        label: "网关地址",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        default: "https://openapi.alipay.com/gateway.do",
        placeholder: "https://openapi.alipay.com/gateway.do",
        tip: "沙箱环境改填沙箱网关",
      },
      {
        key: "private_key",
        label: "应用私钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        multiline: true,
        tip: "整段粘贴；回显 ****** 表示已保存",
      },
      {
        key: "public_key",
        label: "支付宝公钥",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        multiline: true,
        tip: "是支付宝公钥，不是应用公钥",
      },
    ],
  },
  {
    protocol: "stripe",
    label: "Stripe",
    implemented: true,
    method_mode: "single",
    methods: ["card"],
    method_help: "海外信用卡；支持美元收款",
    params: [
      {
        key: "secret_key",
        label: "Secret Key",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        placeholder: "sk_live_…",
        tip: "从 Stripe Dashboard 复制",
      },
      {
        key: "webhook_secret",
        label: "Webhook 密钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        placeholder: "whsec_…",
        tip: "用于回调验签",
      },
      {
        key: "settle_currency",
        label: "结算币种",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        default: "USD",
        tip: "CNY 或 USD",
      },
    ],
  },
];

/** 该协议允许的支付方式（`methods` 缺失 = 不限制，兼容旧后端）。 */
export function allowedMethods(
  schema: PayChannelSchema | undefined,
  all: { key: string; label: string }[],
): { key: string; label: string }[] {
  if (!schema?.methods || schema.methods.length === 0) return all;
  return all.filter((m) => schema.methods!.includes(m.key));
}

/** 按协议取声明（后端下发的优先，缺失回落兜底）。 */
export function schemaOf(schemas: PayChannelSchema[], protocol: string): PayChannelSchema | undefined {
  if (!protocol) return undefined;
  return schemas.find((s) => s.protocol === protocol);
}

/** 取协议的虚拟单选控件（每个协议目前最多一个）。 */
export function switchOf(schema: PayChannelSchema | undefined): PayChannelSwitch | undefined {
  return schema?.switches?.[0];
}

/**
 * 计算「当前模式」：只在挂载时算一次，由 `PayChannelCard` 持有后下传。
 *
 * 规则：**已有值优先** —— 已配平台证书的渠道一律进 `cert` 模式，
 * 哪怕它的默认模式是 `pubkey`；否则打开一张老渠道卡片会看不到它实际在用的字段。
 */
export function initialMode(schema: PayChannelSchema, params: Record<string, string>): string {
  const sw = switchOf(schema);
  if (!sw) return "";
  for (const opt of sw.options) {
    const filled = schema.params.some(
      (p) =>
        p.show_when?.field === sw.key &&
        p.show_when.value === opt.value &&
        paramValue(params, p.key) !== "",
    );
    if (filled) return opt.value;
  }
  return sw.default || sw.options[0]?.value || "";
}

/**
 * 可见字段（`hidden` 永不渲染）。
 *
 * 三条规则，重点是降级安全：
 *  1. 老后端不下发 `switches`、或该开关不存在 → 一律可见（降级=全平铺，永不空白）；
 *  2. **已填值的字段永远可见** —— 否则「两组都配」时看不见的那组仍在生效，
 *     运营会以为自己没配（换证窗口的标准配置就是两组都填：出站走公钥、
 *     入站按 `Wechatpay-Serial` 前缀自适应）；
 *  3. 其余按当前模式过滤。
 */
export function visibleParams(
  schema: PayChannelSchema,
  params: Record<string, string>,
  mode: string,
): PayChannelParamSpec[] {
  return schema.params.filter((p) => {
    if (p.hidden) return false;
    if (paramValue(params, p.key) !== "") return true;
    const when = p.show_when;
    if (!when) return true;
    if (!schema.switches?.some((s) => s.key === when.field)) return true;
    return mode === when.value;
  });
}

/**
 * 新渠道的初始参数：**所有非 hidden 键**，值取 `default ?? ""`。
 *
 * 键集必须是确定的（不能只填有默认值的键）：请求体键集固定，e2e 才能断言
 * 「新建虎皮椒渠道后恰好 3 项」；否则字段集随协议默认值变化而漂移。
 *
 * 注意：`show_when` 控制的字段**也要填空串** —— 后端 `ChannelParams` 没有
 * omitempty，全量回写的键集是固定的；少了这些键会让「切到证书模式」后的
 * 保存体比预期少几个键，运维排查时对不上。
 */
export function fillDefaults(schema: PayChannelSchema): Record<string, string> {
  const out: Record<string, string> = {};
  for (const p of schema.params) {
    if (p.hidden) continue;
    out[p.key] = p.default ?? "";
  }
  return out;
}

/** 读取参数值（缺失返回空串，避免受控组件从 undefined 变 value 的告警）。 */
export function paramValue(params: Record<string, string>, key: string): string {
  const v = params[key];
  return typeof v === "string" ? v : "";
}

/** 密钥是否已配置：回显的 Mask 也算「已设置」（库里确实有值）。 */
export function isSecretFilled(value: string, mask: string): boolean {
  const v = typeof value === "string" ? value : "";
  if (v === "") return false;
  if (v === mask) return true;
  return v.trim() !== "";
}

/**
 * 缺哪些必填项（Mask 视为已填）。
 *
 * **必须走 `visibleParams`**：否则证书模式下会同时要求公钥，误报「缺 2 项」。
 */
export function missingRequired(
  schema: PayChannelSchema,
  params: Record<string, string>,
  mask: string,
  mode: string,
): PayChannelParamSpec[] {
  return visibleParams(schema, params, mode).filter((p) => {
    if (!p.required) return false;
    const v = paramValue(params, p.key);
    if (v === mask) return false;
    return v.trim() === "";
  });
}

/**
 * 渠道参数完整度（只用于提示，**不禁用启用开关**）。
 *
 * 历史合法状态（如种子时代的启用渠道缺密钥）必须能保存，硬禁用会让整页锁死。
 * `mode` 必须与参数区共用同一个值（由 `PayChannelCard` 持有），
 * 否则「切换验签方式」后卡片的缺失计数会和表单对不上。
 */
export function channelReadiness(
  ch: PayChannel,
  schema: PayChannelSchema | undefined,
  mask: string,
  mode: string,
): { missing: PayChannelParamSpec[]; incomplete: boolean } {
  if (!schema) return { missing: [], incomplete: false };
  const missing = missingRequired(schema, ch.params ?? {}, mask, mode);
  return { missing, incomplete: missing.length > 0 };
}
