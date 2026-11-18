/**
 * e2e 用的协议声明桩（`GET /api/admin/config` 的 `pay_channel_schemas`）。
 *
 * 为什么单独放文件：这是 schema 的**第三份副本**（Go / 前端 FALLBACK / e2e 桩）。
 * 之前它内联在 spec 里，既不能被 vitest 断言、也没法和另外两处一起漂移检测。
 * 抽到 fixtures 后，`web/src/test/payChannelSchemasFixture.test.ts` 会用与
 * 后端 `TestSchemaSnapshot` 相同的签名格式比对，三份副本终于都在网内。
 *
 * 这里刻意不 import 任何项目内模块：e2e 由 Playwright 自己的编译链加载，
 * 引 `@/...` 别名会让它在没有 Vite 解析的环境下崩掉。
 */

export interface StubShowWhen {
  field: string;
  value: string;
}

export interface StubParamSpec {
  key: string;
  label: string;
  group: "basic" | "advanced";
  required: boolean;
  secret: boolean;
  hidden: boolean;
  multiline?: boolean;
  default?: string;
  placeholder?: string;
  tip?: string;
  show_when?: StubShowWhen;
}

export interface StubSwitchOption {
  value: string;
  label: string;
}

export interface StubSwitch {
  key: string;
  label: string;
  default: string;
  options: StubSwitchOption[];
}

export interface StubSchema {
  protocol: string;
  label: string;
  implemented: boolean;
  method_mode: "single" | "multi";
  methods?: string[];
  method_help?: string;
  switches?: StubSwitch[];
  params: StubParamSpec[];
}

export const PAY_CHANNEL_SCHEMAS: StubSchema[] = [
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
      { key: "pid", label: "商户 PID", group: "basic", required: true, secret: false, hidden: false },
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
      },
      { key: "app_id", label: "APPID", group: "basic", required: true, secret: false, hidden: false },
      {
        key: "app_secret",
        label: "APP SECRET",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
      },
    ],
  },
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
      { key: "app_id", label: "AppID", group: "basic", required: true, secret: false, hidden: false },
      { key: "mch_id", label: "商户号", group: "basic", required: true, secret: false, hidden: false },
      {
        key: "cert_serial_no",
        label: "证书序列号",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
      },
      {
        key: "api_v3_key",
        label: "APIv3 密钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
      },
      {
        key: "private_key",
        label: "商户私钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        multiline: true,
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
      },
      {
        key: "public_key_id",
        label: "公钥 ID",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        show_when: { field: "verify_mode", value: "pubkey" },
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
      { key: "app_id", label: "应用 AppID", group: "basic", required: true, secret: false, hidden: false },
      {
        key: "pay_address",
        label: "网关地址",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        default: "https://openapi.alipay.com/gateway.do",
      },
      {
        key: "private_key",
        label: "应用私钥",
        group: "basic",
        required: true,
        secret: true,
        hidden: false,
        multiline: true,
      },
      {
        key: "public_key",
        label: "支付宝公钥",
        group: "basic",
        required: true,
        secret: false,
        hidden: false,
        multiline: true,
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
      { key: "secret_key", label: "Secret Key", group: "basic", required: true, secret: true, hidden: false, placeholder: "sk_live_…" },
      { key: "webhook_secret", label: "Webhook 密钥", group: "basic", required: true, secret: true, hidden: false, placeholder: "whsec_…" },
      { key: "settle_currency", label: "结算币种", group: "basic", required: true, secret: false, hidden: false, default: "USD" },
    ],
  },
];
