import { useState } from "react";

import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { Textarea } from "@/components/common/Textarea";
import type {
  DocsAdminEndpoint,
  DocsAdminEndpointData,
  DocsAdminTransport,
} from "@/types/docsadmin";

import { Card, LinesField, ToggleLine } from "./Bits";
import { ErrorTable } from "./ErrorTable";
import { FieldCard } from "./FieldCard";
import { blankPreset, PresetCard } from "./PresetCard";
import { ExampleTitleInput, SamplesCard } from "./SamplesCard";
import { blankField, move, VariantCard } from "./VariantCard";

const METHODS = ["POST", "GET", "PUT", "PATCH", "DELETE"];
/**
 * 试跑形态：与 `apidoc/field.go` 的 `RunMode` 常量逐项对应。
 *
 * 曾经这里是 `json/form/sse/none` —— `form` 在 Go 侧根本不存在（表单上传由
 * `Transport.Multipart` 表达），而 `binary` 缺失，于是后台存得进一个谁都不认识的值。
 */
const RUN_MODES: [string, string][] = [
  ["json", "JSON 表单"],
  ["sse", "事件流"],
  ["binary", "二进制"],
  ["none", "不提供调试"],
];

/**
 * 接口编辑页：一张卡一节，节内字段与页面上那一块一一对应。
 *
 * 唯一的判断逻辑在「绑定平台端点」：选定之后 phase/badge/方法/路径都会跟着契约走，
 * 所以这里把契约值与当前值并列显示 —— 漂移（E_METHOD_DRIFT / E_PATH_DRIFT）
 * 是发布被拦的最常见原因，改的时候必须看得见。
 */
/** 方言篇与主篇同方法、同路径：不标后缀的话，下拉里会出现两行一模一样的选项。 */
function variantSuffix(docId: string): string {
  const at = docId.indexOf("@");
  return at >= 0 ? ` · ${docId.slice(at + 1)} 报文` : "";
}

export function EndpointForm({
  ep,
  data,
  transports,
  onChange,
  onGenerateSamples,
  onGeneratePresetSamples,
  generating,
  generatingPreset,
}: {
  ep: DocsAdminEndpoint;
  data: DocsAdminEndpointData;
  transports: DocsAdminTransport[];
  onChange: (d: DocsAdminEndpointData) => void;
  onGenerateSamples: () => void;
  onGeneratePresetSamples: (index: number) => void;
  generating: boolean;
  generatingPreset: number | null;
}) {
  const patch = (p: Partial<DocsAdminEndpointData>) => onChange({ ...data, ...p });
  /** 参数卡的整体展开信号：0 收起 / 1 展开；点第二次会翻转（FieldCard 监听数值变化） */
  const [openAll, setOpenAll] = useState(0);
  const bound = transports.find((t) => t.doc_id === data.transport_doc_id);
  const patchField = (i: number, f: (typeof data.fields)[number]) => {
    const next = data.fields.slice();
    next[i] = f;
    patch({ fields: next });
  };
  const patchVariant = (i: number, v: (typeof data.response.variants)[number]) => {
    const next = data.response.variants.slice();
    next[i] = v;
    patch({ response: { ...data.response, variants: next } });
  };

  return (
    <>
      <Card title="基本信息" icon="doc">
        <div className="adp-grid adp-grid--2">
          <label className="adp-mini">
            <span>接口名</span>
            <Input value={data.name} onChange={(e) => patch({ name: e.target.value })} />
          </label>
          <label className="adp-mini">
            <span>顺序（小的在上）</span>
            <Input
              type="number"
              value={data.sort}
              onChange={(e) => patch({ sort: Number(e.target.value) || 0 })}
            />
          </label>
        </div>
        <label className="adp-mini">
          <span>一句话简介（面包屑下面那句）</span>
          <Textarea rows={2} value={data.summary} onChange={(e) => patch({ summary: e.target.value })} />
        </label>

        <div className="adp-grid adp-grid--3">
          <label className="adp-mini">
            <span>方法</span>
            <Select items={METHODS} value={data.method} onChange={(e) => patch({ method: e.target.value })} />
          </label>
          <label className="adp-mini span-2">
            <span>路径</span>
            <Input value={data.path} onChange={(e) => patch({ path: e.target.value })} />
          </label>
        </div>

        <label className="adp-mini">
          <span>绑定平台端点</span>
          <Select
            items={[
              ["", "不绑定（只能存草稿，发布会被拦）"],
              ...transports.map(
                (t) => [t.doc_id, `${t.method} ${t.path}${variantSuffix(t.doc_id)}`] as [string, string],
              ),
            ]}
            value={data.transport_doc_id}
            onChange={(e) => patch({ transport_doc_id: e.target.value })}
          />
        </label>
        {bound ? (
          <p className="adp-bound">
            <Icon name="link" size={14} />
            契约要求 <code>{bound.method}</code> <code>{bound.path}</code>
            {bound.phase !== "sync" ? <span className="adp-chip">{bound.phase === "submit" ? "异步提交" : "任务查询"}</span> : null}
            {data.method !== bound.method || data.path !== bound.path ? (
              <span className="adp-chip adp-chip--warn">与契约不一致，发布会被拦</span>
            ) : null}
          </p>
        ) : null}

        <label className="adp-mini">
          <span>别名（一行一个，显示在路径旁）</span>
          <LinesField value={data.aliases} rows={2} ariaLabel="路径别名" onChange={(aliases) => patch({ aliases })} />
        </label>

        <div className="adp-grid adp-grid--3">
          <label className="adp-mini">
            <span>试跑形态</span>
            <Select
              items={RUN_MODES}
              value={data.run_mode}
              onChange={(e) => patch({ run_mode: e.target.value })}
            />
          </label>
          <label className="adp-mini">
            <span>请求体上限</span>
            <Input
              value={data.max_body}
              placeholder="如 20MB；留空不显示"
              onChange={(e) => patch({ max_body: e.target.value })}
            />
          </label>
          <label className="adp-mini">
            <span>不提供试跑的原因</span>
            <Input
              value={data.run_hint}
              placeholder="如「需要真实任务号」；留空不显示"
              onChange={(e) => patch({ run_hint: e.target.value })}
            />
          </label>
        </div>

        <label className="adp-mini">
          <span>注意事项（一行一条，显示在参数表上方）</span>
          <LinesField value={data.notes} rows={3} ariaLabel="注意事项" onChange={(notes) => patch({ notes })} />
        </label>

        <ToggleLine
          label="隐藏这篇接口"
          checked={data.hidden}
          onChange={(hidden) => patch({ hidden })}
        />
      </Card>

      <Card
        title="参数"
        icon="rows"
        actions={
          <>
            {data.fields.length > 1 ? (
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                onClick={() => setOpenAll((v) => (v === 0 ? 1 : 0))}
                title={openAll ? "把所有字段卡收起来" : "一次把所有字段卡摊开"}
              >
                <Icon name={openAll ? "chevronUp" : "chevronDown"} size={14} />
                {openAll ? "全部收起" : "全部展开"}
              </button>
            ) : null}
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              onClick={() => patch({ fields: [...data.fields, blankField("body")] })}
            >
              <Icon name="plus" size={14} />
              加字段
            </button>
          </>
        }
      >
        {data.fields.length === 0 ? (
          <p className="adp-empty">还没有参数。没有参数的接口（如查询类）也可以保存。</p>
        ) : null}
        {data.fields.map((f, i) => (
          <FieldCard
            key={i}
            field={f}
            index={i}
            total={data.fields.length}
            mode="request"
            openAll={openAll}
            onChange={(nf) => patchField(i, nf)}
            onMove={(dir) => patch({ fields: move(data.fields, i, i + dir) })}
            onRemove={() => patch({ fields: data.fields.filter((_, k) => k !== i) })}
          />
        ))}
      </Card>

      <Card
        title="响应"
        icon="activity"
        tip="按状态码分别设置：202 常是裸任务对象，200 常走统一封套。"
        actions={
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() =>
              patch({
                response: {
                  ...data.response,
                  variants: [
                    ...data.response.variants,
                    { status: 200, label: "成功", content_type: "", kind: "json", envelope: false, returns: [], example: [], raw: "", notes: [] },
                  ],
                },
              })
            }
          >
            <Icon name="plus" size={14} />
            加一条响应
          </button>
        }
      >
        {data.response.variants.length === 0 ? (
          <p className="adp-empty">还没有响应变体。发布要求至少有一条 2xx。</p>
        ) : null}
        {data.response.variants.map((v, i) => (
          <VariantCard
            key={i}
            variant={v}
            index={i}
            total={data.response.variants.length}
            onChange={(nv) => patchVariant(i, nv)}
            onMove={(dir) =>
              patch({ response: { ...data.response, variants: move(data.response.variants, i, i + dir) } })
            }
            onRemove={() =>
              patch({
                response: {
                  ...data.response,
                  variants: data.response.variants.filter((_, k) => k !== i),
                },
              })
            }
          />
        ))}
      </Card>

      <Card
        title="示例代码"
        icon="book"
        tip="{{base_url}} 会替换成站点地址；「生成」按当前字段的示例值重写。"
      >
        <SamplesCard
          title="主示例"
          value={data.samples}
          onChange={(samples) => patch({ samples })}
          onGenerate={onGenerateSamples}
          generating={generating}
          onClear={() => patch({ samples: null })}
        />
        <div className="adp-samples2">
          <ExampleTitleInput
            value={data.example_title2}
            onChange={(example_title2) => patch({ example_title2 })}
          />
          <SamplesCard
            title="第二种写法"
            hint="留空则页面只显示主示例"
            value={data.samples2}
            onChange={(samples2) => patch({ samples2 })}
            onClear={() => patch({ samples2: null })}
          />
        </div>
      </Card>

      <Card
        title="调用方式"
        icon="split"
        tip="同一路径的几种用法（如图生图 / 图生视频），每种带自己的示例代码。"
        actions={
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() => patch({ presets: [...data.presets, blankPreset()] })}
          >
            <Icon name="plus" size={14} />
            加一种
          </button>
        }
      >
        {data.presets.length === 0 ? <p className="adp-empty">还没有调用方式。</p> : null}
        {data.presets.map((p, i) => (
          <PresetCard
            key={i}
            preset={p}
            index={i}
            total={data.presets.length}
            onChange={(np) => {
              const next = data.presets.slice();
              next[i] = np;
              patch({ presets: next });
            }}
            onMove={(dir) => patch({ presets: move(data.presets, i, i + dir) })}
            onRemove={() => patch({ presets: data.presets.filter((_, k) => k !== i) })}
            onGenerate={() => onGeneratePresetSamples(i)}
            generating={generatingPreset === i}
          />
        ))}
      </Card>

      <Card title="错误码" icon="alert">
        <ErrorTable value={data.errors} onChange={(errors) => patch({ errors })} />
      </Card>

      <p className="adp-note">
        <Icon name="alert" size={14} />
        接口标识 <code>{ep.key}</code> 建好后不能改（它是 URL 的一部分）。
      </p>
    </>
  );
}
