import { useEffect, useMemo, useRef, useState } from "react";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Tip } from "@/components/common/Tip";
import { toastErr } from "@/components/common/Toaster";
import { probeRequest, type ProbeResult } from "@/lib/apidocs/probe";
import { useMyKey } from "@/lib/useMyKey";
import type { DocV7Endpoint, DocV7Field } from "@/types/docsv7";

import { CopyBtn } from "./CopyBtn";
import {
  authOf,
  bodyAllowed,
  buildTryRequest,
  canTry,
  curlOf,
  defaultText,
  fieldKey,
  fieldOn,
  fieldValue,
  fillPath,
  formBodyOf,
  headerRows,
  isComplexType,
  jsonBodyError,
  pathParams,
  presetBodyOf,
  prettyJson,
  queryRows,
  statusHint,
  timeoutOf,
  tryRows,
  type BodyMode,
} from "./render";

/**
 * 在线运行面板（右栏第二种模式）。
 *
 * 它发的是**真实请求**：用读者自己的 Key、真实计费、真实写调用日志。
 *
 * ## 结构：只有一条构造路径
 *
 * 请求由 `render.buildTryRequest` 产出，**预览与发送读同一个结果** ——
 * 屏幕上那段 cURL 就是真正发出去的那一份，读者复制走不会与面板行为分叉。
 *
 * ## 三道闸，只拦"真发不出去"的
 *
 * 见 `render.canTry`：契约说 `none`（原因取 `run_hint`）、表单上传、没有基址。
 * **不同源不再拦**：跨域能不能通由浏览器与后端 `LD_ALLOWED_ORIGINS` 裁决，
 * 前端再判一次就是第二套真相源，而且更严（文档域与接口域分开是常态）。
 *
 * ## 密钥
 *
 * 只存在 `sessionStorage`：关标签页即失效。已登录读者可以一键取用自己的密钥
 * （服务端会写一条密钥审计），不经过我们的服务器。
 */
const KEY_STORE = "ld_docs_key";

function readKey(): string {
  try {
    return sessionStorage.getItem(KEY_STORE) ?? "";
  } catch {
    return "";
  }
}

function writeKey(v: string): void {
  try {
    if (v) sessionStorage.setItem(KEY_STORE, v);
    else sessionStorage.removeItem(KEY_STORE);
  } catch {
    // 隐私模式下 sessionStorage 可能抛：Key 只留在内存里，不影响本次发送。
  }
}

/** 输入框占位：优先用契约里的示例值，其次是不像文案的默认值。 */
function hintOf(f: DocV7Field): string {
  if (typeof f.example === "string" && f.example.trim() !== "") return f.example;
  const d = (f.default ?? "").trim();
  // `—` 是"没有默认值"，中文是说明文字：两者都不是可填的值，别放进占位符。
  if (d !== "" && d !== "—" && !/[\u4e00-\u9fa5]/.test(d)) return d;
  return "";
}

export function TryPanel({
  doc,
  baseUrl,
  onBack,
}: {
  doc: DocV7Endpoint;
  baseUrl: string;
  onBack: () => void;
}) {
  const gate = useMemo(() => canTry(doc, baseUrl), [doc, baseUrl]);
  const auth = authOf(doc);
  const params = pathParams(doc.path);
  const rows = useMemo(() => tryRows(doc.fields), [doc.fields]);
  const qRows = useMemo(() => queryRows(doc.fields), [doc.fields]);
  const hRows = useMemo(() => headerRows(doc.fields), [doc.fields]);
  const presetBody = useMemo(() => presetBodyOf(doc), [doc]);

  const [key, setKey] = useState(readKey);
  const [pathValues, setPathValues] = useState<Record<string, string>>({});
  const [values, setValues] = useState<Record<string, string>>({});
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  // 请求体的来源（见 render.TryDraft）：同一时刻只有一个真相源。
  const [bodyMode, setBodyMode] = useState<BodyMode>("form");
  const [jsonText, setJsonText] = useState("");
  /** 读者在 JSON 里动过手就不再用表单覆盖它。 */
  const [jsonTouched, setJsonTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<ProbeResult | null>(null);
  const [netErr, setNetErr] = useState("");

  // 在途请求必须 abort：否则 3 分钟（流式）内会把上一个接口的响应写到一个
  // 已经不存在的组件上。blobUrl 也要 revoke，否则每试一次就留一份图在内存里。
  const abortRef = useRef<AbortController | null>(null);
  const blobRef = useRef<string | null>(null);
  useEffect(
    () => () => {
      abortRef.current?.abort();
      if (blobRef.current) URL.revokeObjectURL(blobRef.current);
    },
    [],
  );

  /** 已登录才能一键取用（未登录发 /portal/keys 必然 401，会触发全站会话失效处理）。 */
  const { keys: myKeys, current, setPickId, take: useMine, taking } = useMyKey((v) => {
    setKey(v);
    writeKey(v);
  });

  const draft = {
    key,
    path: pathValues,
    values,
    picked,
    bodyMode,
    json: jsonText,
  };
  const built = useMemo(
    () => buildTryRequest(doc, baseUrl, draft),
    // draft 的每个字段都是状态本身，逐项列依赖比传对象更准（对象每次渲染都是新的）
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [doc, baseUrl, key, pathValues, values, picked, bodyMode, jsonText],
  );

  /** JSON 模式自检：只用于写坏时给出即时提示，真正拦截在 `buildTryRequest` 里。 */
  const jsonErr = bodyMode === "json" ? jsonBodyError(jsonText) : null;

  /**
   * 切到 JSON：**按当前表单拼一份**填进去 —— 这样"先填几个字段，再改 JSON"
   * 是连续的动作，而不是面对一块空白从零手打。
   *
   * 用 `formBodyOf` 而不是整条 `buildTryRequest`：后者会先校验 Key 与路径参数，
   * 于是"还没填 Key 就点 JSON"会看到 `{}`，看起来像表单的值被吞了。
   *
   * 只在读者没在 JSON 里动过手时重算（`jsonTouched`）：否则刚切回表单看一眼前，
   * 刚写的 JSON 就会被悄悄覆盖。
   */
  const enterJson = () => {
    if (!jsonTouched) setJsonText(seedFromForm());
    setBodyMode("json");
  };

  /** 无条件按表单重算一份（读者在 JSON 里改乱了想回到表单值时的出口）。 */
  const regenFromForm = () => {
    setJsonText(seedFromForm());
    setJsonTouched(false);
  };

  function seedFromForm(): string {
    const fb = formBodyOf(doc, values, picked);
    return fb.ok ? prettyJson(fb.body) : "";
  }

  const send = async () => {
    if (busy) return;
    if (!built.ok) {
      toastErr(built.message);
      return;
    }
    if ((doc.method || "").toUpperCase() === "DELETE") {
      const ok = await confirmDialog({
        title: "确认发送删除请求？",
        body: "这会真的删除该资源，不可恢复。",
        okText: "仍然发送",
        okVariant: "danger",
      });
      if (!ok) return;
    }

    writeKey(key.trim());
    const req = built.req;
    const ctrl = new AbortController();
    abortRef.current?.abort();
    abortRef.current = ctrl;
    const limit = timeoutOf(doc);
    const timer = window.setTimeout(() => ctrl.abort(), limit);

    setBusy(true);
    setNetErr("");
    setResult(null);
    try {
      const got = await probeRequest(req, key.trim(), ctrl.signal, {
        authHeader: auth.header === "x-api-key" ? "x-api-key" : "authorization",
        // 二进制端点（下载文件 / 任务产物 / 产物代理）要能看图听音，不塞进 <pre>。
        wantBinary: doc.run_mode === "binary",
      });
      if (blobRef.current) URL.revokeObjectURL(blobRef.current);
      blobRef.current = got.blobUrl ?? null;
      setResult(got);
    } catch (e) {
      const aborted = (e as Error)?.name === "AbortError";
      setNetErr(
        aborted
          ? `请求超时（${limit / 1000} 秒），已中断。`
          : `请求未发出：${(e as Error)?.message ?? "网络错误"}`,
      );
    } finally {
      window.clearTimeout(timer);
      setBusy(false);
    }
  };

  // 完整地址（含基址）：跨域部署下读者需要一眼看到"这一枪打向哪里"，
  // 只显示 path 会让人以为发给了本站。
  const urlPreview = `${baseUrl.replace(/\/+$/, "")}${fillPath(doc.path, pathValues) ?? doc.path}`;

  return (
    <>
      <div className="d-rail-h">
        <b>在线运行</b>
        <span className="sp" />
        <button type="button" className="d-iconbtn" onClick={onBack}>
          返回文档
        </button>
      </div>

      <div className="d-try-url">
        <span className={`d-mth ${doc.method}`}>{doc.method}</span>
        <code>{urlPreview}</code>
        {gate.ok ? (
          <button type="button" className="d-tbtn" onClick={() => void send()} disabled={busy}>
            {busy ? "发送中" : "发送"}
          </button>
        ) : null}
      </div>

      {!gate.ok ? <p className="d-tgate">{gate.reason}</p> : null}

      {gate.ok ? (
        <>
          <div className="d-tlabel">
            接入密钥
            {auth.required ? null : <span className="sub">此接口不需要</span>}
            <span className="sp" />
            <span className="sub">{auth.scheme}</span>
            <Tip text={auth.hint} />
          </div>
          <div className="d-tkeyrow">
            <input
              className="d-tinput"
              type="password"
              value={key}
              placeholder={auth.required ? "粘贴你的 API Key" : "此接口无需密钥"}
              autoComplete="off"
              onChange={(e) => setKey(e.target.value)}
            />
            {key ? (
              <button
                type="button"
                className="d-iconbtn"
                onClick={() => {
                  setKey("");
                  writeKey("");
                }}
              >
                清除
              </button>
            ) : null}
          </div>

          {/* 已登录：下拉选一把自己的密钥，一键填入。未登录时列表必为空、请求也不会发出
              （`useMyKey` 里 `enabled` 判断），所以这里只看列表有没有内容。 */}
          {myKeys.length > 0 ? (
            <div className="d-tkeyrow">
              <select
                className="d-tsel"
                value={current ?? ""}
                onChange={(e) => setPickId(Number(e.target.value))}
              >
                {myKeys.map((k) => (
                  <option key={k.id} value={k.id}>
                    {k.name}（{k.key_prefix}…）
                  </option>
                ))}
              </select>
              <button
                type="button"
                className="d-iconbtn"
                onClick={() => void useMine()}
                disabled={taking}
              >
                {taking ? "取用中" : "使用我的密钥"}
              </button>
              <Tip text="取用会写入一条密钥审计。密钥只填进本页输入框，不上传到服务器。" />
            </div>
          ) : null}

          {params.length > 0 ? (
            <>
              <div className="d-tlabel">路径参数</div>
              {params.map((p) => (
                <label className="d-tcheck" key={p}>
                  <span className="nm">{p}</span>
                  <input
                    className="val"
                    type="text"
                    value={pathValues[p] ?? ""}
                    placeholder={p === "index" ? "0" : "必填"}
                    onChange={(e) => setPathValues((cur) => ({ ...cur, [p]: e.target.value }))}
                  />
                </label>
              ))}
            </>
          ) : null}

          {qRows.length > 0 ? (
            <>
              <div className="d-tlabel">查询参数</div>
              {qRows.map((f) => (
                <label className="d-tcheck" key={fieldKey(f)}>
                  <input
                    type="checkbox"
                    checked={fieldOn(f, picked)}
                    onChange={(e) =>
                      setPicked((cur) => ({ ...cur, [fieldKey(f)]: e.target.checked }))
                    }
                  />
                  <span className="nm">{f.name}</span>
                  <input
                    className="val"
                    type="text"
                    value={fieldValue(f, null, values)}
                    placeholder={hintOf(f)}
                    onChange={(e) =>
                      setValues((cur) => ({ ...cur, [fieldKey(f)]: e.target.value }))
                    }
                  />
                </label>
              ))}
            </>
          ) : null}

          {hRows.length > 0 ? (
            <>
              <div className="d-tlabel">请求头</div>
              {hRows.map((f) => (
                <label className="d-tcheck" key={fieldKey(f)}>
                  <input
                    type="checkbox"
                    checked={fieldOn(f, picked)}
                    onChange={(e) =>
                      setPicked((cur) => ({ ...cur, [fieldKey(f)]: e.target.checked }))
                    }
                  />
                  <span className="nm">{f.name}</span>
                  <input
                    className="val"
                    type="text"
                    value={fieldValue(f, null, values)}
                    placeholder={hintOf(f)}
                    onChange={(e) =>
                      setValues((cur) => ({ ...cur, [fieldKey(f)]: e.target.value }))
                    }
                  />
                </label>
              ))}
            </>
          ) : null}

          {bodyAllowed(doc.method) ? (
            <>
              <div className="d-tlabel">
                请求体
                {/* 提示紧跟标题：行末已被两个胶囊占满，问号会被挤出右边缘 */}
                <Tip
                  text={
                    bodyMode === "json"
                      ? "原样发送这段 JSON（不按字段重排）。切回表单会丢弃这里的改动。"
                      : "必填项已勾选并填入示例值；留空的项不会发送。"
                  }
                />
                <span className="sp" />
                <SegmentedChips
                  className="d-tseg"
                  items={[
                    { key: "form", label: "表单" },
                    { key: "json", label: "JSON" },
                  ]}
                  value={bodyMode}
                  onChange={(k) => (k === "json" ? enterJson() : setBodyMode("form"))}
                />
              </div>

              {bodyMode === "json" ? (
                <>
                  <textarea
                    className={jsonErr ? "d-tarea d-tarea--bad" : "d-tarea"}
                    rows={12}
                    spellCheck={false}
                    value={jsonText}
                    placeholder='{"model": "...", "messages": [{"role": "user", "content": "你好"}]}'
                    onChange={(e) => {
                      setJsonTouched(true);
                      setJsonText(e.target.value);
                    }}
                  />
                  {jsonErr ? <p className="d-terr">{jsonErr}</p> : null}
                  <div className="d-tkeyrow">
                    <span className="sp" />
                    <button
                      type="button"
                      className="d-iconbtn"
                      onClick={() => setJsonText(prettyJson(jsonText))}
                      disabled={jsonErr !== null || jsonText.trim() === ""}
                    >
                      格式化
                    </button>
                    <button type="button" className="d-iconbtn" onClick={regenFromForm}>
                      由表单生成
                    </button>
                  </div>
                </>
              ) : (
                rows.map((f) => {
                  const checked = fieldOn(f, picked);
                  if (isComplexType(f.type)) {
                    return (
                      <div className="d-tfield" key={fieldKey(f)}>
                        <label className="d-tfield-h">
                          <input
                            type="checkbox"
                            checked={checked}
                            onChange={(e) =>
                              setPicked((cur) => ({ ...cur, [fieldKey(f)]: e.target.checked }))
                            }
                          />
                          <span className="nm">{f.name}</span>
                          <span className="sub">{f.type}，JSON</span>
                        </label>
                        <textarea
                          className="d-tarea"
                          rows={4}
                          spellCheck={false}
                          value={fieldValue(f, presetBody, values)}
                          placeholder='{"key": "value"}'
                          onChange={(e) =>
                            setValues((cur) => ({ ...cur, [fieldKey(f)]: e.target.value }))
                          }
                        />
                      </div>
                    );
                  }
                  return (
                    <label className="d-tcheck" key={fieldKey(f)}>
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={(e) =>
                          setPicked((cur) => ({ ...cur, [fieldKey(f)]: e.target.checked }))
                        }
                      />
                      <span className="nm">{f.name}</span>
                      <input
                        className="val"
                        type="text"
                        value={fieldValue(f, presetBody, values)}
                        placeholder={hintOf(f) || defaultText(f) || ""}
                        onChange={(e) =>
                          setValues((cur) => ({ ...cur, [fieldKey(f)]: e.target.value }))
                        }
                      />
                    </label>
                  );
                })
              )}
            </>
          ) : null}

          {built.ok ? (
            <details className="d-tcurl">
              <summary>请求预览</summary>
              <div className="d-tcurl-h">
                <span className="sub">Key 已脱敏为 $LD_API_KEY</span>
                <span className="sp" />
                <CopyBtn text={curlOf(built.req)} label="复制 cURL" />
              </div>
              <pre>{curlOf(built.req)}</pre>
            </details>
          ) : null}

          <div className="d-tres">
            {netErr ? (
              <div className="d-tres-b">
                <span className="ico">!</span>
                <span>
                  {netErr}
                  <br />
                  常见原因：本站与接口不同域且未放行来源（后端配置 <code>LD_ALLOWED_ORIGINS</code>），
                  或网络不可达。
                </span>
              </div>
            ) : result ? (
              <>
                <div className="d-tres-h">
                  <span
                    className={`d-mth ${result.ok ? "GET" : "DELETE"}`}
                  >
                    {result.status}
                  </span>
                  <span>{result.ok ? "成功" : "失败"}</span>
                  <span className="sp" />
                  <span className="ms">{result.ms} ms</span>
                </div>
                {statusHint(result.status) ? (
                  <div className="d-tres-note">{statusHint(result.status)}</div>
                ) : null}
                {result.blobUrl ? (
                  <div className="d-tres-bin">
                    {result.contentType.startsWith("image/") ? (
                      <img className="d-tres-img" src={result.blobUrl} alt="响应预览" />
                    ) : null}
                    <a href={result.blobUrl} download={`result.${result.contentType.split("/")[1] || "bin"}`}>
                      下载结果
                    </a>
                  </div>
                ) : (
                  <pre className="d-tres-body">{result.body || "（空响应体）"}</pre>
                )}
              </>
            ) : (
              <div className="d-tres-b">
                <span className="ico">⌁</span>
                <span>填好参数后点「发送」查看真实返回</span>
              </div>
            )}
          </div>

          <p className="d-hint">
            请求直接发往 <code>{baseUrl}</code>，会计费并写入调用日志；Key 只保存在本标签页。
          </p>
        </>
      ) : null}
    </>
  );
}
