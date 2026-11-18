import { describe, expect, it } from "vitest";
import {
  FALLBACK_SCHEMAS,
  allowedMethods,
  channelReadiness,
  fillDefaults,
  initialMode,
  isSecretFilled,
  missingRequired,
  schemaOf,
  switchOf,
  visibleParams,
} from "@/lib/payChannelSchema";
import type { PayChannel } from "@/types";

describe("支付渠道协议声明", () => {
  /**
   * 与后端 `payment/schema_test.go: TestSchemaSnapshot` 使用**同一份签名格式**。
   *
   * 两边同时锁住是为了防止滚动发布期间漂移：后端改了字段而前端 FALLBACK 没改，
   * 连旧后端时就会渲染出不同的字段集（运营看到「能填的框消失了」）。
   * 第三份副本在 `e2e/fixtures/payChannelSchemas.ts`。
   */
  it("FALLBACK_SCHEMAS 与后端 schema 形状一致", () => {
    const lines: string[] = [];
    for (const s of FALLBACK_SCHEMAS) {
      lines.push(
        `${s.protocol}|${s.label}|${s.method_mode}|${s.implemented ? "true" : "false"}|${(s.methods ?? []).join(",")}`,
      );
      for (const sw of s.switches ?? []) {
        lines.push(`  @switch ${sw.key}:${sw.default}:${sw.options.map((o) => `${o.value}=${o.label}`).join(",")}`);
      }
      for (const p of s.params) {
        const flags = [
          p.required ? "req" : "",
          p.secret ? "secret" : "",
          p.hidden ? "hidden" : "",
          p.multiline ? "multiline" : "",
        ]
          .filter(Boolean)
          .join(",");
        let row = `  ${p.key}:${p.group}:${flags}:${p.default ?? ""}`;
        if (p.show_when) row += `:when=${p.show_when.field}:${p.show_when.value}`;
        lines.push(row);
      }
    }
    expect(lines.join("\n")).toBe(
      [
        "epay|易支付|multi|true|wechat,alipay",
        "  pay_address:basic:req:",
        "  pid:basic:req:",
        "  key:basic:req,secret:",
        "xunhupay|虎皮椒|single|true|wechat,alipay",
        "  pay_address:basic:req:https://api.xunhupay.com/payment/do.html",
        "  app_id:basic:req:",
        "  app_secret:basic:req,secret:",
        "wxpay|微信直连|single|true|wechat",
        "  @switch verify_mode:pubkey:pubkey=微信支付公钥,cert=平台证书",
        "  app_id:basic:req:",
        "  mch_id:basic:req:",
        "  cert_serial_no:basic:req:",
        "  api_v3_key:basic:req,secret:",
        "  private_key:basic:req,secret,multiline:",
        "  public_key:basic:req,multiline::when=verify_mode:pubkey",
        "  public_key_id:basic:req::when=verify_mode:pubkey",
        "  platform_cert:basic:req,multiline::when=verify_mode:cert",
        "alipay|支付宝直连|single|true|alipay",
        "  app_id:basic:req:",
        "  pay_address:basic:req:https://openapi.alipay.com/gateway.do",
        "  private_key:basic:req,secret,multiline:",
        "  public_key:basic:req,multiline:",
        "stripe|Stripe|single|true|card",
        "  secret_key:basic:req,secret:",
        "  webhook_secret:basic:req,secret:",
        "  settle_currency:basic:req:USD",
      ].join("\n"),
    );
  });

  it("官方直连追加在末尾（默认协议仍是 epay）", () => {
    // 新增弹窗取 usable[0] 作默认协议，e2e 也依赖 schemas[0] === epay。
    expect(FALLBACK_SCHEMAS[0].protocol).toBe("epay");
    expect(FALLBACK_SCHEMAS.map((s) => s.protocol)).toEqual([
      "epay",
      "xunhupay",
      "wxpay",
      "alipay",
      "stripe",
    ]);
  });

  it("allowedMethods 按协议过滤方式（缺省不限制）", () => {
    const all = [
      { key: "wechat", label: "微信支付" },
      { key: "alipay", label: "支付宝" },
      { key: "card", label: "信用卡" },
    ];
    expect(allowedMethods(schemaOf(FALLBACK_SCHEMAS, "wxpay"), all).map((m) => m.key)).toEqual(["wechat"]);
    expect(allowedMethods(schemaOf(FALLBACK_SCHEMAS, "alipay"), all).map((m) => m.key)).toEqual(["alipay"]);
    expect(allowedMethods(schemaOf(FALLBACK_SCHEMAS, "stripe"), all).map((m) => m.key)).toEqual(["card"]);
    expect(allowedMethods(schemaOf(FALLBACK_SCHEMAS, "epay"), all)).toHaveLength(2);
    // methods 缺失（连旧后端）：不限制，避免「一个方式都选不了」→ 返回全部 3 项。
    expect(allowedMethods({ ...schemaOf(FALLBACK_SCHEMAS, "epay")!, methods: undefined }, all)).toHaveLength(3);
  });

  it("所有字段都平铺（高级区已移除），且四个可推导字段不再出现", () => {
    const forbidden = ["query_address", "refund_address", "return_url", "callback_url"];
    for (const s of FALLBACK_SCHEMAS) {
      const keys = s.params.map((p) => p.key);
      for (const k of forbidden) {
        expect(keys).not.toContain(k);
      }
      for (const p of s.params) {
        expect(p.group).toBe("basic");
      }
    }
  });

  it("schemaOf 按协议取声明，空协议返回 undefined", () => {
    expect(schemaOf(FALLBACK_SCHEMAS, "epay")?.label).toBe("易支付");
    expect(schemaOf(FALLBACK_SCHEMAS, "xunhupay")?.method_mode).toBe("single");
    expect(schemaOf(FALLBACK_SCHEMAS, "")).toBeUndefined();
    expect(schemaOf(FALLBACK_SCHEMAS, "unknown")).toBeUndefined();
  });
});

describe("虚拟开关与可见性", () => {
  const wx = schemaOf(FALLBACK_SCHEMAS, "wxpay")!;
  const epay = schemaOf(FALLBACK_SCHEMAS, "epay")!;

  it("只有微信声明了开关，且默认是公钥模式", () => {
    const sw = switchOf(wx);
    expect(sw?.key).toBe("verify_mode");
    expect(sw?.default).toBe("pubkey");
    expect(switchOf(epay)).toBeUndefined();
  });

  it("公钥模式 7 项、证书模式 6 项（互斥字段只显示一组）", () => {
    const pubKeys = visibleParams(wx, {}, "pubkey").map((p) => p.key);
    expect(pubKeys).toEqual([
      "app_id",
      "mch_id",
      "cert_serial_no",
      "api_v3_key",
      "private_key",
      "public_key",
      "public_key_id",
    ]);
    const certKeys = visibleParams(wx, {}, "cert").map((p) => p.key);
    expect(certKeys).toEqual([
      "app_id",
      "mch_id",
      "cert_serial_no",
      "api_v3_key",
      "private_key",
      "platform_cert",
    ]);
  });

  it("两组都填时都可见（换证窗口：出站走公钥、入站按回调头自适应）", () => {
    const keys = visibleParams(
      wx,
      { public_key: "pem", public_key_id: "PUB_KEY_ID_1", platform_cert: "pem2" },
      "pubkey",
    ).map((p) => p.key);
    expect(keys).toContain("public_key");
    expect(keys).toContain("public_key_id");
    expect(keys).toContain("platform_cert");
    expect(keys).toHaveLength(8);
  });

  it("旧后端不下发 switches 时降级为全平铺（永不空白）", () => {
    const legacy = { ...wx, switches: undefined };
    expect(visibleParams(legacy, {}, "")).toHaveLength(8);
  });

  it("initialMode 已有值优先：配了平台证书就进证书模式", () => {
    expect(initialMode(wx, {})).toBe("pubkey");
    expect(initialMode(wx, { platform_cert: "pem" })).toBe("cert");
    expect(initialMode(wx, { public_key: "pem", public_key_id: "PUB_KEY_ID_1" })).toBe("pubkey");
    expect(initialMode(epay, {})).toBe("");
  });
});

describe("fillDefaults", () => {
  it("返回所有非 hidden 键（键集确定，不随默认值变化）", () => {
    const epay = schemaOf(FALLBACK_SCHEMAS, "epay")!;
    expect(Object.keys(fillDefaults(epay)).sort()).toEqual(["key", "pay_address", "pid"]);
    const xunhu = schemaOf(FALLBACK_SCHEMAS, "xunhupay")!;
    const filled = fillDefaults(xunhu);
    expect(filled.pay_address).toBe("https://api.xunhupay.com/payment/do.html");
    expect(filled.app_id).toBe("");
    expect(Object.keys(filled).sort()).toEqual(["app_id", "app_secret", "pay_address"]);
  });

  it("show_when 字段也要填空串（后端全量回写的键集是固定的）", () => {
    const wx = schemaOf(FALLBACK_SCHEMAS, "wxpay")!;
    const filled = fillDefaults(wx);
    expect(Object.keys(filled).sort()).toEqual([
      "api_v3_key",
      "app_id",
      "cert_serial_no",
      "mch_id",
      "platform_cert",
      "private_key",
      "public_key",
      "public_key_id",
    ]);
    expect(filled.public_key).toBe("");
    expect(filled.platform_cert).toBe("");
  });
});

describe("必填与密钥状态", () => {
  const epay = schemaOf(FALLBACK_SCHEMAS, "epay")!;
  const wx = schemaOf(FALLBACK_SCHEMAS, "wxpay")!;

  it("Mask 视为已填", () => {
    const missing = missingRequired(
      epay,
      { pay_address: "https://p.x.cn", pid: "1", key: "******" },
      "******",
      "",
    );
    expect(missing).toEqual([]);
  });

  it("空白串算缺失，不能靠空格绕过必填", () => {
    const missing = missingRequired(
      epay,
      { pay_address: "https://p.x.cn", pid: "   ", key: "" },
      "******",
      "",
    );
    expect(missing.map((p) => p.key)).toEqual(["pid", "key"]);
  });

  it("必填走可见集：证书模式不要求公钥，也不误报「缺 2 项」", () => {
    const base = {
      app_id: "wx1",
      mch_id: "1600000000",
      cert_serial_no: "5157F09EFDC096DE15EBE81A47057A7232F1B8E1",
      api_v3_key: "0123456789abcdef0123456789abcdef",
      private_key: "pem",
    };
    expect(missingRequired(wx, { ...base, platform_cert: "pem2" }, "******", "cert")).toEqual([]);
    expect(missingRequired(wx, base, "******", "cert").map((p) => p.key)).toEqual(["platform_cert"]);
    expect(missingRequired(wx, base, "******", "pubkey").map((p) => p.key)).toEqual([
      "public_key",
      "public_key_id",
    ]);
  });

  it("isSecretFilled 区分已设置/未设置/清空", () => {
    expect(isSecretFilled("******", "******")).toBe(true);
    expect(isSecretFilled("real-secret", "******")).toBe(true);
    expect(isSecretFilled("", "******")).toBe(false);
    expect(isSecretFilled("   ", "******")).toBe(false);
  });

  it("channelReadiness 只提示不拦截（历史启用渠道缺密钥仍然可保存）", () => {
    const ch: PayChannel = {
      id: "epay",
      name: "易支付",
      protocol: "epay",
      enabled: true,
      methods: ["wechat"],
      params: { pay_address: "https://p.x.cn", pid: "1", key: "" },
    };
    expect(channelReadiness(ch, epay, "******", "").incomplete).toBe(true);
    // 协议未知时不产生「缺参数」结论（否则脏数据会一直顶红标）
    expect(channelReadiness(ch, undefined, "******", "").incomplete).toBe(false);
  });

  it("channelReadiness 与参数区共用同一个 mode（切换后计数同步）", () => {
    const ch: PayChannel = {
      id: "wxpay",
      name: "微信直连",
      protocol: "wxpay",
      enabled: false,
      methods: ["wechat"],
      params: {
        app_id: "wx1",
        mch_id: "1600000000",
        cert_serial_no: "5157F09EFDC096DE15EBE81A47057A7232F1B8E1",
        api_v3_key: "0123456789abcdef0123456789abcdef",
        private_key: "pem",
        platform_cert: "pem2",
      },
    };
    // 证书模式：参数齐全
    expect(channelReadiness(ch, wx, "******", "cert").missing).toEqual([]);
    // 公钥模式：缺公钥两项（与参数区显示的字段集一致）
    expect(channelReadiness(ch, wx, "******", "pubkey").missing.map((p) => p.key)).toEqual([
      "public_key",
      "public_key_id",
    ]);
  });
});
