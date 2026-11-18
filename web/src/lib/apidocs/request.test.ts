import { describe, expect, it } from "vitest";
import type { ApiDocEndpoint, ApiDocParam } from "@/types";
import {
  absoluteURL,
  buildRequest,
  hasBody,
  missingRequired,
  redactHeaders,
  splitArray,
  toCurl,
  toFetchSnippet,
  userApiPath,
} from "./request";

/** 造一条最小可用契约；只覆盖被测字段，其余给中性默认值。 */
function makeEp(patch: Partial<ApiDocEndpoint> = {}): ApiDocEndpoint {
  return {
    key: "keys.list",
    group: "密钥",
    title: "密钥列表",
    method: "GET",
    path: "/api/user/keys",
    auth: "reader",
    scope: "usage:read",
    scope_enforced: false,
    media: "json",
    idempotency: "",
    notes: "",
    params: [],
    returns: [],
    errors: [],
    no_data: false,
    request_example: {},
    response_example: {},
    try: { callable: true, hint: "", risk: "safe" },
    ...patch,
  };
}

/** 造一个参数。 */
function p(patch: Partial<ApiDocParam> & Pick<ApiDocParam, "name">): ApiDocParam {
  return {
    in: "query",
    type: "string",
    required: "optional",
    required_note: "",
    desc: "说明",
    note: "",
    default: "",
    enum: [],
    example: "",
    constraints: [],
    ...patch,
  };
}

describe("userApiPath", () => {
  it("去掉契约里的 /api 前缀（基址已含它，直接拼会变成 /api/api/...）", () => {
    expect(userApiPath("/api/user/keys/:id")).toBe("/user/keys/:id");
    expect(userApiPath("/api/user")).toBe("/user");
  });

  it("只去掉开头那一个 /api，不动路径中间的 api 段", () => {
    expect(userApiPath("/api/user/api/keys")).toBe("/user/api/keys");
  });

  it("不碰不像前缀的写法", () => {
    expect(userApiPath("/apix/user")).toBe("/apix/user");
    expect(userApiPath("/v1/chat")).toBe("/v1/chat");
  });
});

describe("buildRequest 的路径参数", () => {
  const del = makeEp({
    method: "DELETE",
    path: "/api/user/keys/:id",
    auth: "writer",
    params: [p({ name: "id", in: "path", required: "required" })],
  });

  it("替换成实际值并做 URL 编码", () => {
    const r = buildRequest(del, { id: "a b/c" });
    expect(r.url).toBe("/api/user/keys/a%20b%2Fc");
    expect(r.missing).toEqual([]);
  });

  it("没填时保留 :id 占位符并计入 missing，不抛错、不拼出空段", () => {
    // 抛错会让"还没输完"变成崩溃（URL 预览是逐字调用的）；
    // 拼成空串会得到 `/keys/` —— 看起来正常，实际打到另一条路由。
    const r = buildRequest(del, {});
    expect(r.url).toBe("/api/user/keys/:id");
    expect(r.missing).toEqual(["id"]);
  });
});

describe("buildRequest 的 query", () => {
  const list = makeEp({
    params: [
      p({ name: "page", in: "query", type: "integer" }),
      p({ name: "page_size", in: "query", type: "integer" }),
      p({ name: "sort", in: "query" }),
      p({ name: "tag", in: "query", type: "array" }),
      p({ name: "only_active", in: "query", type: "boolean" }),
      // 非 query 参数绝不能出现在 URL 上
      p({ name: "body_only", in: "body" }),
    ],
  });

  it("只拼 in=query 的参数，且空值不拼", () => {
    const r = buildRequest(list, { page: "2", body_only: "x", sort: "" });
    expect(r.url).toBe("/api/user/keys?page=2");
  });

  it("integer 按数字形态拼（不做 1.5 的静默取整）", () => {
    expect(buildRequest(list, { page: "3" }).url).toContain("page=3");
    expect(buildRequest(list, { page: "3.5" }).url).toContain("page=3.5");
  });

  it("数组型 query 用重复键，不用逗号分隔", () => {
    // `?tag=a,b` 在多数服务端会被当成一个值，这是最容易踩的坑。
    const r = buildRequest(list, { tag: "a\nb, c" });
    expect(r.url).toBe("/api/user/keys?tag=a&tag=b&tag=c");
  });

  it("boolean 只认 true/false", () => {
    expect(buildRequest(list, { only_active: "true" }).url).toContain("only_active=true");
    expect(buildRequest(list, { only_active: "1" }).url).toContain("only_active=false");
  });

  it("参数顺序按契约声明顺序（curl 片段才能 diff）", () => {
    const r = buildRequest(list, { page_size: "20", page: "1" });
    expect(r.url).toBe("/api/user/keys?page=1&page_size=20");
  });
});

describe("buildRequest 的 body", () => {
  const create = makeEp({
    method: "POST",
    path: "/api/user/keys",
    auth: "writer",
    params: [
      p({ name: "name", in: "body", required: "required" }),
      p({ name: "quota", in: "body", type: "integer" }),
      p({ name: "models_allowed", in: "body", type: "array" }),
      p({ name: "enabled", in: "body", type: "boolean" }),
    ],
  });

  it("按 type 归一后再 JSON 化（integer 是数字不是字符串）", () => {
    const r = buildRequest(create, { name: "ci", quota: "100", enabled: "true" });
    expect(JSON.parse(r.body!)).toEqual({ name: "ci", quota: 100, enabled: true });
  });

  it("数组字段是真正的数组", () => {
    const r = buildRequest(create, { name: "ci", models_allowed: "gpt-4o\no3" });
    expect(JSON.parse(r.body!).models_allowed).toEqual(["gpt-4o", "o3"]);
  });

  it("没填的 body 字段不进 JSON（不传与传 null 是两回事）", () => {
    const r = buildRequest(create, { name: "ci" });
    expect(JSON.parse(r.body!)).toEqual({ name: "ci" });
  });

  it("没有 body 参数的端点不产生 body，也不带 Content-Type", () => {
    const r = buildRequest(makeEp(), {});
    expect(r.body).toBeUndefined();
    expect(r.headers["Content-Type"]).toBeUndefined();
    expect(hasBody(makeEp())).toBe(false);
  });

  it("有 body 参数的端点即使全空也发 {}（缺 body 与参数错是两种 400）", () => {
    const r = buildRequest(create, {});
    expect(r.body).toBe("{}");
    expect(r.headers["Content-Type"]).toBe("application/json");
  });
});

/**
 * 手写 JSON 体（试运行面板的 JSON 模式）。
 *
 * 与文档站同一套规则：**原样发送、不与表单合并**。两条都不能"优化"成合并或重排 ——
 * 一旦合并，界面上就无法解释"为什么发出去的不是我看到的这份"。
 */
describe("buildRequest 的 rawBody", () => {
  const create = makeEp({
    method: "POST",
    path: "/api/user/keys",
    auth: "writer",
    params: [
      p({ name: "name", in: "body", required: "required" }),
      p({ name: "quota", in: "body", type: "integer" }),
    ],
  });

  it("原样当请求体，一个字段都不从 values 补", () => {
    const raw = '{\n  "name": "自定义",\n  "quota": 7\n}';
    const r = buildRequest(create, { name: "from-form", quota: "100" }, raw);
    expect(r.body).toBe(raw);
    expect(r.headers["Content-Type"]).toBe("application/json");
  });

  it("空串按 {} 处理（有体端点不能变成「不传」）", () => {
    const r = buildRequest(create, {}, "   ");
    expect(r.body).toBe("{}");
  });

  it("没有 body 参数的端点也可以手写（契约没列全时照样能试）", () => {
    const r = buildRequest(makeEp(), {}, '{"x":1}');
    expect(r.body).toBe('{"x":1}');
    expect(r.headers["Content-Type"]).toBe("application/json");
  });

  it("path/query 仍来自 values：只有体换来源", () => {
    const ep = makeEp({
      method: "POST",
      path: "/api/user/keys/:id",
      params: [p({ name: "id", in: "path", required: "required" }), p({ name: "page", in: "query" })],
    });
    const r = buildRequest(ep, { id: "k1", page: "2" }, "{}");
    expect(r.url).toContain("/user/keys/k1");
    expect(r.url).toContain("page=2");
  });

  it("body 必填不再计入 missing（否则界面隐藏了输入框、发送永远禁用）", () => {
    const r = buildRequest(create, {}, '{"name":"我在 JSON 里写了"}');
    expect(r.missing).toEqual([]);
  });

  it("但路径/查询的必填仍然要拦", () => {
    const ep = makeEp({
      method: "POST",
      path: "/api/user/keys/:id",
      params: [p({ name: "id", in: "path", required: "required" }), p({ name: "q", in: "query", required: "required" })],
    });
    expect(buildRequest(ep, {}, "{}").missing).toEqual(["id", "q"]);
  });
});

describe("missingRequired", () => {
  const ep = makeEp({
    params: [
      p({ name: "a", in: "query", required: "required" }),
      p({ name: "b", in: "body", required: "optional" }),
      p({ name: "c", in: "body", required: "conditional", required_note: "二选一" }),
    ],
  });

  it("只报 required 且没填的", () => {
    expect(missingRequired(ep, {})).toEqual(["a"]);
    expect(missingRequired(ep, { a: "1" })).toEqual([]);
  });

  it("空白串算没填", () => {
    expect(missingRequired(ep, { a: "   " })).toEqual(["a"]);
  });

  it("conditional 不阻止发送（二选一的判据只能写在契约里给人看）", () => {
    // 硬编码"至少填一个"会在契约变化时静默错误。
    expect(missingRequired(ep, { a: "1" })).toEqual([]);
  });
});

describe("鉴权头", () => {
  it("免鉴权端点不提 Bearer", () => {
    const r = buildRequest(makeEp({ auth: "open" }), {});
    expect(r.headers.Authorization).toBeUndefined();
  });

  it("其余级别都带占位 Bearer（不填真密钥）", () => {
    for (const auth of ["reader", "writer", "session"]) {
      const r = buildRequest(makeEp({ auth }), {});
      expect(r.headers.Authorization).toBe("Bearer <你的密钥>");
    }
  });
});

describe("redactHeaders", () => {
  it("遮蔽 Authorization 与 X-Api-Key，不动业务头", () => {
    const out = redactHeaders({
      Authorization: "Bearer sk-real-secret",
      "X-Api-Key": "sk-real-secret",
      Accept: "application/json",
    });
    expect(out.Authorization).toBe("Bearer <你的密钥>");
    expect(out["X-Api-Key"]).toBe("Bearer <你的密钥>");
    expect(out.Accept).toBe("application/json");
    expect(JSON.stringify(out)).not.toContain("sk-real-secret");
  });
});

describe("片段生成", () => {
  const req = {
    method: "POST",
    url: "/api/user/keys?page=1",
    headers: { Accept: "application/json", "Content-Type": "application/json", Authorization: "Bearer <你的密钥>" },
    body: '{"name":"ci"}',
    missing: [],
  };

  it("absoluteURL 补 origin，已是绝对地址时不动", () => {
    expect(absoluteURL("/api/x", "http://127.0.0.1:5178")).toBe("http://127.0.0.1:5178/api/x");
    expect(absoluteURL("/api/x", "http://h/")).toBe("http://h/api/x");
    expect(absoluteURL("https://api.example.com/api/x", "http://h")).toBe("https://api.example.com/api/x");
  });

  it("curl 含方法、绝对地址、全部头与 body", () => {
    const s = toCurl(req, "http://127.0.0.1:5178");
    expect(s).toContain("curl -X POST 'http://127.0.0.1:5178/api/user/keys?page=1'");
    expect(s).toContain("-H 'Accept: application/json'");
    expect(s).toContain("-H 'Content-Type: application/json'");
    expect(s).toContain(`-d '{"name":"ci"}'`);
  });

  it("curl 的单引号被安全转义（body 里带 ' 不会破坏命令）", () => {
    const s = toCurl({ ...req, body: `{"name":"o'brien"}` }, "http://h");
    expect(s).toContain(`-d '{"name":"o'\\''brien"}'`);
  });

  it("fetch 片段与 curl 指向同一个地址、同一份头", () => {
    const s = toFetchSnippet(req, "http://127.0.0.1:5178");
    expect(s).toContain('fetch("http://127.0.0.1:5178/api/user/keys?page=1"');
    expect(s).toContain('method: "POST"');
    expect(s).toContain('"Accept": "application/json"');
    expect(s).toContain('body: "{\\"name\\":\\"ci\\"}"');
  });

  it("无 body 时 curl 不出 -d、fetch 不出 body 行", () => {
    const noBody = { ...req, headers: { Accept: "application/json" }, body: undefined };
    expect(toCurl(noBody, "http://h")).not.toContain("-d ");
    expect(toFetchSnippet(noBody, "http://h")).not.toContain("body:");
  });
});

describe("splitArray", () => {
  it("换行与逗号都当分隔符，去空白丢空项", () => {
    expect(splitArray("a\nb")).toEqual(["a", "b"]);
    expect(splitArray("a, b ,, c")).toEqual(["a", "b", "c"]);
    expect(splitArray("  ")).toEqual([]);
  });
});
