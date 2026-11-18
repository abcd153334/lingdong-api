import { useState } from "react";

import { toastErr } from "@/components/common/Toaster";
import { copyText } from "@/lib/clipboard";
import type { DocV7Field, DocV7NodeModel } from "@/types/docsv7";

import { Inline } from "./Inline";
import {
  chipList,
  defaultText,
  fieldCount,
  groupFields,
  requiredTag,
  showExample,
} from "./render";

/**
 * 请求参数区：若干 `in` 分组卡。
 *
 * 形态是「分组卡片 + 逐项行」而不是大表格：参数描述经常是两三行长文本，
 * 表格列宽会被最长的那条撑开，其余行读起来像被挤扁。
 *
 * 所有判定都来自 `render.ts`（分组、必填、默认值、chip、示例可见性），
 * 这里只做摆放。
 */
export function ParamGroups({
  fields,
  models,
}: {
  fields: DocV7Field[];
  models: DocV7NodeModel[];
}) {
  const groups = groupFields(fields);
  const total = fieldCount(groups);

  return (
    <>
      <div className="d-sec-t">
        <h2>请求参数</h2>
        <span className="n">{total > 0 ? `${total} 个` : "无"}</span>
      </div>

      {/* 四个来源标签的图例。
          为什么要一行图例而不是让读者自己猜：字段行尾的 chip 只有几个字，
          "平台字段"到底是"平台必填"还是"平台自己发明的"，光看标签读不出来。
          而这一层信息恰恰是接入时最想搞清楚的 —— 哪些字段换个模型就不认了。 */}
      {total > 0 && (
        <p className="d-field-legend">
          行尾标签说明字段归属：<b>平台字段</b> 由平台统一提供；
          <b>标准字段</b> 沿用业界标准写法；<b>厂商扩展</b> 只对部分模型生效，换模型要重看；
          <b>兼容写法</b> 是旧写法，平台会自动改写成对应字段。
        </p>
      )}

      {groups.map((g) => (
        <div className="d-pgroup" key={g.id}>
          <div className="d-pgroup-h">
            {g.label}
            <span className="cnt">{g.rows.length} 个</span>
          </div>
          {g.rows.map((f) => (
            <ParamRow key={`${f.in}:${f.name}`} field={f} models={models} />
          ))}
        </div>
      ))}
    </>
  );
}

/** 内嵌模型清单的展示上限：超过 12 个时先折叠，否则 model 行会占满一屏。 */
const MODEL_LIMIT = 12;

function ParamRow({ field: f, models }: { field: DocV7Field; models: DocV7NodeModel[] }) {
  const [expanded, setExpanded] = useState(false);
  const tag = requiredTag(f);
  const chips = chipList(f);
  const def = defaultText(f);
  const example = showExample(f);
  const enums = f.enum ?? [];
  const isModelRow = f.name === "model" && models.length > 0;
  const shown = expanded ? models : models.slice(0, MODEL_LIMIT);

  return (
    <div className="d-prow">
      <div className="d-prow-top">
        <span className="d-pname">{f.name}</span>
        <span className="d-ptype">{f.type}</span>
        <span className={`d-ptag ${tag.cls}`}>{tag.text}</span>
      </div>
      {f.desc ? (
        <div className="d-pdesc">
          <Inline text={f.desc} />
        </div>
      ) : null}
      {f.required === "conditional" && f.required_note ? (
        <div className="d-pdesc d-pnote">
          <Inline text={f.required_note} />
        </div>
      ) : null}
      {enums.length > 0 ? (
        <div className="d-pmeta">
          可选值
          {enums.map((v) => (
            <span
              key={v.value}
              className={`d-chip${v.deprecated ? " dep" : ""}`}
              title={v.desc || ""}
            >
              {v.value}
            </span>
          ))}
        </div>
      ) : null}

      {def !== null || chips.length > 0 ? (
        <div className="d-pmeta">
          {def !== null ? (
            <>
              默认 <span className="d-chip">{def}</span>
            </>
          ) : null}
          {chips.map((c) => (
            <span className="d-chip" key={c}>
              {c}
            </span>
          ))}
        </div>
      ) : null}

      {example !== null ? (
        <div className="d-pex">
          示例 <code>{example}</code>
        </div>
      ) : null}

      {isModelRow ? (
        <div className="d-pmodel">
          <div className="d-pmodel-t">可选模型 {models.length} 个，点击复制</div>
          <div className="d-mchips">
            {shown.map((m) => (
              <ModelChip key={m.id} id={m.id} />
            ))}
            {models.length > MODEL_LIMIT ? (
              <button type="button" className="d-mchip" onClick={() => setExpanded((v) => !v)}>
                {expanded ? "收起" : `展开全部 ${models.length} 个`}
              </button>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}


/**
 * 模型 chip：点一下就复制模型 ID。
 *
 * 文字始终是模型名（点完变成「已复制」会让人不知道复制的是哪个模型），
 * 反馈靠一个短暂的高亮边框；失败必须说出口。
 */
function ModelChip({ id }: { id: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className={`d-mchip${done ? " ok" : ""}`}
      title={`复制 ${id}`}
      onClick={() =>
        void copyText(id).then((ok) => {
          if (!ok) {
            toastErr("复制失败，请手动选中");
            return;
          }
          setDone(true);
          window.setTimeout(() => setDone(false), 1200);
        })
      }
    >
      {id}
    </button>
  );
}
