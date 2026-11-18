/**
 * 支付方式品牌图标（内联 SVG，无外部依赖、无字体图标）。
 *
 * 为什么不用 Icon 组件：那套是单色 stroke 图标，支付品牌必须带自己的识别色
 * （微信绿 / 支付宝蓝），否则「两张一样的灰卡片」等于没做区分。
 *
 * 为什么内联而不是图片：收银台是首屏关键路径，多一个 PNG 就多一次请求；
 * 且 `data:`/外链在部分 WebView 里会被内容安全策略拦掉。
 */
interface Props {
  /** 支付方式枚举 code（后端下发）；无法识别的值走 generic 兜底 */
  method: string;
  size?: number;
}

/** 品牌兜底色（与 --primary 分离：不能让品牌色随主题漂移） */
export function PayMethodBrand(method: string): "wechat" | "alipay" | "other" {
  switch (method) {
    case "wechat":
      return "wechat";
    case "alipay":
      return "alipay";
    default:
      return "other";
  }
}

export function PayMethodIcon({ method, size = 26 }: Props) {
  const brand = PayMethodBrand(method);
  return (
    <span
      className="pay-ch-ic"
      data-brand={brand}
      style={{ width: size, height: size }}
      aria-hidden="true"
    >
      {brand === "wechat" ? (
        // 微信：双气泡
        <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor">
          <path d="M9.1 3.2c-3.9 0-7.1 2.6-7.1 5.9 0 1.8 1 3.4 2.6 4.5l-.7 2 2.3-1.2c.6.2 1.3.3 2 .3h.5a5.4 5.4 0 0 1-.2-1.4c0-3.1 3-5.6 6.7-5.6h.6c-.7-2.6-3.5-4.5-6.7-4.5Zm-2.4 3.1a.9.9 0 1 1 0 1.8.9.9 0 0 1 0-1.8Zm4.8 0a.9.9 0 1 1 0 1.8.9.9 0 0 1 0-1.8Z" />
          <path d="M22.1 13.2c0-2.6-2.6-4.7-5.8-4.7s-5.8 2.1-5.8 4.7 2.6 4.7 5.8 4.7c.7 0 1.4-.1 2-.3l1.9 1-.6-1.7c1.5-.9 2.5-2.2 2.5-3.7Zm-7.7-1.3a.8.8 0 1 1 0 1.6.8.8 0 0 1 0-1.6Zm3.8 0a.8.8 0 1 1 0 1.6.8.8 0 0 1 0-1.6Z" />
        </svg>
      ) : brand === "alipay" ? (
        // 支付宝：品牌字标（「支」是它的识别符号，且不依赖品牌字体文件）
        <svg viewBox="0 0 24 24" width="16" height="16">
          <text
            x="12"
            y="17"
            textAnchor="middle"
            fontSize="16"
            fontWeight="700"
            fill="currentColor"
            fontFamily="'PingFang SC','Microsoft YaHei',sans-serif"
          >
            支
          </text>
        </svg>
      ) : (
        // 聚合支付/未知：钱包轮廓
        <svg
          viewBox="0 0 24 24"
          width="15"
          height="15"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.8"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <rect x="2.5" y="5.5" width="19" height="13" rx="2.5" />
          <path d="M2.5 10h19" />
        </svg>
      )}
    </span>
  );
}
