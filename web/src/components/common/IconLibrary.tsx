import { useMemo, useRef, useState, type ChangeEvent, type KeyboardEvent } from "react";
import { toast } from "sonner";

import { EntityIcon } from "./EntityIcon";
import { Input } from "./Input";
import { useAdminIconsQuery, useUploadIconMutation } from "@/lib/api";
import { builtinIconToken } from "@/lib/entityIcon";
import { formatDateTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { BrandIconItem } from "@/types";

/**
 * 分组标题与顺序。
 *
 * 顺序写在前端是展示决策（哪个分组更常用），但**分组集合以数据为准**：
 * 后端加了第 4 个分组而这里没登记时，会以原始 group 值追加在最后，
 * 而不是让那批图标凭空消失（消失的表现是"图库里明明有，就是选不到"）。
 */
const GROUP_LABELS: Record<string, string> = {
  llm: "大模型厂商",
  media: "图像 / 视频 / 音频",
  platform: "云平台与推理工具",
};
const GROUP_ORDER = ["llm", "media", "platform"];

const UPLOADED_LABEL = "已上传";

/**
 * 网格列数（与 `.ipop-grid` 的 `repeat(4, 1fr)` 严格一致）。
 *
 * 方向键上下移动必须知道列数。写成 `auto-fill` 的话只能读
 * `getComputedStyle(el).gridTemplateColumns` 反解，既脆又慢，还会在
 * 面板宽度动画期间给出错的列数；固定 4 列是 360px 面板里 76px 格子的自然结果。
 */
const COLS = 4;

interface LibItem {
  /** 写回实体 icon 列的令牌 */
  token: string;
  /** 直接渲染的地址（后端已拼好，前端不参与路径规则） */
  url: string;
  /** 首字占位用的名字（图片加载失败时才会显示） */
  name: string;
  /** 格子下方的可见文字 */
  label: string;
  /** 悬浮标题（可见文字被省略号截断时补全） */
  title: string;
}

interface LibSection {
  key: string;
  label: string;
  /** 本节第一格在 flat 里的下标（方向键与 roving tabindex 都按下标算） */
  start: number;
  items: LibItem[];
}

export interface IconLibraryProps {
  /** 当前 token（`""` | `builtin:` | `upload:`） */
  value: string;
  /**
   * 选中某一项：同时给出令牌（写回用）与地址（预览用）。
   *
   * 只给令牌不够用：表单里「刚选中、还没保存」这一帧要立刻画出预览，
   * 而该令牌还没经过任何实体的读投影、拿不到 `icon_url`。
   */
  onPick: (token: string, url: string) => void;
  /** 清空为「首字占位」（等价提交 `""`） */
  onClear: () => void;
  className?: string;
}

/**
 * 图标库面板内容（**不含触发器、不含定位**，由 Popover 承载）。
 *
 * 三件事必须留在这里，因为只有它知道网格的完整顺序：
 *   1. 分组顺序 + 未登记分组追加末尾（见 GROUP_LABELS 注释）；
 *   2. 搜索过滤（key / 名称 / 别名）；
 *   3. 方向键导航（依赖 flat 索引与固定列数）。
 *
 * 上传走 `useUploadIconMutation`：回包成对给 `icon`（令牌）与 `url`（地址），
 * 前端不再自己拼前缀 —— 拼错的表現是"上传成功但保存后图标不显示"。
 */
export function IconLibrary({ value, onPick, onClear, className }: IconLibraryProps) {
  const [keyword, setKeyword] = useState("");
  /** roving tabindex 的当前格（网格里只有一个格子是 Tab 停靠点） */
  const [activeIdx, setActiveIdx] = useState(0);
  const searchRef = useRef<HTMLInputElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const cellRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const icons = useAdminIconsQuery();
  const upload = useUploadIconMutation();

  const builtin = icons.data?.builtin ?? [];
  const uploaded = icons.data?.uploaded ?? [];
  const kw = keyword.trim().toLowerCase();

  // 分组与扁平顺序**一次算出来**：方向键按 flat 的索引移动，
  // 若两处各算一份，顺序稍有出入就会出现「方向键跳到看不见的格子」。
  const { sections, flat } = useMemo(() => {
    const secs: LibSection[] = [];
    let n = 0;
    const push = (key: string, label: string, items: LibItem[]): void => {
      secs.push({ key, label, start: n, items });
      n += items.length;
    };

    // 搜索时隐藏「已上传」：那不是按关键词筛出来的结果，混在里面会让人以为
    // 这些图命中了他搜的词。
    if (kw === "" && uploaded.length) {
      push(
        "__uploaded",
        UPLOADED_LABEL,
        uploaded.map((u) => {
          const dt = formatDateTime(u.created_at * 1000);
          return {
            token: u.icon,
            url: u.url,
            name: "自定义图片",
            // 全是「自定义图片」四个字的话，四个格子长得一模一样、读屏也读不出区别；
            // 上传日期是唯一能区分它们的信息（同一张图重复上传会被 sha256 去重）。
            label: dt.slice(5, 10),
            title: `自定义图片 · ${dt}`,
          };
        }),
      );
    }

    const hit: BrandIconItem[] = builtin.filter(
      (it) =>
        !kw ||
        it.key.includes(kw) ||
        it.name.toLowerCase().includes(kw) ||
        it.aliases.some((a) => a.toLowerCase().includes(kw)),
    );
    const known = new Set(GROUP_ORDER);
    const extras = Array.from(new Set(hit.map((i) => i.group))).filter((g) => !known.has(g));
    for (const g of [...GROUP_ORDER, ...extras]) {
      const items = hit.filter((i) => i.group === g);
      if (!items.length) continue;
      push(
        g,
        GROUP_LABELS[g] ?? g,
        items.map((it) => ({
          token: builtinIconToken(it.key),
          url: it.url,
          name: it.name,
          label: it.name,
          title: it.name,
        })),
      );
    }

    return { sections: secs, flat: secs.flatMap((s) => s.items) };
  }, [builtin, uploaded, kw]);

  // 过滤后长度会变，activeIdx 可能越界；渲染时夹一次即可，不必用 effect 回写。
  const active = Math.min(activeIdx, Math.max(flat.length - 1, 0));

  /** 聚焦第 i 格并把它设为 Tab 停靠点。 */
  const focusCell = (i: number): void => {
    setActiveIdx(i);
    cellRefs.current[i]?.focus();
  };

  const onSearchKeyDown = (e: KeyboardEvent<HTMLInputElement>): void => {
    // 从搜索框下移进网格（↑ 在网格首行会回到这里，形成闭环）
    if (e.key === "ArrowDown" && flat.length) {
      e.preventDefault();
      focusCell(0);
    }
  };

  const onGridKeyDown = (e: KeyboardEvent<HTMLDivElement>): void => {
    const len = flat.length;
    if (!len) return;
    let next: number;
    switch (e.key) {
      case "ArrowRight":
        next = Math.min(active + 1, len - 1);
        break;
      case "ArrowLeft":
        next = Math.max(active - 1, 0);
        break;
      case "ArrowDown":
        next = Math.min(active + COLS, len - 1);
        break;
      case "ArrowUp":
        if (active - COLS < 0) {
          e.preventDefault();
          searchRef.current?.focus();
          return;
        }
        next = active - COLS;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = len - 1;
        break;
      default:
        return;
    }
    e.preventDefault();
    focusCell(next);
  };

  const onPickFile = () => fileRef.current?.click();

  const onFileChange = async (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    // 清空 value：同一个文件连续选两次也要能触发 change，否则"再传一次同一张图"没反应。
    e.target.value = "";
    if (!file) return;
    try {
      const res = await upload.mutateAsync(file);
      if (!res.icon) {
        toast.error("上传结果异常，请重试");
        return;
      }
      onPick(res.icon, res.url);
      toast.success("图标已上传");
    } catch (err) {
      // 后端的文案本身就是可照做的（"图标不能超过 512KB" / "图标边长不能超过 1024px" /
      // "只支持 PNG / JPEG / WebP"），前端不再自造一套判断——两套规则迟早给出不同的答案。
      toast.error(err instanceof Error ? err.message : "上传失败，请重试");
    }
  };

  // 只有"正在搜索且确实没有结果"才提示无匹配：空搜索词下列表本来就是全量，
  // 显示「没有匹配的图标」会让人以为图标库是空的。
  const noMatch = !icons.isLoading && !icons.isError && kw !== "" && flat.length === 0;

  return (
    <div className={cn("ipop", className)}>
      <div className="ipop-head">        <Input
          ref={searchRef}
          type="search"
          className="ipop-search"
          value={keyword}
          placeholder="搜索图标"
          aria-label="搜索图标"
          autoFocus
          onChange={(e) => {
            setKeyword(e.target.value);
            // 换了关键词就回到第一格：否则 Tab 停靠点停在一个已经消失的索引上。
            setActiveIdx(0);
          }}
          onKeyDown={onSearchKeyDown}
        />
        <button type="button" className="btn btn-ghost btn-sm" onClick={onPickFile} disabled={upload.isPending}>
          {upload.isPending ? "上传中…" : "上传图片"}
        </button>
        <input
          ref={fileRef}
          className="ipop-file"
          type="file"
          accept="image/png,image/jpeg,image/webp"
          onChange={onFileChange}
        />
      </div>

      {/* 唯一滚动容器：滚动条不能挂在 .ipop 上，否则会切掉圆角 */}
      <div className="ipop-body" role="group" aria-label="图标库" onKeyDown={onGridKeyDown}>
        {icons.isLoading && <div className="ipop-empty">加载中…</div>}
        {icons.isError && (
          <div className="ipop-empty">
            {/* 直出后端原话：存储不可用 / 未登录这类原因只有后端知道 */}
            <span>{icons.error instanceof Error ? icons.error.message : "图标库加载失败"}</span>
            <button type="button" className="btn btn-ghost btn-sm ipop-retry" onClick={() => void icons.refetch()}>
              重试
            </button>
          </div>
        )}
        {sections.map((sec) => (
          <div key={sec.key} className="ipop-sec">
            <div className="ipop-sec-t">{sec.label}</div>
            <div className="ipop-grid">
              {sec.items.map((it, i) => {
                const idx = sec.start + i;
                const on = it.token === value;
                return (
                  <button
                    key={`${sec.key}:${it.token}`}
                    ref={(el) => {
                      cellRefs.current[idx] = el;
                    }}
                    type="button"
                    className={cn("ipop-cell", on && "is-on")}
                    aria-pressed={on}
                    title={it.title}
                    tabIndex={idx === active ? 0 : -1}
                    onClick={() => onPick(it.token, it.url)}
                    onFocus={() => setActiveIdx(idx)}
                  >
                    <EntityIcon url={it.url} name={it.name} size="md" />
                    <span className="ipop-cell-t">{it.label}</span>
                  </button>
                );
              })}
            </div>
          </div>
        ))}
        {noMatch && <div className="ipop-empty">没有匹配的图标</div>}
      </div>

      <div className="ipop-foot">
        <button type="button" className="btn btn-ghost btn-sm" onClick={onClear}>
          使用首字
        </button>
      </div>
    </div>
  );
}
