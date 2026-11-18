import { useEffect, useRef, useState } from "react";

import { Icon } from "@/components/common/Icon";
import { jsonParts } from "@/docs/code";
import { copyText } from "@/lib/clipboard";

/**
 * 开放接口手册（`/admin/api-docs`）的 JSON 代码块。
 *
 * 高亮分词走 `@/docs/code` 的 `jsonParts`：全站只有那一份正则。此前手册页自己
 * 用纯文本渲染、文档站另写一套 span，同一段 JSON 在两个页面上长得不一样，
 * 读者分不清哪个是"真实返回"。
 *
 * 容器仍是 `.api-codeblock`（`.code-block` 的深色底 + 头部工具条），
 * 与手册页其余代码块一致；入参 `{text, testId}` 保持不变，调用方不用改。
 */
export function JsonBlock({ text, testId }: { text: string; testId?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  const copy = () => {
    void copyText(text).then((ok) => {
      if (!ok) return;
      setCopied(true);
      timer.current = window.setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <div className="api-codeblock" data-testid={testId}>
      <div className="api-codetabs">
        <span className="api-codetag">json</span>
        <button type="button" className={`api-copybtn${copied ? " on" : ""}`} onClick={copy}>
          <Icon name={copied ? "check" : "copy"} size={13} />
          {copied ? "已复制" : "复制"}
        </button>
      </div>
      <pre className="code-block api-codepre">{jsonParts(text)}</pre>
    </div>
  );
}
