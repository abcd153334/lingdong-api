/**
 * 生成文档站 e2e 的 fixtures（真实接口快照）。
 *
 * 为什么不用手写桩：上一版手写的接口对象少了 `notes` 字段，前端 `doc.notes.length`
 * 直接抛错，整页白屏 —— 而用例当时是绿的（只断言 testid 存在）。手写桩与真实
 * 契约之间没有任何机制保证一致，快照至少有「生成时间点等于真实响应」。
 *
 * 用法（后端已启动在 8080）：
 *   node scripts/gen-docs-fixtures.mjs
 *
 * 产物提交进仓库；契约变更后重新生成即可，用例断言的是结构与几何，
 * 不依赖具体某个模型名。
 */
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const API = process.env.DOCS_API || "http://127.0.0.1:8080";
const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, "..", "e2e", "fixtures", "docs-v7");

const get = async (path) => {
  const res = await fetch(API + path);
  if (!res.ok) throw new Error(`${path} → HTTP ${res.status}`);
  const body = await res.json();
  if (!body || body.data === undefined) throw new Error(`${path} 没有统一信封里的 data`);
  return body.data;
};

const tree = await get("/api/docs/tree");
const nodes = tree.sections.flatMap((s) => s.nodes);
// 选一个「有接口、接口带请求体与响应变体」的节点：对话补全最能覆盖页面所有块。
// 找不到就退到第一个有接口的节点，保证脚本在任何数据集上都能跑出可用快照。
const target =
  nodes.find((n) => n.key === "openai" && n.endpoints.length > 0) ??
  nodes.find((n) => n.endpoints.length > 0);
if (!target) throw new Error("目录里没有任何带接口的节点，先发布文档再生成快照");

const endpointRef = target.endpoints.find((e) => e.key.includes("chat")) ?? target.endpoints[0];
const node = await get(`/api/docs/nodes/${encodeURIComponent(target.key)}`);
const endpoint = await get(
  `/api/docs/nodes/${encodeURIComponent(target.key)}/endpoints/${encodeURIComponent(endpointRef.key)}`,
);

mkdirSync(OUT, { recursive: true });
const write = (name, value) => writeFileSync(join(OUT, name), `${JSON.stringify(value, null, 2)}\n`, "utf8");

write("tree.json", tree);
write("node.json", node);
write("endpoint.json", endpoint);
// 用例从这里读路由，避免把某个接口 key 硬编码进断言（契约改名时只重新生成）。
write("index.json", { node: target.key, endpoint: endpointRef.key });

// 第二份快照：GPT Image 的图片生成。
//
// 单独快照的理由不是「多覆盖一个页面」，而是它守两件**只在这一页成立**的事：
//   1. `size` / `quality` 按模型分组陈述（同一个字段在不同模型上规则不同）；
//   2. `stream` / `partial_images` **不存在** —— 这一族没有模型声明流式出图。
// 第 2 条曾经是反的（文档写着「响应是 SSE」，实际网关把 stream 剥掉了），
// 有快照才能让「它回来了」变成一条红用例，而不是又一次静默回归。
const gi = nodes.find((n) => n.key === "images-gpt-image");
if (gi && gi.endpoints.length > 0) {
  const giRef = gi.endpoints.find((e) => e.key === "ep-images-generations") ?? gi.endpoints[0];
  const giEp = await get(
    `/api/docs/nodes/${encodeURIComponent(gi.key)}/endpoints/${encodeURIComponent(giRef.key)}`,
  );
  write("endpoint-image.json", giEp);
  write("index-image.json", { node: gi.key, endpoint: giRef.key });
}

console.log(`fixtures 已写入 ${OUT}`);
console.log(`节点 ${target.key} / 接口 ${endpointRef.key}（${endpoint.fields.length} 个字段）`);
