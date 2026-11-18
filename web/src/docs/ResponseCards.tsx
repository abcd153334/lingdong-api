import { useMemo, useState } from "react";

import type { DocV7Field, DocV7Variant } from "@/types/docsv7";

import { buildTree, emptyReturnsText, statusTone, type TreeNode } from "./render";

/**
 * 响应：状态卡（可折叠）+ 字段树。
 *
 * 字段在后端是**扁平名字**（`contents[].url` / `choices[].message.content`），
 * 层级由名字里的 `.` 与 `[]` 派生 —— 库里不加父子列，避免第二套结构。
 * 构树与 `sketch_skip` 过滤都在 `render.buildTree`（与其他三处共用同一份规则）。
 */

/** 一行字段；有子节点时可折叠。 */
function Row({ node, depth }: { node: TreeNode; depth: number }) {
  const [closed, setClosed] = useState(false);
  const hasKids = node.children.length > 0;
  const type = hasKids ? (node.arr ? "array [object]" : "object") : (node.row?.type ?? "");
  const required = node.row?.required === "required";
  return (
    <>
      <div className={`d-rrow${closed ? " closed" : ""}`} style={{ paddingLeft: 14 + depth * 18 }}>
        {hasKids ? (
          <button
            type="button"
            className="tri"
            aria-label={closed ? "展开" : "收起"}
            aria-expanded={!closed}
            onClick={() => setClosed((v) => !v)}
          />
        ) : (
          <span className="sp" />
        )}
        <span className="d-rname">{node.name}</span>
        <span className="d-rtype">{type}</span>
        <span className={`d-rreq${required ? "" : " opt"}`}>{required ? "必填" : "可选"}</span>
      </div>
      {hasKids && !closed
        ? node.children.map((c) => <Row key={c.key} node={c} depth={depth + 1} />)
        : null}
    </>
  );
}

export function ResponseTree({ rows }: { rows: DocV7Field[] }) {
  const tree = useMemo(() => buildTree(rows), [rows]);
  if (tree.length === 0) return null;
  return (
    <div className="d-rtree">
      {tree.map((n) => (
        <Row key={n.key} node={n} depth={0} />
      ))}
    </div>
  );
}

/** 一个状态码一张卡；第一种默认展开（读者最常看的是成功态）。 */
export function VariantCard({ variant, first }: { variant: DocV7Variant; first: boolean }) {
  const tone = statusTone(variant.status);
  const ct =
    variant.content_type && variant.content_type !== "application/json" ? variant.content_type : "";
  const hasReturns = (variant.returns ?? []).length > 0;
  return (
    <details className="d-rcard" open={first}>
      <summary>
        <span className={`d-dot ${tone}`} />
        <span className="d-stcode">{variant.status}</span>
        <span className="d-lbl">{variant.label}</span>
        {ct ? <span className="d-ctype">{ct}</span> : null}
        <span className="d-chev" />
      </summary>
      {variant.notes && variant.notes.length > 0 ? (
        <div className="d-rnotes">{variant.notes.join(" · ")}</div>
      ) : null}
      {hasReturns ? (
        <ResponseTree rows={variant.returns} />
      ) : (
        <p className="d-hint" style={{ padding: "10px 14px 14px" }}>
          {emptyReturnsText(variant.example !== undefined && variant.example !== null || !!variant.raw)}
        </p>
      )}
    </details>
  );
}
