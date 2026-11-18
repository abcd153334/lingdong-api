import { Fragment, useRef, useState } from "react";
import { toast } from "sonner";
import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Icon } from "@/components/common/Icon";
import { PageHead } from "@/components/common/PageHead";
import { Pagination } from "@/components/common/Pagination";
import {
  useDeletePortalFileMutation,
  usePortalFileUploadMutation,
  usePortalFilesConfigQuery,
  usePortalFilesQuery,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { useSite } from "@/lib/site";
import { copyToast } from "@/lib/clipboard";
import { cn } from "@/lib/utils";
import { Tag, fileTypeKey, tagOf, type TagTone } from "@/components/common/StatusTag";
import type { UploadConfig, UploadItem } from "@/types";

/** 字节 → 可读大小（对齐原型 fmtSize：<1MB 显示 KB，≥10MB 取整） */
function fmtSize(bytes: number): string {
  if (bytes >= 1048576) {
    return `${(bytes / 1048576).toFixed(bytes >= 10485760 ? 0 : 1)} MB`;
  }
  return `${(bytes / 1024).toFixed(0)} KB`;
}

/** 剩余秒数 → 「X 天 X 小时 / X 小时 X 分钟 / X 分钟」（对齐原型 fmtRemain） */
function fmtRemain(seconds: number): string {
  if (seconds <= 0) return "已过期";
  const m = Math.floor(seconds / 60);
  const h = Math.floor(m / 60);
  if (h >= 24) return `${Math.floor(h / 24)} 天 ${h % 24} 小时`;
  if (h >= 1) return `${h} 小时 ${m % 60} 分钟`;
  return `${m} 分钟`;
}

/** TTL 四态：≤1h danger / ≤6h warn / 其余 ok（≤0 由调用处按「已过期」灰色展示） */
type TtlTone = "ok" | "warn" | "danger";

function ttlTone(seconds: number): TtlTone {
  if (seconds <= 3600) return "danger";
  if (seconds <= 6 * 3600) return "warn";
  return "ok";
}

const TTL_TONE: Record<TtlTone, TagTone> = {
  ok: "gray",
  warn: "warning",
  danger: "danger",
};

/** 在存容量分母展示（MB→GB 自适应，对齐原型） */
function fmtTotalMb(mb: number): string {
  return mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`;
}

/** 单条文件的进度条：min(100, used/limit*100) */
function pct(used: number, limit: number): number {
  if (!limit) return 0;
  return Math.min(100, (used / limit) * 100);
}

/* ------------------------------ 接入示例（curl） ------------------------------ */

/** 三段 curl（上传 / 直链 / 删除）；注释写明 403/413/415/429/400 语义；基址走站点配置 */
const curlsOf = (base: string): { t: string; c: string }[] => [
  {
    t: "1 · 上传（API KEY 鉴权，multipart 单文件）",
    c: `curl -X POST ${base}/v1/files \\
  -H "Authorization: Bearer $LD_API_KEY" \\
  -F "file=@photo.jpg"
# 201 → {"id":"01JC…","filename":"photo.jpg","size":2845312,"content_type":"image/png",
#        "url":"${base}/f/01JC…/photo.jpg","expires_at":"…"}
# 403 总闸关闭 / 413 超上限 / 415 类型白名单外 / 429 日限·容量·并发 / 400 空文件`,
  },
  {
    t: "2 · 直链拉取（厂商服务器直接 GET，无鉴权）",
    c: `curl ${base}/f/{file_id}/{safe_name}
# 200 代理流（存储桶私读，不暴露桶）；非 200 一律 410 file_expired（不存在/已删除/已过期）
# 响应头：Content-Type=嗅探值 · Content-Disposition: inline · Cache-Control: private, max-age=300`,
  },
  {
    t: "3 · 主动删除（API KEY 或门户 JWT）",
    c: `curl -X DELETE ${base}/v1/files/{file_id} \\
  -H "Authorization: Bearer $LD_API_KEY"
# 204 成功；404 不存在/非本 KEY/已过期（过期件物理删除由每 5 分钟清理任务兜底）`,
  },
];

/* ------------------------------ 上传引导卡 ------------------------------ */

/**
 * 上传失败的对外文案。
 *
 * 按错误码（而不是 HTTP 状态）分支：同一句「文件上传失败」会让用户不知道该
 * 清旧文件、等明天、还是换个格式，而这三种处置完全不同。
 */
function uploadErrorText(error: unknown): string {
  if (!(error instanceof ApiError)) return "上传失败，请稍后重试";
  switch (error.errorCode) {
    case "file_daily_limit_exceeded":
      return "今日上传次数已达上限，请明天再试";
    case "file_storage_full":
      return "在存容量已达上限，请先删除一些旧文件";
    case "file_concurrency_limit":
      return "上传有点频繁，请稍等片刻再试";
    case "file_too_large":
      return "文件超过单文件上限";
    case "file_type_unsupported":
      return "不支持的文件类型";
    case "file_empty":
      return "文件内容为空";
    case "file_disabled":
      return "管理员已关闭文件上传";
    default:
      return error.message || "上传失败，请稍后重试";
  }
}

/**
 * 上传卡（S8）：真实上传。
 *
 * 门户走会话身份（JWT + CSRF），**不需要 API KEY** —— 日限额的计数主体因此是账号，
 * 与 /v1/files 的「按 KEY 计」不是同一个口径，文案必须说清楚。
 */
function UploadCard({ config }: { config: UploadConfig }) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [drag, setDrag] = useState(false);
  const upMut = usePortalFileUploadMutation();
  const busy = upMut.isPending;

  // 后缀与体积上限都由服务端下发（types[]）：前端不再维护第二份会漂移的映射，
  // 也就不用猜「这个后缀算哪一类、该类上限是多少」。
  const types = config.types ?? [];
  const accept = types.length ? types.map((t) => "." + t.ext).join(",") : undefined;
  const limitOf = (ext: string): number => types.find((t) => t.ext === ext)?.max_mb ?? 0;

  const pick = () => {
    if (!busy) inputRef.current?.click();
  };

  const handleFiles = (list: FileList | null) => {
    const file = list?.[0];
    if (!file || busy) return;
    const ext = (file.name.split(".").pop() ?? "").toLowerCase();
    const cap = limitOf(ext);
    if (types.length && cap === 0) {
      toast.error(`不支持 .${ext}，可上传：${types.map((t) => t.ext).join(" / ")}`);
      return;
    }
    if (file.size === 0) {
      toast.error("文件内容为空");
      return;
    }
    if (cap > 0 && file.size > cap * 1048576) {
      toast.error(`.${ext} 单文件上限 ${cap}MB`);
      return;
    }
    upMut.mutate(file, {
      onSuccess: () => toast.success("上传成功，直链已生成"),
      onError: (error) => toast.error(uploadErrorText(error)),
    });
  };

  return (
    <Card
      title="上传文件"
      sub="multipart 单文件 · 免费不扣费 · 四重限额内即时生效"
      testId="upload-card"
    >
      <div
        role="button"
        tabIndex={0}
        aria-label="选择或拖拽文件上传"
        aria-busy={busy}
        aria-disabled={busy}
        data-testid="upload-zone"
        className={cn("up-zone", drag && "drag", busy && "busy")}
        onClick={pick}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            pick();
          }
        }}
        onDragOver={(e) => {
          e.preventDefault();
          if (!busy) setDrag(true);
        }}
        onDragLeave={() => setDrag(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDrag(false);
          handleFiles(e.dataTransfer.files);
        }}
      >
        <span className="up-ic">
          <Icon name={busy ? "refresh" : "plus"} size={20} className={busy ? "spin" : undefined} />
        </span>
        <div>
          {busy ? (
            <b>上传中…</b>
          ) : (
            <>
              <b>点击选择</b> 或拖拽文件到此处
            </>
          )}
        </div>
        <div className="hint">
          图片 ≤ <span className="mono">{config.limits?.image}</span>MB · 视频 ≤{" "}
          <span className="mono">{config.limits?.video}</span>MB · 有效期{" "}
          <span className="mono">{config.retention_hours}</span> 小时
        </div>
        {/* 真实控件：隐藏不占位（font-size 16px 防 iOS 聚焦放大），交互由 zone 承载（≥44px 触控） */}
        <input
          ref={inputRef}
          type="file"
          accept={accept}
          disabled={busy}
          onChange={(e) => {
            handleFiles(e.target.files);
            e.target.value = "";
          }}
        />
      </div>

      <div data-testid="files-quota">
        <div className="qrow" data-testid="quota-row-today">
          <span className="lbl">
            {/* 计数主体由服务端下发：门户没有 KEY，写死「本 KEY」会让用户去找一把不存在的密钥 */}
            <span>今日上传（{config.quota_scope === "key" ? "本 KEY" : "账号"}）</span>
          </span>
          <div className="qbar">
            <i style={{ width: `${pct(config.today_count, config.daily_limit_per_key)}%` }} />
          </div>
          <b>
            {config.today_count} / {config.daily_limit_per_key}
          </b>
        </div>
        <div className="qrow" data-testid="quota-row-storage">
          <span className="lbl">
            <span>在存容量</span>
          </span>
          <div className="qbar">
            <i style={{ width: `${pct(config.total_bytes, config.user_total_mb * 1048576)}%` }} />
          </div>
          <b>
            {fmtSize(config.total_bytes)} / {fmtTotalMb(config.user_total_mb)}
          </b>
        </div>
      </div>

      <div className="hint">
        每日 {config.daily_limit_per_key} 次 · 单账号同时在存 {fmtTotalMb(config.user_total_mb)} ·
        并发 {config.concurrency} · 到期自动删除
      </div>
    </Card>
  );
}

/* ------------------------------ 文件条目 ------------------------------ */

function FileRow({ file, onDelete, deleting }: { file: UploadItem; onDelete: (f: UploadItem) => void; deleting: boolean }) {
  const expired = file.remaining_seconds <= 0;
  const [typeTone, typeLabel] = tagOf(fileTypeKey(file.content_type));
  const tone = expired ? "gray" : TTL_TONE[ttlTone(file.remaining_seconds)];
  const isImage = file.content_type.startsWith("image/");
  return (
    <div
      data-testid="file-card"
      className={cn("f-card", expired && "expired")}
    >
      <div className="f-top">
        {/* 缩略图直接打直链（同源 /f/，无鉴权）：门户要能一眼看出传的是哪张图，
            只看文件名时用户往往要靠「试下载一次」来确认。 */}
        {isImage && !expired ? (
          <img
            className="f-thumb"
            src={file.url}
            alt={file.filename}
            loading="lazy"
            data-testid="file-thumb"
          />
        ) : null}
        <Tag tone={typeTone} testId="file-type">
          {typeLabel}
        </Tag>
        <span className="fname" data-testid="file-name">
          {file.filename}
        </span>
        <Tag tone={tone} testId="file-ttl" className="mono">
          {expired ? "已过期" : `剩 ${fmtRemain(file.remaining_seconds)}`}
        </Tag>
        <span className="row-acts right f-acts">
          <a
            className="btn btn-sm"
            data-testid="file-download"
            href={file.url}
            download={file.safe_name}
            target="_blank"
            rel="noreferrer"
            aria-disabled={expired}
            onClick={(e) => {
              if (expired) e.preventDefault();
            }}
          >
            下载
          </a>
          <button
            type="button"
            className="btn btn-sm"
            disabled={deleting}
            onClick={() => onDelete(file)}
          >
            删除
          </button>
        </span>
      </div>

      <div className="f-meta">
        <span className="mono">{fmtSize(file.size_bytes)}</span>
        <span className="mono">{file.content_type}</span>
        {/* key_id=0 = 门户直传（没有 KEY 上下文），此时 key_name 为空 */}
        <span>{file.key_name ? `KEY ${file.key_name}` : "门户上传"}</span>
      </div>

      <div className="f-url">
        <code data-testid="file-url">{file.url}</code>
        <button
          type="button"
          className="icon-btn"
          title="复制直链"
          aria-label="复制直链"
          onClick={() => void copyToast(file.url)}
        >
          <Icon name="copy" size={15} />
        </button>
      </div>
    </div>
  );
}

/* ------------------------------ 页面 ------------------------------ */

/**
 * 文件存储（S8）
 * 上传引导（无真实上传，差异 Δ16）+ 配额条 + 文件卡片列表（TTL 四态 / 直链复制 / 删除）+ 接入示例；
 * 总闸关闭（config.enabled=false）时隐藏上传卡并顶部告警，存量文件仍可查看 / 复制 / 删除。
 */
export function FilesPage() {
  const configQuery = usePortalFilesConfigQuery();
  const [page, setPage] = useState(1);
  const filesQuery = usePortalFilesQuery({
    page,
    page_size: DEFAULT_PAGE_SIZE,
  });
  const delMut = useDeletePortalFileMutation();
  const { apiBase } = useSite();

  const config = configQuery.data;
  const items = filesQuery.data?.items ?? [];
  const total = filesQuery.data?.total ?? 0;

  const askDelete = async (file: UploadItem) => {
    const ok = await confirmDialog({
      title: "删除文件",
      body: (
        <>
          确定删除 <b>{file.filename}</b>？
          <br />
          <span className="small muted">存储对象与记录将立即清除，直链即刻失效（410）。</span>
        </>
      ),
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    delMut.mutate(file.id, {
      onSuccess: () => toast.success("文件已删除，直链已失效"),
      onError: (error) => {
        if (error instanceof ApiError && error.errorCode === "file_not_found") {
          toast.error(error.message || "文件不存在或已过期");
          return;
        }
        toast.error(error instanceof ApiError ? error.message : "删除失败，请稍后重试");
      },
    });
  };

  return (
    <>
      <PageHead title="文件存储" desc="为仅接受 URL 输入的模型厂商提供临时文件托管 · 上传即得公网直链" />

      {config && !config.enabled ? (
        <div className="alert-banner info" role="status" data-testid="files-gate-banner">
          管理员已关闭文件上传 —— 存量文件在有效期内仍可查看、复制直链与删除
        </div>
      ) : null}

      {config?.enabled ? <UploadCard config={config} /> : null}

      <Card
        title="我的文件"
        sub="直链可被模型厂商直接拉取（无鉴权）· 到期自动删除"
        testId="files-card"
      >
        {filesQuery.isLoading ? (
          <EmptyState variant="loading" title="正在加载文件…" />
        ) : filesQuery.isError ? (
          <EmptyState
            variant="error"
            title="文件加载失败"
            desc={filesQuery.error instanceof ApiError ? filesQuery.error.message : "请稍后重试"}
            action={
              <button type="button" className="btn btn-primary" onClick={() => void filesQuery.refetch()}>
                重试
              </button>
            }
          />
        ) : !items.length ? (
          <EmptyState
            icon="🗂"
            title="还没有上传过文件"
            desc="上传后即可把直链填进模型请求的 image_url / fileUri 字段。"
          />
        ) : (
          <>
            {items.map((f) => (
              <FileRow key={f.id} file={f} onDelete={askDelete} deleting={delMut.isPending} />
            ))}
            <Pagination total={total} page={page} pageSize={DEFAULT_PAGE_SIZE} onPageChange={setPage} />
          </>
        )}
      </Card>

      <Card
        title="接入示例"
        sub="API KEY 鉴权上传 · 公网直链无鉴权拉取"
        testId="files-api-card"
      >
        {curlsOf(apiBase).map((s) => (
          <Fragment key={s.t}>
            <div className="blk-title">
              <span>{s.t}</span>
              <button type="button" className="btn btn-sm right" onClick={() => void copyToast(s.c)}>
                <Icon name="copy" size={15} />
                复制
              </button>
            </div>
            <div className="code-block">{s.c}</div>
          </Fragment>
        ))}
      </Card>
    </>
  );
}
