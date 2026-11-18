import { useEffect, useState } from "react";

interface Props {
  /** 二维码内容（微信 `weixin://...` / 支付宝 `https://qr.alipay.com/...`） */
  content: string;
}

/**
 * 内容型支付二维码的前端渲染。
 *
 * 为什么在前端画：微信 Native 与支付宝当面付返回的都是**内容**（不是图片地址），
 * 后端没有二维码生成库、也不该为了画图去引入一个（更不该把用户可见的支付链接
 * 发到第三方服务上去生成）。
 *
 * 为什么动态 import：`qrcode` 只在这个分支用到，静态引入会让收银台首屏包体
 * 多出几十 KB；动态 import 让 Vite 自动分包，只有真的需要扫码时才加载。
 *
 * 失败静默降级：返回 null，调用方保留「复制链接」按钮 —— 画不出二维码时
 * 用户仍能把链接贴到手机上，比显示一张裂图好。
 */
export function PayQRCode({ content }: Props) {
  const [dataURL, setDataURL] = useState("");
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    setDataURL("");
    setFailed(false);
    if (!content) return () => { alive = false; };

    void (async () => {
      try {
        const mod = await import("qrcode");
        const url = await mod.toDataURL(content, {
          margin: 1,
          width: 200,
          errorCorrectionLevel: "M",
        });
        if (alive) setDataURL(url);
      } catch {
        if (alive) setFailed(true);
      }
    })();
    return () => {
      alive = false;
    };
  }, [content]);

  if (failed || !dataURL) return null;
  return (
    <img
      className="pay-qr-img"
      src={dataURL}
      alt="支付二维码"
      data-testid="pay-qr-rendered"
    />
  );
}
