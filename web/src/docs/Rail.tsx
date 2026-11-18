import { useState } from "react";

import type { DocV7Endpoint, DocV7Samples } from "@/types/docsv7";

import { CopyBtn } from "./CopyBtn";
import { JsonBlock, PlainBlock, stringify } from "./code";
import { samplesOf, variantKind, variantLabel } from "./render";
import { TryPanel } from "./TryPanel";

/**
 * 右栏：请求示例 / 响应示例（`mode="docs"`）或在线运行（`mode="try"`）。
 *
 * 示例代码**不在前端拼**：后端 `BuildSamples` 是唯一生成处（它知道真实域名、
 * 预设体、multipart 该用 -F 还是 -d）。前端再拼一套的话，页面上的代码与导出
 * Markdown 里的代码会各说一套，而读者只会信页面上那一份。
 */

type Lang = keyof DocV7Samples;

const LANGS: { id: Lang; label: string }[] = [
  { id: "curl", label: "cURL" },
  { id: "python", label: "Python" },
  { id: "node", label: "Node.js" },
  { id: "go", label: "Go" },
];

export function Rail({
  doc,
  baseUrl,
  mode,
  onMode,
  embedded = false,
}: {
  doc: DocV7Endpoint;
  baseUrl: string;
  mode: "docs" | "try";
  onMode: (m: "docs" | "try") => void;
  /** 工作台窄栏预览：不提供在线运行（会把管理员浏览器当成终端用户）。 */
  embedded?: boolean;
}) {
  if (mode === "try" && !embedded) {
    return <TryPanel doc={doc} baseUrl={baseUrl} onBack={() => onMode("docs")} />;
  }
  return <ExampleRail doc={doc} />;
}

function ExampleRail({ doc }: { doc: DocV7Endpoint }) {
  const [lang, setLang] = useState<Lang>("curl");
  const [preset, setPreset] = useState(0);
  const [variant, setVariant] = useState(0);

  const presets = doc.presets ?? [];
  const safePreset = Math.min(preset, Math.max(presets.length - 1, 0));
  const samples = samplesOf(doc, safePreset);
  const variants = doc.response.variants ?? [];
  const safeVariant = Math.min(variant, Math.max(variants.length - 1, 0));
  const v = variants[safeVariant];
  const kind = v ? variantKind(v) : "empty";

  return (
    <>
      <div className="d-rail-block">
        <div className="d-rail-h">
          <b>请求示例</b>
          <span className="sp" />
          <CopyBtn text={samples?.[lang] ?? ""} disabled={!samples} title="复制这段示例" />
        </div>
        {presets.length > 1 ? (
          <div className="d-tabs">
            {presets.map((p, i) => (
              <button
                key={p.key}
                type="button"
                className={safePreset === i ? "on" : ""}
                title={p.summary}
                onClick={() => setPreset(i)}
              >
                {p.label}
              </button>
            ))}
          </div>
        ) : null}
        <div className="d-tabs">
          {LANGS.map((l) => (
            <button
              key={l.id}
              type="button"
              className={lang === l.id ? "on" : ""}
              onClick={() => setLang(l.id)}
            >
              {l.label}
            </button>
          ))}
        </div>
        {samples ? (
          <PlainBlock text={samples[lang]} />
        ) : (
          <div className="d-rail-empty">这个接口没有示例代码。</div>
        )}
      </div>

      {doc.samples2 ? (
        <div className="d-rail-block">
          <div className="d-rail-h">
            <b>{doc.example_title2 || "另一种写法"}</b>
            <span className="sp" />
            <CopyBtn text={doc.samples2[lang] ?? ""} title="复制这段示例" />
          </div>
          <PlainBlock text={doc.samples2[lang]} />
        </div>
      ) : null}

      <div className="d-rail-block">
        <div className="d-rail-h">
          <b>响应示例</b>
          <span className="sp" />
          <span className="sub">
            {variants.length > 0 ? `${variants.length} 种状态` : "无"}
          </span>
          <CopyBtn
            text={v ? (kind === "raw" ? (v.raw ?? "") : kind === "json" ? stringify(v.example) : "") : ""}
            disabled={!v || kind === "empty"}
            title="复制这段响应"
          />
        </div>
        {variants.length > 1 ? (
          <div className="d-tabs">
            {variants.map((x, i) => (
              <button
                key={`${x.status}-${i}`}
                type="button"
                className={safeVariant === i ? "on" : ""}
                onClick={() => setVariant(i)}
              >
                {variantLabel(x)}
              </button>
            ))}
          </div>
        ) : null}
        {!v ? (
          <div className="d-rail-empty">这个接口没有响应示例。</div>
        ) : kind === "raw" ? (
          <PlainBlock text={v.raw ?? ""} />
        ) : kind === "json" ? (
          <JsonBlock value={v.example} />
        ) : (
          <div className="d-rail-empty">这个状态码没有示例。</div>
        )}
      </div>
    </>
  );
}
