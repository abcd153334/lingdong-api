import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { canRender, resolveMediaKind, type MediaKind } from "@/lib/mediaKind";
import { fetchTaskContentBlob, fetchUsageMediaBlob } from "@/lib/apiClient";
import { lockScroll } from "@/lib/scrollLock";
import { Icon } from "./Icon";

/**
 * 媒体预览（零转存）。
 *
 * 三条硬约束（与方案 §3.2.1 一致）：
 *  1. **只有拿得到字节的才预览**：管理台直连上游 `url`、平台既有产物（`artifact`）、
 *     或同步调用的产物代理（`usage`）；
 *  2. 门户**不直连上游**：上游域名本身就说明「背后是谁」。门户统一走
 *     `/api/portal/usage/:id/media/:index`（服务端校验归属、限体积、限类型）；
 *  3. 视频/音频**点击才加载**：打开抽屉就预取一部 30 MB 的片子，
 *     会把"看一眼记录"变成"下载一部片"。图片是例外：小图直接出缩略图，
 *     否则一次出 4 张图要在详情里点 4 次才看得见自己生成了什么。
 */

export interface PreviewItem {
  kind: MediaKind;
  mime?: string;
  bytes?: number;
  /** 源 1：上游 http(s) 链接（仅管理台使用，直连预览） */
  url?: string;
  /** 源 2：平台既有产物（异步任务的 contents，只读） */
  artifact?: { taskId: string; index: number };
  /** 源 3：同步调用的产物（走门户媒体代理，不暴露上游地址） */
  usage?: { usageId: number; index: number };
  /** 不可预览的原因（内联数据 / 链接已脱敏 / 内容未留存） */
  unavailable?: string;
}

/** 超过这个体积只给下载，不在页面里播放（移动端会 OOM）。 */
const MAX_PLAY_BYTES = 50 * 1024 * 1024;
/** 缩略图自动取流上限：再大就退回占位 + 点击（打开抽屉不该拖 10 MB）。 */
const THUMB_MAX_BYTES = 2 * 1024 * 1024;
/** 文本预览上限：超出只给下载（一段 5 MB 的日志塞进 <pre> 会卡死渲染）。 */
const TEXT_PREVIEW_MAX = 64 * 1024;

const KIND_TEXT: Record<MediaKind, string> = {
  image: "图片",
  video: "视频",
  audio: "音频",
  text: "文本",
  file: "文件",
};

function fmtBytes(n?: number): string {
  if (!n || n <= 0) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

/** 有可用的字节来源（三选一）。 */
function hasSource(it: PreviewItem): boolean {
  return Boolean(it.url || it.artifact || it.usage);
}

/**
 * 任务产物 → 预览项（唯一实现）。
 *
 * 调用详情的 `task.contents` 与任务详情的 `contents` 是同一份数据，
 * 两处各写一遍映射必然出现「调用记录里点开是另一张图」。
 * 序号用后端给的稠密 index，不能按数组下标重排。
 */
export function artifactPreviewItems(
  taskId: string,
  arts: { index: number; content_type: string; bytes: number; kind?: string }[],
): PreviewItem[] {
  return arts.map((a) => ({
    kind: resolveMediaKind(a.kind, a.content_type),
    mime: a.content_type,
    bytes: a.bytes,
    artifact: { taskId, index: a.index },
  }));
}

export interface MediaPreviewProps {
  items: PreviewItem[];
  /** 管理台视角还是门户视角（决定产物端点的前缀与鉴权） */
  admin: boolean;
}

export function MediaPreview({ items, admin }: MediaPreviewProps) {
  const [openAt, setOpenAt] = useState<number | null>(null);
  if (items.length === 0) return null;
  return (
    <>
      <div className="cd-media-grid">
        {items.map((it, i) => (
          <MediaCell
            key={`${it.kind}-${it.artifact?.index ?? it.usage?.index ?? i}-${it.url ?? ""}`}
            item={it}
            admin={admin}
            onOpen={() => setOpenAt(i)}
          />
        ))}
      </div>
      {openAt !== null && items[openAt] ? (
        <MediaLightbox
          items={items}
          index={openAt}
          admin={admin}
          onClose={() => setOpenAt(null)}
          onIndex={setOpenAt}
        />
      ) : null}
    </>
  );
}

/** 网格单元：图片直接出缩略图；其它类型只画类型与大小（点击才加载）。 */
function MediaCell({ item, admin, onOpen }: { item: PreviewItem; admin: boolean; onOpen: () => void }) {
  const [failed, setFailed] = useState(false);
  const kind = resolveMediaKind(item.kind, item.mime, item.url);
  const label = KIND_TEXT[kind];
  const can = hasSource(item) && canRender(kind);
  // 只有"小图 + 走平台端点"才自动取流：直链直接 <img>，视频音频本来就该点击加载。
  const wantThumb = kind === "image" && !item.url && (item.bytes ?? 0) <= THUMB_MAX_BYTES;
  const { url: thumbURL, loading: thumbLoading, error: thumbError } = useSourceBlob(item, admin, wantThumb);

  if (kind === "image" && !failed) {
    const src = item.url || thumbURL || "";
    if (src) {
      return (
        <button type="button" className="cd-media-item" onClick={onOpen} aria-label={`预览${label}`}>
          <img src={src} alt={label} loading="lazy" onError={() => setFailed(true)} />
        </button>
      );
    }
    if (wantThumb && thumbLoading) {
      return (
        <span className="cd-media-item is-static">
          <span className="cd-media-ph">
            <Icon name="doc" size={18} />
            <span className="cd-media-kind">{label}</span>
            <span className="small">加载中…</span>
          </span>
        </span>
      );
    }
  }

  // 不可预览时用 span 而不是 disabled button：
  // 灰按钮仍然会被读屏软件念成"按钮"，而它实际上什么也不做。
  const note = failed
    ? "链接已失效"
    : thumbError
      ? "取回失败（可能已过保留期）"
      : item.unavailable || (can ? label : "内容未留存");
  const inner = (
    <span className="cd-media-ph">
      <Icon name={kind === "audio" ? "bell" : kind === "video" ? "eye" : "doc"} size={18} />
      <span className="cd-media-kind">{label}</span>
      {item.mime ? <span className="small">{item.mime}</span> : null}
      {fmtBytes(item.bytes) ? <span className="small">{fmtBytes(item.bytes)}</span> : null}
      <span className="small">{note}</span>
    </span>
  );
  if (!can) {
    return <span className="cd-media-item is-static">{inner}</span>;
  }
  return (
    <button type="button" className="cd-media-item" onClick={onOpen} aria-label={`预览${label}`}>
      {inner}
    </button>
  );
}

/**
 * 预览源的 blob 与 objectURL（任务产物 或 同步调用产物代理）。
 *
 * revoke 必须在 effect cleanup 里做：在 queryFn 里立刻 revoke 会得到白图；
 * 不 revoke 则每次预览都留一份内存（大视频尤其明显）。
 */
function useSourceBlob(item: PreviewItem | undefined, admin: boolean, enabled: boolean) {
  const taskId = item?.artifact?.taskId ?? "";
  const index = item?.artifact?.index ?? 0;
  const usageId = item?.usage?.usageId ?? 0;
  const usageIndex = item?.usage?.index ?? 0;
  const byUsage = Boolean(item?.usage);
  const on = enabled && (Boolean(item?.artifact) || Boolean(item?.usage));
  const q = useQuery({
    queryKey: ["call-media-blob", admin, taskId, index, usageId, usageIndex],
    queryFn: () =>
      byUsage ? fetchUsageMediaBlob(usageId, usageIndex) : fetchTaskContentBlob(taskId, index, admin),
    enabled: on,
    staleTime: Infinity,
    gcTime: 60_000,
    retry: false,
  });
  const [url, setUrl] = useState<string | null>(null);
  useEffect(() => {
    if (!q.data) {
      setUrl(null);
      return;
    }
    const objectURL = URL.createObjectURL(q.data);
    setUrl(objectURL);
    return () => URL.revokeObjectURL(objectURL);
  }, [q.data]);
  return { url, blob: q.data, loading: on && q.isLoading, error: on && q.isError };
}

interface LightboxProps {
  items: PreviewItem[];
  index: number;
  admin: boolean;
  onClose: () => void;
  onIndex: (i: number) => void;
}

/** 全屏预览层。 */
function MediaLightbox({ items, index, admin, onClose, onIndex }: LightboxProps) {
  const item = items[index];
  const total = items.length;
  const kind = resolveMediaKind(item.kind, item.mime, item.url);
  const needsFetch = !item.url && (Boolean(item.artifact) || Boolean(item.usage));
  const { url: sourceURL, blob, loading, error } = useSourceBlob(item, admin, needsFetch);
  const ref = useRef<HTMLDivElement>(null);
  const [text, setText] = useState<string | null>(null);

  // 文本产物：直接把 blob 读成字符串（后端已按 Content-Type 白名单下发）。
  useEffect(() => {
    let alive = true;
    if (kind !== "text" || !blob) {
      setText(null);
      return;
    }
    blob
      .text()
      .then((t) => {
        if (alive) setText(t.length > TEXT_PREVIEW_MAX ? t.slice(0, TEXT_PREVIEW_MAX) : t);
      })
      .catch(() => {
        if (alive) setText(null);
      });
    return () => {
      alive = false;
    };
  }, [blob, kind]);

  // Esc / ← / → / Tab 用**捕获阶段**拦截。
  //
  // 抽屉的 useOverlayBehavior 是在 document 冒泡阶段监听且 Escape 无条件关闭，
  // 不拦的话按一次 Esc 会把预览层和抽屉一起关掉。
  // 只拦这四个键是刻意的：全量 stopPropagation 会让层内按钮收不到 Enter/Space
  // （事件根本到不了 target）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" && e.key !== "ArrowLeft" && e.key !== "ArrowRight" && e.key !== "Tab") {
        return;
      }
      e.stopPropagation();
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
        return;
      }
      if (e.key === "ArrowLeft") {
        e.preventDefault();
        onIndex((index - 1 + total) % total);
        return;
      }
      if (e.key === "ArrowRight") {
        e.preventDefault();
        onIndex((index + 1) % total);
        return;
      }
      // Tab：把焦点环闭在预览层内部（抽屉的焦点环此时不该接管）
      const list = ref.current?.querySelectorAll<HTMLElement>(
        'a[href],button:not([disabled]),[tabindex]:not([tabindex="-1"])',
      );
      if (!list || list.length === 0) return;
      e.preventDefault();
      const first = list[0];
      const last = list[list.length - 1];
      const active = document.activeElement as HTMLElement | null;
      if (e.shiftKey && (active === first || !ref.current?.contains(active))) last.focus();
      else if (!e.shiftKey && active === last) first.focus();
      else (e.shiftKey ? last : first).focus();
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [index, total, onClose, onIndex]);

  // 滚动锁：与抽屉/弹窗共用同一个计数锁（lib/scrollLock.ts）——自己记一份内联
  // 样式再恢复，在「从抽屉里打开预览」时会把底下的抽屉一起解锁。
  useEffect(() => lockScroll(), []);

  const src = item.url || sourceURL || "";
  const tooBig = (item.bytes ?? 0) > MAX_PLAY_BYTES;
  const label = KIND_TEXT[kind];

  const stage = () => {
    if (needsFetch && loading) return <div className="lb-tip">加载中…</div>;
    if (needsFetch && error) return <div className="lb-tip">产物取回失败（可能已过保留期）</div>;
    if (!src && kind !== "text") return <div className="lb-tip">{item.unavailable || "内容未留存，无法预览"}</div>;
    if (tooBig) return <div className="lb-tip">文件超过 {fmtBytes(MAX_PLAY_BYTES)}，请下载后查看</div>;
    if (kind === "image") return <img src={src} alt={label} />;
    if (kind === "video") return <video src={src} controls autoPlay />;
    if (kind === "audio") return <audio src={src} controls autoPlay style={{ width: "80%" }} />;
    if (kind === "text") {
      if (text === null) return <div className="lb-tip">{item.unavailable || "没有可读取的文本内容"}</div>;
      return <pre className="json-view json-view--tall lb-text">{text}</pre>;
    }
    return <div className="lb-tip">该类型不支持预览，请下载后查看</div>;
  };

  return (
    <div
      className="lb"
      role="dialog"
      aria-modal="true"
      aria-label="媒体预览"
      ref={ref}
      onClick={(e) => {
        // 点击空白处关闭（点内容不关）
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="lb-head">
        <span className="mono small">
          {index + 1} / {total}
        </span>
        <span className="small">{label}</span>
        <button type="button" className="lb-btn" aria-label="关闭预览" onClick={onClose} style={{ marginLeft: "auto" }}>
          <Icon name="close" size={18} />
        </button>
      </div>
      <div className="lb-stage">{stage()}</div>
      <div className="lb-foot">
        <span className="small">
          {[item.mime, fmtBytes(item.bytes)].filter(Boolean).join(" · ")}
        </span>
        <span style={{ marginLeft: "auto", display: "flex", gap: 8 }}>
          {src ? (
            <>
              <a className="lb-btn" href={src} download={`call-media-${index + 1}`}>
                下载
              </a>
              {item.url ? (
                <a className="lb-btn" href={item.url} target="_blank" rel="noreferrer">
                  打开原链接
                </a>
              ) : null}
            </>
          ) : null}
          {total > 1 ? (
            <>
              <button type="button" className="lb-btn" aria-label="上一张" onClick={() => onIndex((index - 1 + total) % total)}>
                ‹
              </button>
              <button type="button" className="lb-btn" aria-label="下一张" onClick={() => onIndex((index + 1) % total)}>
                ›
              </button>
            </>
          ) : null}
        </span>
      </div>
    </div>
  );
}
