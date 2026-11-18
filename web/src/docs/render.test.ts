import { describe, expect, it } from "vitest";

import type { DocV7Endpoint, DocV7Field } from "@/types/docsv7";

import {
  authOf,
  bodyAllowed,
  buildTree,
  buildTryRequest,
  canTry,
  chipList,
  coerce,
  curlOf,
  defaultOn,
  defaultText,
  fieldCount,
  fieldValue,
  formBodyOf,
  fillPath,
  groupFields,
  headerRows,
  initialValue,
  isComplexType,
  jsonBodyError,
  missingPathParam,
  phaseTags,
  prettyJson,
  requiredTag,
  samplesOf,
  showExample,
  statusHint,
  timeoutOf,
  variantKind,
  variantLabel,
} from "./render";

/**
 * `render.ts` 是文档站唯一的判定层：分组、必填、默认值、chip、响应树、
 * 鉴权、可调试性。这些规则一旦错，页面不会崩 —— 只会对读者说假话
 * （把可传的字段标成不展示、把不能调的接口给出「发送」按钮）。
 *
 * 因此这里按「规则 → 反例」成对断言，而不是只测正常路径。
 */

const F = (patch: Partial<DocV7Field> & { name: string }): DocV7Field => ({
  type: "string",
  in: "body",
  required: "optional",
  default: "",
  desc: "",
  tier: "S",
  ...patch,
});

describe("分组", () => {
  it("header 行照原样成组：后台能编辑的内容不许被前端藏掉", () => {
    const groups = groupFields([
      F({ name: "Authorization", in: "header" }),
      F({ name: "prompt" }),
    ]);
    expect(groups.map((g) => g.id)).toEqual(["header", "body"]);
    expect(fieldCount(groups)).toBe(2);
  });

  it("顺序固定 path → query → header → body，与库里写入顺序无关", () => {
    const groups = groupFields([
      F({ name: "b", in: "body" }),
      F({ name: "h", in: "header" }),
      F({ name: "p", in: "path" }),
      F({ name: "q", in: "query" }),
    ]);
    expect(groups.map((g) => g.id)).toEqual(["path", "query", "header", "body"]);
  });

  it("未知 in 追加到末尾而不是丢弃", () => {
    const groups = groupFields([F({ name: "c", in: "cookie" })]);
    expect(groups).toHaveLength(1);
    expect(groups[0].label).toBe("cookie 参数");
  });
});

describe("必填与默认值", () => {
  it("三态各自有标签", () => {
    expect(requiredTag(F({ name: "a", required: "required" })).text).toBe("必填");
    expect(requiredTag(F({ name: "a", required: "conditional" })).text).toBe("条件必填");
    expect(requiredTag(F({ name: "a" })).text).toBe("可选");
  });

  it("默认值原样展示：写了什么就显示什么，空串才不显示", () => {
    expect(defaultText(F({ name: "a", default: "—" }))).toBe("—");
    expect(defaultText(F({ name: "a", default: "模型上限" }))).toBe("模型上限");
    expect(defaultText(F({ name: "a", default: "0" }))).toBe("0");
    expect(defaultText(F({ name: "a", default: "false" }))).toBe("false");
    expect(defaultText(F({ name: "a", default: "  " }))).toBeNull();
  });
});

describe("chip", () => {
  // 曾经只显示 V/N，理由是"P 占绝大多数，逐行挂等于刷噪音"。
  // 但那个理由的前提是读者知道"没标注 = 平台字段" —— 实际反馈是没人知道，
  // 于是"哪些字段是平台自己发明的"变成说不清的问题。现在四值全显，
  // 噪音用弱化样式解决（P 的 chip 在页面上是灰的）。
  it("四个层级都出标签，空值不出", () => {
    expect(chipList(F({ name: "a", tier: "P" }))).toEqual(["平台字段"]);
    expect(chipList(F({ name: "a", tier: "S" }))).toEqual(["标准字段"]);
    expect(chipList(F({ name: "a", tier: "standard" }))).toEqual(["标准字段"]);
    expect(chipList(F({ name: "a", tier: "V" }))).toEqual(["厂商扩展"]);
    expect(chipList(F({ name: "a", tier: "N" }))).toEqual(["兼容写法"]);
    expect(chipList(F({ name: "a", tier: "" }))).toEqual([]);
  });

  it("取值随模型只在来源确实由模型决定时出现", () => {
    expect(chipList(F({ name: "a", value_source: "image_tier" }))).toContain("取值随模型");
    expect(chipList(F({ name: "a", value_source: "static" }))).not.toContain("取值随模型");
    // model 字段的取值就是模型清单（已有 chips 表达），不该再说一遍
    expect(chipList(F({ name: "model", value_source: "group_members" }))).not.toContain("取值随模型");
  });

  it("约束文案逐字使用，不二次格式化", () => {
    const chips = chipList(
      F({ name: "a", tier: "", constraints: [{ kind: "range", value: 0, text: "1 ~ 4096" }] }),
    );
    expect(chips).toEqual(["1 ~ 4096"]);
  });
});

describe("示例", () => {
  it("对象与数组不展开成一行", () => {
    expect(showExample(F({ name: "a", example: { x: 1 } }))).toBe("{…}");
    expect(showExample(F({ name: "a", example: [1, 2] }))).toBe("[2 项]");
    expect(showExample(F({ name: "a", example: null }))).toBeNull();
  });

  it("与默认值相同不重复展示", () => {
    expect(showExample(F({ name: "a", default: "1", example: 1 }))).toBeNull();
  });

  it("model 行不展示示例（已有模型 chips）", () => {
    expect(showExample(F({ name: "model", example: "kling-v2" }))).toBeNull();
  });
});

describe("响应树", () => {
  it("点分名字构成三层，同 key 复用父节点", () => {
    const tree = buildTree([
      F({ name: "id" }),
      F({ name: "choices[].message.content" }),
      F({ name: "choices[].message.role" }),
      F({ name: "choices[].finish_reason" }),
    ]);
    expect(tree.map((n) => n.name)).toEqual(["id", "choices"]);
    const choices = tree[1];
    expect(choices.arr).toBe(true);
    // 同一父节点下的两个子字段共用一个 message 节点，不会各建一份
    expect(choices.children.map((c) => c.name)).toEqual(["message", "finish_reason"]);
    expect(choices.children[0].children.map((c) => c.name)).toEqual(["content", "role"]);
  });

  it("sketch_skip 的字段连带子树一起剔除", () => {
    const tree = buildTree([
      F({ name: "result" }),
      F({ name: "result.alt", sketch_skip: true }),
      F({ name: "result.alt.url" }),
      F({ name: "result.main" }),
    ]);
    const result = tree[0];
    expect(result.children.map((c) => c.name)).toEqual(["main"]);
  });
});

describe("鉴权", () => {
  it("Messages 走 x-api-key，其余走 Bearer", () => {
    expect(authOf({ path: "/v1/messages" }).header).toBe("x-api-key");
    expect(authOf({ path: "/v1/chat/completions" }).header).toBe("Authorization");
  });
});

describe("页头标签", () => {
  it("流式是独立标签，不覆盖 phase", () => {
    expect(phaseTags({ phase: "同步", run_mode: "sse" }).map((t) => t.text)).toEqual([
      "同步",
      "流式 SSE",
    ]);
    expect(phaseTags({ phase: "异步提交", run_mode: "json" }).map((t) => t.text)).toEqual(["异步提交"]);
  });
});

describe("示例选择", () => {
  const doc = {
    presets: [{ samples: { curl: "a", python: "", node: "", go: "" } }],
    samples: { curl: "b", python: "", node: "", go: "" },
  } as unknown as DocV7Endpoint;

  it("预设自带示例优先于接口级", () => {
    expect(samplesOf(doc, 0)?.curl).toBe("a");
  });

  it("两者都没有时返回 null（让调用方渲染空态，而不是空代码块）", () => {
    expect(samplesOf({ presets: [], samples: undefined } as unknown as DocV7Endpoint, 0)).toBeNull();
  });
});

describe("响应变体", () => {
  it("raw 优先于 json", () => {
    expect(variantKind({ status: 200, label: "", returns: [], raw: "text", example: { a: 1 } })).toBe("raw");
    expect(variantKind({ status: 200, label: "", returns: [], example: { a: 1 } })).toBe("json");
    expect(variantKind({ status: 200, label: "", returns: [] })).toBe("empty");
  });

  it("页签文案说清种类", () => {
    expect(variantLabel({ status: 400, label: "", returns: [], raw: "{}" })).toBe("400 原文");
    expect(variantLabel({ status: 202, label: "", returns: [], example: {} })).toBe("202 JSON");
  });
});

describe("在线运行可用性", () => {
  const base = "https://api.example.com";

  it("run_mode=none 时原因取自契约的 run_hint（每篇不同，不再是同一句）", () => {
    const gate = canTry(
      { run_mode: "none", transport: "json", run_hint: "文件直链由上传接口返回，请直接打开响应里的 url。" },
      base,
    );
    expect(gate.ok).toBe(false);
    expect(gate.reason).toContain("文件直链");
  });

  it("run_mode=none 且契约没写原因时给一句兜底", () => {
    expect(canTry({ run_mode: "none", transport: "json" }, base).reason).toBe(
      "这条接口不支持在线调试。",
    );
  });

  it("transport=multipart 不可用（取值是 multipart，不是 multipart/form-data）", () => {
    const gate = canTry({ run_mode: "json", transport: "multipart" }, base);
    expect(gate.ok).toBe(false);
    expect(gate.reason).toContain("服务端");
  });

  it("跨域同样可用：能不能通由浏览器与后端 CORS 裁决，前端不再代判", () => {
    expect(canTry({ run_mode: "json", transport: "json" }, "https://other.example.com").ok).toBe(
      true,
    );
  });

  it("没有基址时不可用", () => {
    expect(canTry({ run_mode: "json", transport: "json" }, "").ok).toBe(false);
  });
});

describe("鉴权方式", () => {
  it("Anthropic 原生协议认 x-api-key", () => {
    expect(authOf({ path: "/v1/messages" }).header).toBe("x-api-key");
  });

  it("产物代理与文件直链不需要密钥（链接本身即凭证）", () => {
    expect(authOf({ path: "/v1/media/{token}" }).required).toBe(false);
    expect(authOf({ path: "/f/{id}/{name}" }).required).toBe(false);
    expect(authOf({ path: "/v1/images/generations" }).required).toBe(true);
  });
});

describe("路径参数", () => {
  it("按占位符填值，缺值返回 null", () => {
    expect(fillPath("/v1/tasks/{id}", { id: "t1" })).toBe("/v1/tasks/t1");
    expect(fillPath("/v1/tasks/{id}", {})).toBeNull();
    expect(missingPathParam("/v1/tasks/{id}/c/{index}", { id: "t1" })).toBe("index");
  });

  it("值要 encodeURIComponent：带斜杠时不能拼出另一条路由", () => {
    expect(fillPath("/v1/tasks/{id}", { id: "a/b" })).toBe("/v1/tasks/a%2Fb");
    expect(fillPath("/v1/files/{id}", { id: "a b" })).toBe("/v1/files/a%20b");
  });
});

describe("调试表单初值", () => {
  it("— 不是值（契约用它表示「没有默认值」）", () => {
    expect(initialValue(F({ name: "model", default: "—" }), null)).toBe("");
  });

  it("人话默认值不落值（按类型判据，不靠词表）", () => {
    expect(initialValue(F({ name: "max_tokens", type: "integer", default: "模型上限" }), null)).toBe(
      "",
    );
    expect(initialValue(F({ name: "seed", type: "number", default: "随机" }), null)).toBe("");
  });

  it("能落地的布尔/数字默认值照用", () => {
    expect(initialValue(F({ name: "stream", type: "boolean", default: "false" }), null)).toBe(
      "false",
    );
    expect(initialValue(F({ name: "temperature", type: "number", default: "1" }), null)).toBe("1");
  });

  it("example 优先于 default（chat 的 model 就靠它）", () => {
    expect(initialValue(F({ name: "model", default: "—", example: "gpt-4o" }), null)).toBe(
      "gpt-4o",
    );
  });

  it("preset 只对 body 生效：query 与 body 是两个键空间", () => {
    expect(initialValue(F({ name: "page", in: "query" }), { page: 3 })).toBe("");
    expect(initialValue(F({ name: "page" }), { page: 3 })).toBe("3");
  });

  it("preset 优先于 example（一次完整调用的组合更可信）", () => {
    expect(
      initialValue(F({ name: "model", example: "gpt-image-1" }), { model: "gpt-image-2" }),
    ).toBe("gpt-image-2");
  });

  it("复杂参数序列化成 JSON 文本（否则读者要从空框手打 messages）", () => {
    const v = initialValue(F({ name: "messages", type: "array" }), { messages: [{ role: "user" }] });
    expect(JSON.parse(v)).toEqual([{ role: "user" }]);
  });

  it("复杂类型判定只认 array/object/file", () => {
    expect(isComplexType("array")).toBe(true);
    expect(isComplexType("string")).toBe(false);
  });
});

describe("coerce", () => {
  it("数字与布尔按类型还原", () => {
    expect(coerce("3", "integer")).toEqual({ ok: true, value: 3 });
    expect(coerce("1.5", "number")).toEqual({ ok: true, value: 1.5 });
    expect(coerce("true", "boolean")).toEqual({ ok: true, value: true });
  });

  it("非法值报错，不回落成字符串（否则上游只说「参数错误」）", () => {
    expect(coerce("模型上限", "integer").ok).toBe(false);
    expect(coerce("yes", "boolean").ok).toBe(false);
    expect(coerce("", "integer").ok).toBe(false);
  });

  it("未知类型原样透传（网关不校验，报错只会让新类型永远调不通）", () => {
    expect(coerce("auto", "any")).toEqual({ ok: true, value: "auto" });
  });
});

describe("表单行与勾选", () => {
  it("headerRows 排除两种鉴权头与 Content-Type", () => {
    const rows = headerRows([
      F({ name: "Authorization", in: "header" }),
      F({ name: "x-api-key", in: "header" }),
      F({ name: "Content-Type", in: "header" }),
      F({ name: "X-Trace-Id", in: "header" }),
    ]);
    expect(rows.map((f) => f.name)).toEqual(["X-Trace-Id"]);
  });

  it("必填默认勾选：query 与 body 同规则", () => {
    expect(defaultOn(F({ name: "page", in: "query", required: "required" }))).toBe(true);
    expect(defaultOn(F({ name: "page", in: "query" }))).toBe(false);
  });

  it("同名字段按位置分开存（改 query 的 model 不该影响 body 的）", () => {
    const q = F({ name: "model", in: "query" });
    const b = F({ name: "model" });
    const values = { "body:model": "gpt-4o" };
    expect(fieldValue(q, null, values)).toBe("");
    expect(fieldValue(b, null, values)).toBe("gpt-4o");
  });
});

describe("buildTryRequest", () => {
  const doc = (patch: Partial<DocV7Endpoint> = {}) => ({
    method: "POST",
    path: "/v1/x",
    fields: [] as DocV7Field[],
    presets: [] as DocV7Endpoint["presets"],
    run_mode: "json",
    ...patch,
  });
  const draft = (
    patch: Partial<{
      key: string;
      path: Record<string, string>;
      values: Record<string, string>;
      picked: Record<string, boolean>;
      bodyMode: "form" | "json";
      json: string;
    }> = {},
  ) => ({
    key: "sk-t",
    path: {} as Record<string, string>,
    values: {} as Record<string, string>,
    picked: {} as Record<string, boolean>,
    ...patch,
  });

  it("POST 没有字段也发 {} 并带 Content-Type（否则网关回 415）", () => {
    const r = buildTryRequest(doc(), "https://api.example.com", draft());
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBe("{}");
    expect(r.req.headers["Content-Type"]).toBe("application/json");
    expect(r.req.headers.Authorization).toBe("Bearer sk-t");
  });

  it("GET 不带 body 也不带 Content-Type", () => {
    const r = buildTryRequest(doc({ method: "GET" }), "https://api.example.com", draft());
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBeUndefined();
    expect(r.req.headers["Content-Type"]).toBeUndefined();
  });

  it("DELETE 同样不带 body（取消任务这类接口就是这么调的）", () => {
    const r = buildTryRequest(
      doc({ method: "DELETE", path: "/v1/tasks/{id}" }),
      "https://api.example.com",
      draft({ path: { id: "t1" } }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.url).toBe("https://api.example.com/v1/tasks/t1");
    expect(r.req.body).toBeUndefined();
  });

  it("必填 query 默认发出（旧实现默认不勾，永远不发）", () => {
    const r = buildTryRequest(
      doc({ method: "GET", fields: [F({ name: "page", in: "query", required: "required", default: "1" })] }),
      "https://api.example.com",
      draft({ values: { "query:page": "2" } }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.url).toContain("page=2");
  });

  it("缺路径参数时指名，不发请求", () => {
    const r = buildTryRequest(doc({ path: "/v1/tasks/{id}" }), "https://api.example.com", draft());
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.message).toContain("id");
  });

  it("非法数字指名到字段", () => {
    const r = buildTryRequest(
      doc({ fields: [F({ name: "max_tokens", type: "integer", required: "required", default: "—" })] }),
      "https://api.example.com",
      draft({ values: { "body:max_tokens": "模型上限" } }),
    );
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.message).toContain("max_tokens");
  });

  it("header 字段能发出去，且不会出现两个 Authorization", () => {
    const r = buildTryRequest(
      doc({
        fields: [
          F({ name: "X-Trace-Id", in: "header", required: "required" }),
          F({ name: "Authorization", in: "header" }),
        ],
      }),
      "https://api.example.com",
      draft({ values: { "header:X-Trace-Id": "abc" } }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.headers["X-Trace-Id"]).toBe("abc");
    const authKeys = Object.keys(r.req.headers).filter((k) => k.toLowerCase() === "authorization");
    expect(authKeys).toHaveLength(1);
  });

  it("Anthropic 协议用 x-api-key，并带 anthropic-version", () => {
    const r = buildTryRequest(doc({ path: "/v1/messages" }), "https://api.example.com", draft());
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.headers["x-api-key"]).toBe("sk-t");
    expect(r.req.headers["anthropic-version"]).toBe("2023-06-01");
    expect(r.req.headers.Authorization).toBeUndefined();
  });

  it("免鉴权接口不填 Key 也能构造", () => {
    const r = buildTryRequest(
      doc({ method: "GET", path: "/v1/media/{token}" }),
      "https://api.example.com",
      draft({ key: "", path: { token: "tk1" } }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.headers.Authorization).toBeUndefined();
  });

  it("需要密钥的接口没填 Key 时拒绝", () => {
    const r = buildTryRequest(doc(), "https://api.example.com", draft({ key: "" }));
    expect(r.ok).toBe(false);
  });
});

/**
 * JSON 请求体模式。
 *
 * 这是「自定义请求体」的唯一实现：读者写什么就发什么。三条不能被"优化"掉的约束：
 *  1. **原样发送**（不 `JSON.stringify` 重排）：否则"屏幕上的这段"与"发出去的这段"
 *     不再逐字相同，读者无法自证；
 *  2. **不与表单合并**：同时生效就等于两个真相源，必然出现"改了表单却发旧 JSON"；
 *  3. 非法 JSON **停在面板里**，不发半截请求出去。
 */
describe("buildTryRequest · JSON 请求体", () => {
  const doc = (patch: Partial<DocV7Endpoint> = {}) => ({
    method: "POST",
    path: "/v1/chat/completions",
    fields: [] as DocV7Field[],
    presets: [] as DocV7Endpoint["presets"],
    run_mode: "json",
    ...patch,
  });
  const draft = (
    patch: Partial<{
      key: string;
      path: Record<string, string>;
      values: Record<string, string>;
      picked: Record<string, boolean>;
      bodyMode: "form" | "json";
      json: string;
    }> = {},
  ) => ({
    key: "sk-t",
    path: {} as Record<string, string>,
    values: {} as Record<string, string>,
    picked: {} as Record<string, boolean>,
    ...patch,
  });

  const RAW = '{\n  "model": "gpt-4o",\n  "messages": [{"role": "user", "content": "你好"}],\n  "temperature": 0.3\n}';

  it("原样发送：缩进、键顺序、中文都不被改写", () => {
    const r = buildTryRequest(doc(), "https://api.example.com", draft({ bodyMode: "json", json: RAW }));
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBe(RAW);
    expect(r.req.headers["Content-Type"]).toBe("application/json");
  });

  it("不与表单合并：表单勾了 model 也一个字段都不掺进来", () => {
    const r = buildTryRequest(
      doc({ fields: [F({ name: "model", required: "required" })] }),
      "https://api.example.com",
      draft({
        bodyMode: "json",
        json: '{"prompt":"一只猫"}',
        values: { "body:model": "from-form" },
        picked: { "body:model": true },
      }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBe('{"prompt":"一只猫"}');
    expect(r.req.body).not.toContain("from-form");
  });

  it("非法 JSON 停在面板里，不发请求", () => {
    const r = buildTryRequest(doc(), "https://api.example.com", draft({ bodyMode: "json", json: '{"a":' }));
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.message).toContain("JSON");
  });

  it("清空 JSON 退回 {}：POST 必须带体，否则网关回 415", () => {
    const r = buildTryRequest(doc(), "https://api.example.com", draft({ bodyMode: "json", json: "   " }));
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBe("{}");
    expect(r.req.headers["Content-Type"]).toBe("application/json");
  });

  it("GET 带 JSON 也不发体（方法说了算，不是模式说了算）", () => {
    const r = buildTryRequest(
      doc({ method: "GET" }),
      "https://api.example.com",
      draft({ bodyMode: "json", json: '{"a":1}' }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBeUndefined();
    expect(r.req.headers["Content-Type"]).toBeUndefined();
  });

  it("query 与 header 在 JSON 模式下照常生效（只有 body 换来源）", () => {
    const r = buildTryRequest(
      doc({
        method: "POST",
        path: "/v1/x",
        fields: [
          F({ name: "page", in: "query", required: "required" }),
          F({ name: "X-Trace-Id", in: "header", required: "required" }),
        ],
      }),
      "https://api.example.com",
      draft({
        bodyMode: "json",
        json: '{"a":1}',
        values: { "query:page": "2", "header:X-Trace-Id": "trace-1" },
      }),
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.url).toContain("page=2");
    expect(r.req.headers["X-Trace-Id"]).toBe("trace-1");
  });

  it("数组与标量也是合法 JSON（不限于对象）", () => {
    const r = buildTryRequest(doc(), "https://api.example.com", draft({ bodyMode: "json", json: "[1,2,3]" }));
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.req.body).toBe("[1,2,3]");
  });

  it("bodyAllowed 只对带体的方法为真", () => {
    expect(bodyAllowed("POST")).toBe(true);
    expect(bodyAllowed("put")).toBe(true);
    expect(bodyAllowed("PATCH")).toBe(true);
    expect(bodyAllowed("GET")).toBe(false);
    expect(bodyAllowed("DELETE")).toBe(false);
    // 没写方法时按 POST 处理（契约里 POST 是常态）
    expect(bodyAllowed(undefined)).toBe(true);
  });

  it("jsonBodyError 给中文解释，不把浏览器的英文原文丢给读者", () => {
    expect(jsonBodyError("")).toBeNull();
    expect(jsonBodyError('{"a":1}')).toBeNull();
    const msg = jsonBodyError('{"a":}');
    expect(msg).toContain("JSON");
    expect(msg).not.toMatch(/Unexpected token|position/i);
  });

  it("prettyJson 只在合法时排版，写坏的中间态原样留着", () => {
    expect(prettyJson('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettyJson('{"a":')).toBe('{"a":');
    expect(prettyJson("")).toBe("");
    expect(prettyJson(undefined)).toBe("");
  });
});

describe("formBodyOf（切到 JSON 时的预填来源）", () => {
  it("不依赖 Key 与路径参数：只要有字段值就给体", () => {
    const r = formBodyOf(
      { fields: [F({ name: "model", required: "required", example: "gpt-4o" })], presets: [] },
      {},
      {},
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(JSON.parse(r.body)).toEqual({ model: "gpt-4o" });
  });

  it("预设体优先于字段示例（同一个字段有两个来源时按预设走）", () => {
    const r = formBodyOf(
      {
        fields: [F({ name: "model", required: "required", example: "gpt-4o" })],
        presets: [
          {
            key: "p",
            label: "p",
            summary: "",
            fields: ["model"],
            body: { model: "gpt-image-2" },
          },
        ],
      },
      {},
      {},
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(JSON.parse(r.body)).toEqual({ model: "gpt-image-2" });
  });

  it("可选字段不进默认请求：预设里给了值也要读者勾选才发", () => {
    const r = formBodyOf(
      {
        fields: [
          F({ name: "model", required: "required", example: "gpt-image-2" }),
          F({ name: "size", example: "16:9" }),
        ],
        presets: [],
      },
      {},
      {},
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(JSON.parse(r.body)).toEqual({ model: "gpt-image-2" });
  });

  it("字段值是坏 JSON 时指名是哪个字段（与 buildTryRequest 同一份实现）", () => {
    const r = formBodyOf(
      { fields: [F({ name: "messages", type: "array", required: "required" })], presets: [] },
      { "body:messages": "[{" },
      {},
    );
    expect(r.ok).toBe(false);
    if (r.ok) return;
    expect(r.message).toContain("messages");
  });
});

describe("curlOf", () => {
  it("密钥一律脱敏成 $LD_API_KEY", () => {
    const r = buildTryRequest(
      { method: "POST", path: "/v1/x", fields: [], presets: [], run_mode: "json" },
      "https://api.example.com",
      { key: "sk-secret-123", path: {}, values: {}, picked: {} },
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    const curl = curlOf(r.req);
    expect(curl).toContain("$LD_API_KEY");
    expect(curl).not.toContain("sk-secret-123");
    expect(curl).toContain("curl -X POST");
  });

  it("单引号按 shell 规则转义", () => {
    const r = buildTryRequest(
      { method: "POST", path: "/v1/x", fields: [F({ name: "q", required: "required" })], presets: [], run_mode: "json" },
      "https://api.example.com",
      { key: "k", path: {}, values: { "body:q": "it's" }, picked: { "body:q": true } },
    );
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(curlOf(r.req)).toContain(`it'\\''s`);
  });
});

describe("失败提示与超时", () => {
  it("常见状态码各给一句可执行的话，成功不解释", () => {
    const seen = [400, 401, 403, 404, 409, 429, 503].map((s) => statusHint(s));
    expect(seen.every((s) => typeof s === "string" && s.length > 0)).toBe(true);
    expect(new Set(seen).size).toBe(seen.length);
    expect(statusHint(200)).toBeNull();
    expect(statusHint(302)).toBeNull();
  });

  it("流式与二进制放宽到 180 秒（30 秒会掐断已计费的成功请求）", () => {
    expect(timeoutOf({ run_mode: "sse" })).toBe(180_000);
    expect(timeoutOf({ run_mode: "binary" })).toBe(180_000);
    expect(timeoutOf({ run_mode: "json" })).toBe(30_000);
  });
});

describe("页头标签", () => {
  it("支持异步是独立能力标签，只在同步形态上补一枚", () => {
    expect(
      phaseTags({ phase: "同步", phase_code: "sync", run_mode: "json", accepts_async: true }).map(
        (t) => t.text,
      ),
    ).toEqual(["同步", "支持异步"]);
    expect(
      phaseTags({ phase: "异步提交", phase_code: "submit", run_mode: "json", accepts_async: false }).map(
        (t) => t.text,
      ),
    ).toEqual(["异步提交"]);
  });

  it("tone 只认 phase_code：中文文案改了这个分支也不动", () => {
    expect(phaseTags({ phase: "同步", phase_code: "submit", run_mode: "json" })[0].tone).toBe("");
    expect(phaseTags({ phase: "同步", phase_code: "sync", run_mode: "json" })[0].tone).toBe("sync");
  });
});
