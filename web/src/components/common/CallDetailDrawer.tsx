import { useMemo, useState, type ReactNode } from "react";

import { CallSummaryBar, LatencyDetail } from "@/components/common/CallSummaryBar";
import { ChargeBreakdown } from "@/components/common/ChargeBreakdown";
import { CollapseSection } from "@/components/common/CollapseSection";
import { KV, LongID } from "@/components/common/DetailBits";
import { copyText } from "@/lib/clipboard";
import { Drawer } from "@/components/common/Drawer";
import { EntityCell } from "@/components/common/EntityCell";
import { EmptyState } from "@/components/common/EmptyState";
import { artifactPreviewItems, MediaPreview, type PreviewItem } from "@/components/common/MediaPreview";
import { MoneyText, QuotaUnit } from "@/components/common/MoneyText";
import { ModalityTag, StatusTag } from "@/components/common/StatusTag";
import { TaskInfoSection } from "@/components/common/TaskInfoSection";
import { callKindLabel } from "@/lib/charge";
import { formatBytes, formatDateTime, formatNum } from "@/lib/format";
import { resolveMediaKind } from "@/lib/mediaKind";
import { groupDiscountText, moneyAt } from "@/lib/money";
import { isActiveTask } from "@/lib/task";
import type {
  AdminCallDetail,
  AdminTaskItem,
  CallIODetail,
  PortalCallDetail,
  TaskItem,
} from "@/types";

/**
 * 调用记录 / 异步任务详情抽屉（管理台与门户共用，同一套骨架与分区）。
 *
 * 两种数据源、同一个组件：
 *  - `detail`：调用记录（`/api/{portal,admin}/usage|logs/:id`）；
 *  - `task`：异步任务（`/api/{portal,admin}/tasks/:id`）。
 * 异步任务本身就是 `call_kind=async` 的调用记录，拆成第二个抽屉只会让
 * 「同一份任务事实」在两处显示成不同字段、不同顺序、不同空态。
 *
 * 结构：摘要条（一行结论）+ 折叠分区。
 * 为什么不分成 Tab：用户经常要**对照**「我发的」与「我拿回的」，
 * Tab 会强迫来回切换；折叠可以让两者在同一屏里上下对照。
 *
 * 文案约定：本组件的说明文字一律 `muted small`，不用 `Tip` 也不用 `.hint` ——
 * 门禁 M19/M21 对**未登记文件**的基线是 0，一个新加的 `tip=` 就直接红灯。
 */

export interface CallDetailDrawerProps {
  open: boolean;
  onClose: () => void;
  /** 调用记录模式的数据（与 task 互斥；两者同时给时 detail 优先） */
  detail?: PortalCallDetail | null;
  /** 异步任务模式的数据（任务列表的「详情」） */
  task?: TaskItem | AdminTaskItem | null;
  loading?: boolean;
  error?: string;
  onRetry?: () => void;
  /** 门户视角：不展示排障区（渠道/适配器/IP/倍率链）与任何报文原文 */
  portal?: boolean;
  /** 页脚动作（取消任务 / 重试）：由页面注入，这样确认弹窗与 mutation 留在页面层 */
  actions?: ReactNode;
}

/** 把文本下载为文件（本地生成 Blob，不新增后端下载端点）。 */
function downloadText(filename: string, text: string) {
  const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

/** 尝试把 JSON 文本格式化；失败原样返回（说明它是纯文本或已被截断）。 */
function prettyJSON(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed || (trimmed[0] !== "{" && trimmed[0] !== "[")) return raw;
  try {
    return JSON.stringify(JSON.parse(trimmed), null, 2);
  } catch {
    return raw;
  }
}

/**
 * 报文块：默认折叠到 6 行，展开后可格式化 / 复制 / 下载。
 *
 * 默认折叠是"去冗余"的关键一步：一段 200 行 JSON 直接铺满抽屉，
 * 会把下面的扣费与排障区挤到要滚几屏才看得到。
 */
function PayloadBlock({
  title,
  raw,
  filename,
  truncated,
  note,
}: {
  /** 必填：这一块到底是「客户端入站」还是「实际出站」，不能靠默认文案猜 */
  title: string;
  raw: string;
  filename: string;
  truncated?: boolean;
  note?: string;
}) {
  const [pretty, setPretty] = useState(true);
  const [expanded, setExpanded] = useState(false);
  const text = useMemo(() => (pretty ? prettyJSON(raw) : raw), [pretty, raw]);
  const isJSON = text.trim().startsWith("{") || text.trim().startsWith("[");
  return (
    <div className="cd-payload">
      <div className="cd-payload-head">
        <span>{title}</span>
        <span className="cd-payload-acts">
          {isJSON ? (
            <button type="button" className="btn btn-sm" onClick={() => setPretty((v) => !v)}>
              {pretty ? "原始" : "格式化"}
            </button>
          ) : null}
          <button type="button" className="btn btn-sm" onClick={() => void copyText(text)}>
            复制
          </button>
          <button type="button" className="btn btn-sm" onClick={() => downloadText(filename, text)}>
            下载
          </button>
          <button type="button" className="btn btn-sm" onClick={() => setExpanded((v) => !v)}>
            {expanded ? "收起" : "展开"}
          </button>
        </span>
      </div>
      {truncated ? <div className="muted small">超过留存上限，以下为截断后的前缀。</div> : null}
      {note ? <div className="muted small">{note}</div> : null}
      <pre className={expanded ? "json-view json-view--tall" : "json-view json-view--clamp"}>{text}</pre>
    </div>
  );
}

/**
 * 把后端 media 与任务产物合成统一的预览项（按产物序号去重）。
 *
 * 门户**不读 `m.url`**：后端门户投影里根本没有这个字段，产物字节统一走
 * `/api/portal/usage/:id/media/:index`（服务端校验归属 + 限体积 + MIME 白名单）。
 * 管理台保留上游直连（运营排障要看"这张图是不是上游真的返回了"）。
 */
function buildPreviewItems(d: PortalCallDetail, portal?: boolean): PreviewItem[] {
  // 有任务引用时，产物只有一个真相源：task.contents（后端按 files 实时过滤过期项）。
  // call_io.media 是结算那一刻的快照，产物过期后它仍列着那一项、点开必然 410；
  // 两者再按 artifact_index 去重合并，序号口径一漂就是"点开是另一张图"。
  if (d.task) {
    return artifactPreviewItems(d.task.id, d.task.contents ?? []);
  }
  const io = d.call_io;
  const items: PreviewItem[] = [];
  // 下标必须用**真实数组序号**：以前用 items.length 推导，一旦某个分支不 push，
  // 后面的媒体就会拿到错的序号 —— 表现为点开是另一张图。
  (io?.media ?? []).forEach((m, i) => {
    const raw = portal ? undefined : (m as { url?: string }).url;
    const kind = resolveMediaKind(m.kind, m.mime, raw);
    const base = { kind, mime: m.mime, bytes: m.bytes };
    if (raw) {
      items.push({ ...base, url: raw });
    } else if (portal) {
      // 同步调用的产物：走平台代理，不暴露上游地址。
      items.push({ ...base, usage: { usageId: d.id, index: i } });
    } else {
      items.push({ ...base, unavailable: m.note || "内容未留存" });
    }
  });
  return items;
}

/** 没有请求正文时的原因（必须互斥，不能说"过期"当万能答案）。 */
function missingRequestReason(d: PortalCallDetail): string {
  if (d.is_rejected || d.status === "rejected") return "本次请求在鉴权/校验阶段被拒绝，没有产生请求报文";
  if (d.status === "pending") return "请求进行中，完成后可查看";
  if (!d.call_io) return "本次调用未留存报文（已过保留期，或平台未开启留存）";
  return "请求正文未留存";
}

/** 没有响应正文时的原因（同样必须互斥）。 */
function missingResponseReason(d: PortalCallDetail): string {
  if (d.is_rejected || d.status === "rejected") return "本次请求未发往模型，没有响应正文";
  if (d.status === "pending") return "请求进行中，完成后可查看";
  if (!d.call_io) return "本次调用未留存报文（已过保留期，或平台未开启留存）";
  if (d.http_status === 499) return "客户端提前断开，没有拿到完整响应";
  if (d.is_stream) return "流式响应未留存正文，仅记录了帧数";
  if ((d.call_io.media ?? []).some((m) => resolveMediaKind(m.kind, m.mime) === "audio")) {
    return "本次响应是音频二进制，平台不保存内容（需要可重新调用生成）";
  }
  if (d.status === "released") return "本次调用未完成，没有响应正文";
  return "响应正文未留存";
}

/** 结果区为空时的原因。 */
function resultEmptyReason(d: PortalCallDetail, portal?: boolean): string {
  if (d.is_rejected || d.status === "rejected") return "本次请求未通过校验，无结果";
  if (d.status === "pending") return "仍在进行中，完成后会显示结果";
  // 异步调用的产物只来自任务；任务行被物理清理后，media 快照也不再写，
  // 必须说清"是任务记录没了"，不能让用户以为是普通空结果。
  if (d.task) return taskArtifactReason(d.task);
  if (d.call_kind === "async") return "任务记录已清理，产物不可用";
  if (!d.call_io) return "本次调用未留存结果（历史记录，或已过保留期）";
  return portal ? "本次没有可预览的结果，响应正文见「响应内容」" : "本次没有可预览的结果";
}

/** 任务产物为空时的原因（按状态互斥，不能一句「暂无数据」走天下）。 */
function taskArtifactReason(
  task: { status: string; contents?: { index: number }[] },
  loading?: boolean,
): string {
  // 详情还没到 ≠ 没有产物：先把"正在读"说出来，避免一秒钟的假空态。
  if (loading && task.contents === undefined) return "正在读取产物…";
  if (isActiveTask(task.status)) return "任务进行中，完成后会显示产物";
  if (task.status === "succeeded") return "任务已完成，但没有可用的产物（未生成，或已过保留期）";
  return "任务未完成，没有产物";
}

/** 正文是否是完整 JSON（被留存上限截断的前缀解析会失败）。 */
function looksComplete(raw?: string): boolean {
  const t = (raw ?? "").trim();
  if (!t.startsWith("{") && !t.startsWith("[")) return true;
  try {
    JSON.parse(t);
    return true;
  } catch {
    return false;
  }
}

/** 请求区的口径说明（按字段互斥推导，不猜）。 */
function requestNote(io: CallIODetail): string | undefined {
  const parts: string[] = [];
  if (io.req_source === "snapshot") parts.push("任务参数快照：字段顺序与未知字段可能已丢失");
  if (io.req_source === "rebuilt") parts.push("提交的文件已转为内联数据并脱敏");
  if (io.body_mode === "meta") parts.push("只保留结构摘要（模型 / 条数 / 尺寸），不含正文");
  return parts.length ? parts.join("；") : undefined;
}

/** 响应区的口径说明。 */
function responseNote(d: PortalCallDetail, io: CallIODetail): string | undefined {
  if (d.http_status >= 400) return "本次请求失败，以下是平台返回的错误响应";
  if (d.is_stream) return "流式响应：以下是拼接后的完整结果，不是逐帧原文";
  if (io.body_mode === "meta") return "只保留结构摘要（状态 / 用量等），不含正文";
  if (!looksComplete(io.resp_body)) return "内容超过留存上限，以下为截断后的前缀";
  return undefined;
}

export function CallDetailDrawer({
  open,
  onClose,
  detail,
  task,
  loading,
  error,
  onRetry,
  portal,
  actions,
}: CallDetailDrawerProps) {
  const isTask = !detail && Boolean(task);
  const data = detail ?? task ?? null;

  const body = () => {
    if (error) {
      return (
        <EmptyState
          icon="⚠"
          title="详情加载失败"
          desc={error}
          action={
            onRetry ? (
              <button type="button" className="btn btn-primary" onClick={onRetry}>
                重试
              </button>
            ) : undefined
          }
        />
      );
    }
    if (!data) {
      // 详情是异步拉的：没数据就是还没到（loading 与"请求刚发出"之间有窗口期，
      // 只判 loading 会让抽屉在那几十毫秒里显示成空白）。
      return <div className="muted small">{loading === false ? "暂无数据" : "加载中…"}</div>;
    }
    if (isTask && task) {
      return <TaskDetailBody task={task} loading={loading} portal={portal} />;
    }

    const d = data as PortalCallDetail;
    const io = d.call_io;
    // 管理台字段（渠道/适配器/IP/服务商单号/报文原文/倍率链）只在非门户视角读取：
    // 门户契约里根本没有这些键，读了也只会得到 undefined。
    const admin = portal ? null : (d as AdminCallDetail);
    const items = buildPreviewItems(d, portal);
    const hasOutput = Boolean(io?.output_text) || items.length > 0;
    // 门户拿分类文案（error_summary），管理台仍是脱敏后的服务商原文（error_msg）。
    const errorText = d.error_summary || admin?.error_msg || "";
    // 正文里的 ****** 是脱敏占位：用户看到它时容易以为"平台改了我的请求"。
    const redacted = Boolean(io?.req_raw?.includes("******") || io?.resp_body?.includes("******"));

    return (
      <>
        <CallSummaryBar detail={d} />
        {errorText ? (
          <div className="cd-fail">
            <span>{errorText}</span>
            <button type="button" className="btn btn-sm" onClick={() => void copyText(errorText)}>
              复制
            </button>
          </div>
        ) : null}

        <CollapseSection
          title="生成结果"
          defaultOpen
          summary={items.length > 0 ? `${items.length} 个结果` : io?.output_text ? "仅文本" : undefined}
        >
          {items.length > 0 ? <MediaPreview items={items} admin={!portal} /> : null}
          {/* 报文原文（含服务商错误体）只有管理台可见：门户给的是分类文案 + 请求 ID。 */}
          {admin?.call_io?.resp_error ? (
            <PayloadBlock
              title="上游错误（原始）"
              raw={admin.call_io.resp_error}
              filename={`upstream-error-${d.id}.txt`}
            />
          ) : null}
          {admin?.call_io?.resp_raw ? (
            <PayloadBlock
              title={
                admin.call_io.resp_source === "aggregate" || d.is_stream
                  ? "上游返回（流式拼接）"
                  : "上游返回（原文）"
              }
              raw={admin.call_io.resp_raw}
              filename={`response-${d.id}.json`}
              note={
                admin.call_io.resp_source === "usage"
                  ? "上游没有返回完整响应体，此处为上游自报用量原文"
                  : admin.call_io.resp_source === "aggregate" || d.is_stream
                    ? "流式响应的拼接结果，不是上游逐帧原文（帧数见排障信息）。"
                    : admin.call_io.body_mode === "meta"
                      ? "结构摘要口径（LD_CALL_IO_BODY=meta）。"
                      : undefined
              }
            />
          ) : null}
          {/* 交付体：与上游原文不同才有值（产物直链改写 / 协议转码 / 失败错误体）。
              排障时对比这两块，就能回答「用户说他收到的不是这个」到底差在哪。 */}
          {admin?.call_io?.delivered_raw ? (
            <PayloadBlock
              title="交付给客户端的响应（改写/转码后）"
              raw={admin.call_io.delivered_raw}
              filename={`delivered-${d.id}.json`}
            />
          ) : null}
          {/* 门户的"结果"就是抽出来的纯文本输出（≤8 KiB），不含任何报文结构。 */}
          {portal && io?.output_text ? (
            <PayloadBlock title="输出文本" raw={io.output_text} filename={`output-${d.id}.txt`} />
          ) : null}
          {portal && io ? (
            <div className="cd-muted">
              请求 {formatBytes(io.req_bytes)} · 响应 {formatBytes(io.resp_bytes)}
              {io.sse_frames > 0 ? ` · 流式 ${io.sse_frames} 帧` : ""}
            </div>
          ) : null}
          {!hasOutput && !admin?.call_io?.resp_error ? (
            <div className="muted small">{resultEmptyReason(d, portal)}</div>
          ) : null}
        </CollapseSection>

        {/* 任务信息：与「异步任务」列表打开的抽屉共用同一个分区组件 ——
            同一份任务事实不能因为入口不同就换一套字段与顺序。 */}
        {d.task ? <TaskInfoSection task={d.task} rawError={admin?.task_error_msg} /> : null}

        {/* 门户的「请求 / 响应」两段：给的是**用户自己的**报文。
            req_raw 是他发进来的，resp_body 是平台真正返回给他的那一份；
            服务商地址与上游原文（upstream_* / resp_raw / resp_error）永不下发。 */}
        {portal ? (
          <>
            <CollapseSection
              title="请求内容"
              defaultOpen
              summary={io?.req_raw ? formatBytes(io.req_bytes) || "已留存" : "未留存"}
            >
              {io?.req_raw ? (
                <PayloadBlock
                  title="我发出的请求"
                  raw={io.req_raw}
                  filename={`request-${d.id}.json`}
                  truncated={io.req_truncated}
                  note={requestNote(io)}
                />
              ) : (
                <div className="muted small">{missingRequestReason(d)}</div>
              )}
            </CollapseSection>

            <CollapseSection
              title="响应内容"
              defaultOpen
              summary={io?.resp_body ? formatBytes(io.resp_bytes) || "已留存" : "无"}
            >
              {io?.resp_body ? (
                <PayloadBlock
                  title="我收到的响应"
                  raw={io.resp_body}
                  filename={`response-${d.id}.json`}
                  note={responseNote(d, io)}
                />
              ) : (
                <div className="muted small">{missingResponseReason(d)}</div>
              )}
              {redacted ? (
                <div className="cd-muted">正文中的 ****** 表示密钥、签名与内联二进制内容已自动隐藏。</div>
              ) : null}
            </CollapseSection>
          </>
        ) : null}

        {/* 「请求参数（原始）」只对管理台开放：门户拿到的是脱敏后的统计量，
            原文里既有服务商地址也有上游模型名。 */}
        {portal ? null : (
          <CollapseSection
            title="客户端入站"
            defaultOpen
            summary={admin?.call_io?.req_raw ? `${formatNum(admin.call_io.req_bytes || 0)} B` : "未留存"}
          >
            {admin?.call_io?.req_raw ? (
              <PayloadBlock
                title="入站报文（客户端发的）"
                raw={admin.call_io.req_raw}
                filename={`request-${d.id}.json`}
                truncated={admin.call_io.req_truncated}
                note={requestNote(admin.call_io)}
              />
            ) : (
              <div className="muted small">{missingRequestReason(d)}</div>
            )}
          </CollapseSection>
        )}

        {/* 实际出站报文：与入站并排，排障时才能一眼看出平台改了什么。
            没有它的时候，看到入站摘要会误以为「下游返回异常」（2026-11-04 的教训）。 */}
        {portal ? null : (
          <CollapseSection
            title="实际出站"
            defaultOpen
            summary={
              admin?.call_io?.upstream_url
                ? `${admin.call_io.upstream_status || "无响应"} · ${formatNum(admin.call_io.upstream_bytes || 0)} B`
                : "未采集"
            }
          >
            {admin?.call_io?.upstream_url ? (
              <>
                <div className="cd-kv">
                  <KV k="地址">{admin.call_io.upstream_url}</KV>
                  <KV k="状态">
                    {admin.call_io.upstream_status
                      ? String(admin.call_io.upstream_status)
                      : "未收到响应（连接失败或超时）"}
                  </KV>
                  <KV k="尝试">{String(admin.call_io.upstream_attempts ?? 0)}</KV>
                </div>
                {admin.call_io.upstream_raw ? (
                  <PayloadBlock
                    title="出站报文（平台发给上游的）"
                    raw={admin.call_io.upstream_raw}
                    filename={`upstream-${d.id}.json`}
                    truncated={admin.call_io.upstream_truncated}
                    note={
                      admin.call_io.upstream_source === "tap-meta"
                        ? "结构摘要口径（LD_CALL_IO_BODY=meta），不含正文。"
                        : admin.call_io.upstream_source === "tap-skipped"
                          ? "请求体是流式或二进制（文件上传），无法复制留存；只记录了地址与状态。"
                          : undefined
                    }
                  />
                ) : (
                  <div className="muted small">未采集到出站报文（二进制/流式请求体，或该列为旧记录）。</div>
                )}
              </>
            ) : (
              <div className="muted small">未采集到出站报文（流式上传，或该列为旧记录）。</div>
            )}
          </CollapseSection>
        )}

        <CollapseSection title="扣费明细">
          <ChargeBreakdown detail={d} portal={portal} />
        </CollapseSection>

        {/* 排障信息：管理台全量，门户只保留"报障必需"的两项。
            门户不给渠道/适配器/IP/服务商单号/倍率链 —— 那属于供应链与内部拓扑；
            但请求 ID 必须给：用户报障时平台第一句就问"要一下请求 ID"。 */}
        <CollapseSection title="排障信息" summary={admin?.channel_name || undefined}>
          <LatencyDetail detail={d} />
          <div className="cd-kv">
            {admin?.channel_name ? (
              <KV k="渠道">
                <EntityCell iconUrl={admin.channel_icon_url} name={admin.channel_name} />
              </KV>
            ) : null}
            {admin?.adapter_id ? (
              <KV k="适配器">
                <span className="mono small">{admin.adapter_id}</span>
              </KV>
            ) : null}
            {d.request_id ? (
              <KV k="请求 ID">
                <LongID value={d.request_id} />
              </KV>
            ) : null}
            {admin?.upstream_request_id ? (
              <KV k="上游单号">
                <LongID value={admin.upstream_request_id} />
              </KV>
            ) : null}
            {admin?.request_digest ? (
              <KV k="请求摘要">
                <span className="mono small">{admin.request_digest}</span>
              </KV>
            ) : null}
            {admin?.ip ? (
              <KV k="IP">
                <span className="mono small">{admin.ip}</span>
              </KV>
            ) : null}
            {admin?.group ? (
              <KV k="分组">
                <span className="mono small">{admin.group}</span>
              </KV>
            ) : null}
            {d.call_kind ? <KV k="调用来源">{callKindLabel(d.call_kind)}</KV> : null}
            <KV k="开始时间">
              <span className="mono small">
                {d.started_at ? formatDateTime(d.started_at) : formatDateTime(d.created_at)}
              </span>
            </KV>
            {d.finished_at ? (
              <KV k="结束时间">
                <span className="mono small">{formatDateTime(d.finished_at)}</span>
              </KV>
            ) : null}
          </div>
          {admin ? <PriceSnapshot detail={admin} /> : null}
          {admin?.raw_usage ? (
            <div className="cd-payload">
              <div className="cd-payload-head">
                <span>上游用量原文</span>
              </div>
              <pre className="json-view json-view--clamp">{prettyJSON(admin.raw_usage)}</pre>
            </div>
          ) : null}
        </CollapseSection>
      </>
    );
  };

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={data ? `${isTask ? "任务详情" : "调用明细"} #${data.id}` : isTask ? "任务详情" : "调用明细"}
      // 页脚只放动作（取消任务 / 重试），不放「关闭」：抽屉头部已有关闭按钮
      // （aria-label="关闭"），两个同名按钮会让 getByRole("button",{name:"关闭"}) 命中 2 个。
      footer={actions}
    >
      {body()}
    </Drawer>
  );
}

/**
 * 任务模式的摘要条（一行回答「成没成功、什么模型、多少钱」）。
 *
 * 与 `CallSummaryBar` 同一套 `.cd-summary` 结构与类名，只是数据源从 usage 换成 task：
 * 任务没有耗时/首字延迟/密钥名，多的是模态与金额口径（进行中=冻结，终态=结算）。
 */
function TaskSummaryBar({ task }: { task: TaskItem | AdminTaskItem }) {
  const active = isActiveTask(task.status);
  const amount = active ? task.frozen : task.settled;
  return (
    <div className="cd-summary">
      <StatusTag status={task.status} />
      <span className="model" title={task.model}>
        <EntityCell iconUrl={task.model_icon_url} name={task.model} mono />
      </span>
      <ModalityTag modality={task.modality} />
      <span className="cd-sep" aria-hidden="true" />
      <span className="muted small">{formatDateTime(task.created_at)}</span>
      {task.frozen > 0 || task.settled > 0 ? (
        <QuotaUnit micro={amount} className="small" />
      ) : null}
      <span className="muted small">{active ? "冻结中" : "已结算"}</span>
      <button
        type="button"
        className="cd-id"
        title="复制任务 ID"
        onClick={() => void copyText(String(task.id))}
      >
        #{task.id}
      </button>
    </div>
  );
}

/** 任务模式的主体：生成结果 / 任务信息 / 扣费明细 /（管理台）排障信息。 */
function TaskDetailBody({
  task,
  loading,
  portal,
}: {
  task: TaskItem | AdminTaskItem;
  loading?: boolean;
  portal?: boolean;
}) {
  const arts = task.contents ?? [];
  const items = artifactPreviewItems(task.id, arts);
  const active = isActiveTask(task.status);
  const admin = portal ? null : (task as AdminTaskItem);

  // 结论句按状态分派；失败/超时/取消已由后端全额释放，不能只说「已结算 0」。
  const chargeLead = active
    ? "任务进行中，已预扣额度，结束后按实际结算"
    : task.status === "succeeded"
      ? "任务已完成，按实际结算"
      : "任务未完成，预扣额度已全额退回";

  return (
    <>
      <TaskSummaryBar task={task} />

      <CollapseSection
        title="生成结果"
        defaultOpen
        summary={items.length > 0 ? `${items.length} 个结果` : undefined}
      >
        {items.length > 0 ? (
          <>
            <MediaPreview items={items} admin={!portal} />
            {/* 不写死天数：产物保留期由 files.expires_at 决定，写「7 天」只会在改配置后骗人。 */}
            <div className="cd-muted">产物有保留期，过期后无法预览或下载。</div>
          </>
        ) : (
          <div className="muted small">{taskArtifactReason(task, loading)}</div>
        )}
      </CollapseSection>

      <TaskInfoSection task={task} loading={loading} />

      <CollapseSection title="扣费明细">
        <div className="cd-charge">
          <div className="cd-charge-lead">{chargeLead}</div>
          {task.frozen > 0 ? (
            <div className="cd-charge-line">
              <span className="k">冻结额</span>
              <span className="v mono">
                <MoneyText micro={task.frozen} />
              </span>
            </div>
          ) : null}
          <div className="cd-charge-line is-total">
            <span className="k">已结算</span>
            <span className="v">
              <MoneyText micro={task.settled} />
            </span>
          </div>
        </div>
      </CollapseSection>

      {/* 排障信息只在管理台渲染：门户的 AdminTaskItem 字段根本不下发，
          而「平台 / 渠道 / 上游单号」属于供应链拓扑。 */}
      {admin ? (
        <CollapseSection title="排障信息">
          <div className="cd-kv">
            <KV k="任务 ID">
              <span className="mono small">#{admin.id}</span>
            </KV>
            <KV k="用户 / 密钥">
              <span className="mono small">
                #{admin.user_id} · #{admin.key_id}
              </span>
            </KV>
            <KV k="平台 / 渠道">
              <span className="mono small">
                {admin.platform || "—"} · #{admin.channel_id}
              </span>
            </KV>
            <KV k="上游任务 ID">
              {admin.upstream_task_id ? (
                <LongID value={admin.upstream_task_id} />
              ) : (
                <span className="muted small">未提交</span>
              )}
            </KV>
          </div>
        </CollapseSection>
      ) : null}
    </>
  );
}

/**
 * 价目快照（仅管理台）：这笔钱是按哪些单价算出来的。
 *
 * 改造前这里是「倍率链」（model_ratio / group_ratio …），而快照结构里
 * 那些键**已经不存在**了 —— `rows.filter(数字)` 一过滤，整块静默消失，
 * 于是「凭什么收这个价」在管理台再也查不到。现在直接读快照里真实存在的
 * 绝对价目（微）与折扣，口径与后端 `billing.Snapshot` 一一对应。
 */
const SNAP_LANE_LABELS: Record<string, string> = {
  input: "输入",
  output: "输出",
  cache: "缓存读取",
  cache_create: "缓存写入",
  cache_create_1h: "1h 缓存写入",
  image: "图片输入",
  audio: "音频输入",
  audio_output: "音频输出",
  call: "每次",
};

const SNAP_SPEC_LABELS: Record<string, string> = {
  resolution_fixed: "默认档",
  per_clip_price: "每条",
  per_task_price: "每次",
  per_second_price: "每秒",
  min_price: "最低消费",
  ref_image_price: "参考图",
  ref_audio_price: "参考音频",
  ref_video_price: "参考视频",
};

function PriceSnapshot({ detail }: { detail: AdminCallDetail }) {
  const snap = detail.pricing_snapshot as Record<string, unknown> | undefined;
  if (!snap) return null;
  const cur = typeof snap.currency === "string" ? snap.currency : detail.currency;

  const items: string[] = [];
  const push = (label: string, v: unknown) => {
    if (typeof v === "number") items.push(`${label} ${moneyAt(v, cur)}`);
  };

  const price = snap.price as Record<string, unknown> | undefined;
  if (price) {
    for (const [key, label] of Object.entries(SNAP_LANE_LABELS)) push(label, price[key]);
  }

  const sp = snap.spec_price as Record<string, unknown> | undefined;
  if (sp) {
    for (const key of ["tier_price", "second_price", "ref_video_second_price"]) {
      const map = sp[key];
      if (!map || typeof map !== "object") continue;
      for (const [tier, v] of Object.entries(map as Record<string, unknown>)) push(tier, v);
    }
    for (const [key, label] of Object.entries(SNAP_SPEC_LABELS)) push(label, sp[key]);
  }

  const discount = typeof snap.discount === "number" ? snap.discount : null;
  const discText = discount !== null ? groupDiscountText(discount) : "";

  if (items.length === 0 && (!discText || discText === "无折扣")) return null;
  return (
    <div className="cd-muted">
      {items.join(" · ")}
      {discText && discText !== "无折扣" ? `　折扣 ${discText}` : ""}
    </div>
  );
}
