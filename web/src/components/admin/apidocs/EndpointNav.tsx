import { useMemo, useState } from "react";

import { Input } from "@/components/common/Input";
import { cn } from "@/lib/utils";
import type { ApiDocEndpoint } from "@/types";

export interface EndpointNavProps {
  items: ApiDocEndpoint[];
  /** 分组顺序（后端下发，即契约声明顺序） */
  groups: string[];
  /** 当前选中端点的 key */
  activeKey: string;
  onPick: (key: string) => void;
}

/**
 * 左栏目录：分组 + 端点。
 *
 * 为什么搜索框在目录里而不是页头：这一页的检索对象是「41 条里的某一条」，
 * 搜索的作用是**缩小目录**，不是筛选正文。放在页头会让用户以为筛选会影响
 * 别的区域（本页没有别的区域），而他实际只想要一个更短的列表。
 *
 * 过滤在客户端做：41 条全量已随目录一次拉回（后端刻意没开单条详情端点），
 * 再打一次接口只会多一次往返与一个 loading 态。
 */
export function EndpointNav({ items, groups, activeKey, onPick }: EndpointNavProps) {
  const [q, setQ] = useState("");

  const byGroup = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const hit = needle
      ? items.filter((e) => `${e.method} ${e.path} ${e.title} ${e.key}`.toLowerCase().includes(needle))
      : items;
    // 按 groups 的顺序输出，组内保持契约顺序；空组直接不渲染。
    const buckets = new Map<string, ApiDocEndpoint[]>();
    for (const g of groups) buckets.set(g, []);
    for (const e of hit) {
      const arr = buckets.get(e.group);
      // 分组来自后端，理论上不会缺；缺了也照样显示（否则这条会静默消失）。
      if (arr) arr.push(e);
      else buckets.set(e.group, [e]);
    }
    return [...buckets.entries()].filter(([, arr]) => arr.length > 0);
  }, [items, groups, q]);

  return (
    <nav className="adx-nav" aria-label="接口目录">
      <div className="adx-find">
        <Input
          value={q}
          onChange={(ev) => setQ(ev.target.value)}
          placeholder="搜索路径或名称"
          aria-label="搜索接口"
        />
      </div>
      {byGroup.length === 0 ? (
        <p className="muted">没有匹配的接口</p>
      ) : (
        byGroup.map(([group, arr]) => (
          <div className="adx-grp" key={group}>
            <div className="h">{group}</div>
            {arr.map((e) => (
              <button
                key={e.key}
                type="button"
                className={cn("adx-item", e.key === activeKey && "on")}
                aria-current={e.key === activeKey ? "true" : undefined}
                onClick={() => onPick(e.key)}
                title={e.title}
              >
                <span className="m">{e.method}</span>
                <span className="p">{e.path.replace(/^\/api\/user/, "") || "/"}</span>
              </button>
            ))}
          </div>
        ))
      )}
    </nav>
  );
}
