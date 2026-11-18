import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Textarea } from "@/components/common/Textarea";
import { Tip } from "@/components/common/Tip";
import { toastOk, toastErr } from "@/components/common/Toaster";
import { copyText } from "@/lib/clipboard";
import { jsonBodyError, prettyJson } from "@/lib/jsonBody";
import { probeRequest, type ProbeResult } from "@/lib/apidocs/probe";
import {
  buildRequest,
  hasBody,
  toCurl,
  toFetchSnippet,
  type ParamValues,
} from "@/lib/apidocs/request";
import { useApiDocCatalogQuery } from "@/lib/api";
import { useMyKey } from "@/lib/useMyKey";
import type { ApiDocEndpoint } from "@/types";

import { FieldInputs } from "./FieldInputs";
import { ResponseView } from "./ResponseView";

/**
 * 试运行面板。
 *
 * 五个安全/正确性取舍，每一条都对应一种真实事故：
 *
 *  1. **默认收起**。展开即出现"发送"按钮，误点就是一次生产写操作。
 *  2. **参数不预填**。契约里的示例值（金额、手机号）看起来无害，但它会真的
 *     打进生产：填好示例 + 一次误点 = 一笔真实订单。
 *  3. **必填缺失时禁用发送**，并在按钮旁写明缺哪些。让服务端返回 400 会浪费
 *     一次往返，而且接入方会以为是参数名拼错了。
 *  4. **高风险端点二次确认**，确认框里写具体后果（来自契约 notes，不编）。
 *  5. **不走 apiClient**（见 `lib/apidocs/probe.ts` 顶部注释）：
 *     401 在这里的含义是"密钥不对"，用会话过期的方式处理会把人踢出后台。
 */
export function TryItPanel({ e }: { e: ApiDocEndpoint }) {
  const [open, setOpen] = useState(false);
  const [values, setValues] = useState<ParamValues>({});
  const [apiKey, setApiKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<ProbeResult | null>(null);
  const [failure, setFailure] = useState("");
  // 请求体来源：表单（按参数拼）或 JSON（原样发送）。同一时刻只有一个真相源。
  const [bodyMode, setBodyMode] = useState<"form" | "json">("form");
  const [jsonText, setJsonText] = useState("");
  const [jsonTouched, setJsonTouched] = useState(false);

  /** 表单那份永远算：JSON 模式下的"由表单生成"要拿它，且与文档站规则一致。 */
  const formReq = useMemo(() => buildRequest(e, values), [e, values]);
  const jsonErr = bodyMode === "json" ? jsonBodyError(jsonText) : null;
  const req = useMemo(
    () => (bodyMode === "json" && !jsonErr ? buildRequest(e, values, jsonText) : formReq),
    [bodyMode, jsonErr, jsonText, values, e, formReq],
  );
  const origin = typeof window === "undefined" ? "" : window.location.origin;
  const needsKey = e.auth !== "open";
  const canSend = !busy && req.missing.length === 0 && !jsonErr;

  /**
   * 切到 JSON：按当前表单拼一份填进去（不依赖密钥/路径参数，那些与体无关）。
   * 读者动过手（`jsonTouched`）就不再覆盖。
   */
  const enterJson = () => {
    if (!jsonTouched) setJsonText(prettyJson(formReq.body ?? ""));
    setBodyMode("json");
  };

  /**
   * 请求超时：没有它，"请求中…" 会永远停在那里 —— 接入方以为是自己网络的问题。
   * 30 秒是"人还在等"的上限；更久的应该去后台看调用记录。
   */
  const ctrlRef = useRef<AbortController | null>(null);
  useEffect(() => () => ctrlRef.current?.abort(), []);

  /** 已登录就用管理员自己的密钥：接入方那把不在手上，手填等于把功能锁死。 */
  const {
    keys: mine,
    current,
    setPickId,
    take: useMine,
    taking,
  } = useMyKey(setApiKey, needsKey);

  /** 契约里写了示例值的参数一键填满（高风险端点仍会在发送前二次确认）。 */
  const fillExamples = () => {
    const next: ParamValues = { ...values };
    let n = 0;
    for (const p of e.params) {
      if (typeof p.example === "string" || typeof p.example === "number") {
        next[p.name] = String(p.example);
        n++;
      }
    }
    if (n === 0) {
      toastErr("这条接口没有示例值");
      return;
    }
    setValues(next);
  };

  const catalog = useApiDocCatalogQuery();
  const swagger = catalog.data?.links.swagger_ui;

  const onChange = useCallback((name: string, value: string) => {
    setValues((prev) => ({ ...prev, [name]: value }));
  }, []);

  const reset = () => {
    setValues({});
    setResult(null);
    setFailure("");
  };

  const send = async () => {
    if (e.try.risk === "confirm") {
      const ok = await confirmDialog({
        title: `确认试运行「${e.title}」`,
        body: (
          <>
            <p>
              这次调用会真的发到生产环境（{e.method} {e.path}），可能改变真实数据。
            </p>
            {e.notes ? <p>{e.notes}</p> : null}
          </>
        ),
        okText: "仍然发送",
        okVariant: "danger",
      });
      if (!ok) return;
    }
    setBusy(true);
    setFailure("");
    setResult(null);
    const ctrl = new AbortController();
    ctrlRef.current?.abort();
    ctrlRef.current = ctrl;
    const timer = window.setTimeout(() => ctrl.abort(), 30_000);
    try {
      setResult(await probeRequest(req, needsKey ? apiKey : "", ctrl.signal));
    } catch (err) {
      // 只有网络层失败会到这里：CORS、断网、超时。HTTP 4xx/5xx 是结果，走 result 分支。
      const aborted = (err as Error)?.name === "AbortError";
      setFailure(
        aborted
          ? "已中断（30 秒无响应）。请求可能仍在服务端执行，可到调用记录里核对。"
          : err instanceof Error
            ? err.message
            : "网络请求失败",
      );
    } finally {
      window.clearTimeout(timer);
      setBusy(false);
    }
  };

  // 不可试运行：说清「为什么」+「去哪做」。只写"本条不支持"等于把人堵在门口。
  if (!e.try.callable) {
    return (
      <section className="adx-sec" data-testid="apidoc-try-blocked">
        <div className="h">
          <b>试运行</b>
          <span>本条不支持</span>
        </div>
        <p className="muted">{e.try.hint || "这条接口需要浏览器会话，无法在这里代发。"}</p>
        <p className="muted">
          {swagger ? (
            <a href={swagger} target="_blank" rel="noreferrer">
              在 Swagger UI 里查看完整请求结构
            </a>
          ) : null}
          {swagger ? "；" : ""}
          需要网页会话的操作请在门户对应页面完成。
        </p>
      </section>
    );
  }

  return (
    <section className="adx-sec" data-testid="apidoc-try">
      <div className="h">
        <b>试运行</b>
        <span>会真的发到生产环境</span>
        <button type="button" className="btn adx-openbtn" onClick={() => setOpen((v) => !v)}>
          {open ? "收起" : "展开"}
        </button>
      </div>

      {open ? (
        <>
          <div className="form-grid">
            {needsKey ? (
              <Field label="接入密钥" span>
                <Input
                  type="password"
                  value={apiKey}
                  autoComplete="off"
                  placeholder="粘贴一把该应用的密钥，本页不会保存"
                  onChange={(ev) => setApiKey(ev.target.value)}
                />
              </Field>
            ) : null}
            <FieldInputs
              // JSON 模式下 body 参数不参与（体由那段 JSON 唯一决定），留着会让人以为两处都会发
              params={bodyMode === "json" ? e.params.filter((p) => p.in !== "body" && p.in !== "form") : e.params}
              values={values}
              onChange={onChange}
            />
          </div>

          {hasBody(e) ? (
            <>
              <div className="adx-trybar">
                <span className="adx-trybar__lbl">请求体</span>
                <SegmentedChips
                  items={[
                    { key: "form", label: "表单" },
                    { key: "json", label: "JSON" },
                  ]}
                  value={bodyMode}
                  onChange={(k) => (k === "json" ? enterJson() : setBodyMode("form"))}
                />
                <Tip
                  text={
                    bodyMode === "json"
                      ? "原样发送这段 JSON（不按参数重排）。切回表单会丢弃这里的改动。"
                      : "按参数拼请求体；留空的参数不会发送。"
                  }
                />
                <span className="sp" />
                {bodyMode === "json" ? (
                  <>
                    <button
                      type="button"
                      className="btn"
                      onClick={() => setJsonText(prettyJson(jsonText))}
                      disabled={jsonErr !== null || jsonText.trim() === ""}
                    >
                      格式化
                    </button>
                    <button
                      type="button"
                      className="btn"
                      onClick={() => {
                        setJsonText(prettyJson(formReq.body ?? ""));
                        setJsonTouched(false);
                      }}
                    >
                      由表单生成
                    </button>
                  </>
                ) : null}
              </div>
              {bodyMode === "json" ? (
                <>
                  <Textarea
                    rows={10}
                    spellCheck={false}
                    className={jsonErr ? "ta--bad" : undefined}
                    value={jsonText}
                    placeholder='{"package_id": "...", "amount": "9.90"}'
                    onChange={(ev) => {
                      setJsonTouched(true);
                      setJsonText(ev.target.value);
                    }}
                  />
                  {jsonErr ? <p className="adx-err">{jsonErr}</p> : null}
                </>
              ) : null}
            </>
          ) : null}

          {needsKey ? (
            <div className="adx-acts">
              {mine.length > 0 ? (
                <>
                  <select
                    className="adx-sel"
                    value={current ?? ""}
                    onChange={(ev) => setPickId(Number(ev.target.value))}
                  >
                    {mine.map((k) => (
                      <option key={k.id} value={k.id}>
                        {k.name}（{k.key_prefix}…）
                      </option>
                    ))}
                  </select>
                  <button
                    type="button"
                    className="btn"
                    onClick={() => void useMine()}
                    disabled={taking}
                  >
                    {taking ? "取用中" : "用我的密钥"}
                  </button>
                  <Tip text="管理员自己的密钥也能调 Reader/Writer 级接口；取用会写一条密钥审计。" />
                </>
              ) : null}
              <button type="button" className="btn" onClick={fillExamples}>
                填入示例值
              </button>
            </div>
          ) : null}

          <div className="adx-acts">
            <button type="button" className="btn btn-primary" disabled={!canSend} onClick={() => void send()}>
              {busy ? "请求中…" : "发送"}
            </button>
            <button type="button" className="btn" onClick={reset} disabled={busy}>
              清空
            </button>
            {req.missing.length ? (
              <span className="missing">还缺：{req.missing.join("、")}</span>
            ) : null}
          </div>

          <Snippet req={req} origin={origin} />

          {failure ? (
            <p className="missing" data-testid="apidoc-try-network">
              {failure}
            </p>
          ) : null}
          {result ? <ResponseView result={result} /> : null}
        </>
      ) : null}
    </section>
  );
}

/**
 * curl / fetch 片段。
 *
 * 与"发送"按钮共用同一个 `BuiltRequest`：屏幕上看到的地址、头、体
 * 与真正发出去的那一份逐字一致。任何"另拼一份"的写法都会在某天分叉，
 * 而接入方正是拿着复制出来的那份去写代码。
 */
function Snippet({ req, origin }: { req: ReturnType<typeof buildRequest>; origin: string }) {
  const [lang, setLang] = useState<"curl" | "fetch">("curl");
  const text = lang === "curl" ? toCurl(req, origin) : toFetchSnippet(req, origin);

  return (
    <div className="api-codeblock adx-snippet">
      <div className="api-codetabs">
        <button type="button" className={lang === "curl" ? "on" : ""} onClick={() => setLang("curl")}>
          curl
        </button>
        <button type="button" className={lang === "fetch" ? "on" : ""} onClick={() => setLang("fetch")}>
          fetch
        </button>
        <button
          type="button"
          className="api-copybtn"
          onClick={() => {
            void copyText(text).then((ok) => {
              if (ok) toastOk("已复制");
              else toastErr("复制失败，请手动选中");
            });
          }}
        >
          复制
        </button>
      </div>
      <pre className="code-block api-codepre">{text}</pre>
    </div>
  );
}
