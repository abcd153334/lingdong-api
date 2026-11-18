import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearch } from "@tanstack/react-router";

import { useAdminAdaptersQuery, useAdminChannelsQuery } from "@/lib/api";
import { useLooseNavigate } from "@/lib/pageTabs";
import { useIsRailDrawer } from "@/lib/responsive";
import { modelServiceSearch } from "@/lib/searchParams";
import { useLazyOverlay } from "@/components/common/useLazyOverlay";
import { AgentDialog } from "@/components/admin/agent/AgentDialog";
import type { ChannelItem } from "@/types";

import { ChannelForm } from "./model-service/ChannelForm";
import { ModelPanel } from "./model-service/ModelPanel";
import { Rail } from "./model-service/Rail";
import { groupChannels } from "./model-service/grouping";
import { focusOf, nextSelection } from "./model-service/selection";

/**
 * 模型服务（适配器 → 渠道 → 模型，两栏贴边）。
 *
 * 为什么是这个形状：适配器下管渠道、渠道上管模型是真实的数据形状
 * （`channels.adapter_id` 非空、`model_bindings` 把模型挂到渠道），三张页面各自
 * 维护一份筛选与分页只会让"我这条渠道到底能用哪些模型"要跨三个地址才能回答。
 *
 * 四条硬约束：
 *  1. **左栏不做搜索/筛选**（需求原文）：它是导航，不是数据表。搜索只在模型列表；
 *  2. 页面**四面贴边、没有标题行**：`.a-content` 的 24px 内边距由
 *     `model-service.css` 的 `:has()` 逃逸掉，h1 用 `.a11y-only` 满足可访问名门禁；
 *  3. 选中状态进 URL（`?a` / `?c`），搜索与分页不进 —— 前者是"发给同事"的东西，
 *     后者是我的浏览状态；
 *  4. **点适配器不动右栏**：只展开/收起左边。这是本页最容易写错的一条 ——
 *     旧实现顺带把 `?c` 删掉，operator 点一下就看到一整页空白。
 */
export function AdminModelServicePage() {
  const raw = useSearch({ strict: false }) as Record<string, unknown>;
  const search = modelServiceSearch(raw);
  const navigate = useLooseNavigate();
  const drawer = useIsRailDrawer();
  const [railOpen, setRailOpen] = useState(false);

  // 两个列表都是一次拉全量：左栏是导航（要求"点开就有"），分页的导航没有意义。
  // 渠道用 page=0（后端语义 = 全量，上限 5000）；适配器接口本身不分页。
  const adaptersQ = useAdminAdaptersQuery();
  const channelsQ = useAdminChannelsQuery({ page: 0 });

  const adapters = useMemo(() => adaptersQ.data?.items ?? [], [adaptersQ.data]);
  const channels = useMemo(() => channelsQ.data?.items ?? [], [channelsQ.data]);
  // groupChannels 每次都重排一遍；不 memo 的话下面依赖 `model.groups` 的 effect 每帧都跑。
  const model = useMemo(() => groupChannels(adapters, channels), [adapters, channels]);
  const groupIds = useMemo(() => model.groups.map((g) => g.id), [model]);

  /**
   * 刚建好的渠道还不在列表缓存里，先拿它兜住 `?c`。
   *
   * 没有这一层的话：新建成功 → 立刻写 `?c=<新 id>` → 纠偏 effect 看到列表里没有它
   * → 判为悬空清掉 → 用户看到"保存了，右边又回到没选中"。列表刷新后自动清掉。
   */
  const [pending, setPending] = useState<ChannelItem | null>(null);
  const activeChannel =
    channels.find((c) => c.id === search.c) ?? (pending?.id === search.c ? pending : null);
  const activeId = activeChannel?.id;
  const activeAdapterId = activeChannel?.adapter_id;

  /** 展开态是本地 state：URL 的 `?a` 表达不了「全部折叠」 */
  const [openId, setOpenId] = useState("");
  const focusId = focusOf(search.a, activeChannel, groupIds);
  /**
   * 无选中渠道时右栏要展示的适配器：优先**当前展开的那个**，其次 `?a`。
   *
   * 用 openId 而不是只用 focusId：首屏默认展开第一组（左栏已经摊开），
   * 若右栏还写"从左侧选一个适配器"，用户会看到"左边明明展开了、右边却说没选"。
   * 全部折叠且没有选中渠道时才落到根提示。
   */
  const activeGroup = useMemo(() => {
    if (activeChannel) return null;
    const id = openId || focusId;
    return id ? (model.groups.find((g) => g.id === id) ?? null) : null;
  }, [activeChannel, openId, focusId, model.groups]);

  const go = useCallback(
    (next: { a?: string; c?: number }) => {
      // 只写 a/c，且用新对象整体替换：本页只有这两个参数，spread 旧值会把
      // 刚判为失效的 c 再带回来。
      void navigate({
        search: () => ({ ...(next.a ? { a: next.a } : {}), ...(next.c ? { c: next.c } : {}) }),
        replace: true,
        resetScroll: false,
      });
    },
    [navigate],
  );

  // ① 默认展开：数据首次到位时落一次。
  //    不能写进 useState 初始化 —— 首帧 channels 是空的，只能算出"全折叠"。
  const inited = useRef(false);
  useEffect(() => {
    if (inited.current || !adaptersQ.isSuccess || !channelsQ.isSuccess) return;
    inited.current = true;
    const def = focusId || model.groups.find((g) => g.channels.length > 0)?.id || "";
    if (def) setOpenId(def);
  }, [adaptersQ.isSuccess, channelsQ.isSuccess, focusId, model.groups]);

  // ② 深链 `?a`：只对「从空变非空」响应。
  //    折叠时 a 会从 A 变成 undefined，按值同步会立刻把组重新展开 —— 折叠永远不生效。
  const prevA = useRef(search.a);
  useEffect(() => {
    if (search.a && search.a !== prevA.current) setOpenId(search.a);
    prevA.current = search.a;
  }, [search.a]);

  // ③ 选中变化 → 展开其所属组（点渠道 / 深链 ?c / 新建后选中）
  useEffect(() => {
    if (activeAdapterId) setOpenId(activeAdapterId);
  }, [activeId, activeAdapterId]);

  // ④ 纠偏三件事。必须等渠道列表成功：首帧 channels=[] 会把合法的深链 `?c` 当场清掉。
  useEffect(() => {
    if (!channelsQ.isSuccess || pending) return;
    // (a) ?c 指向已不存在的渠道 → 清 c（保留 a，右栏落回该适配器的总览）
    if (search.c && !activeId) {
      go({ a: search.a });
      return;
    }
    // (b) ?a 不在适配器清单里 → 清 a
    if (search.a && !groupIds.includes(search.a)) {
      go({ c: search.c });
      return;
    }
    // (c) a 与 c 所属适配器不一致 → 以 c 为准回填 a。
    //     必须带 `search.a` 守卫：折叠含选中渠道的组时 a 被清空、c 保留，
    //     少了这个守卫，刚折叠就会被回填成 a=A 并由 effect ② 重新展开。
    if (search.a && activeAdapterId && search.a !== activeAdapterId) {
      go({ a: activeAdapterId, c: search.c });
    }
  }, [channelsQ.isSuccess, search.a, search.c, activeId, activeAdapterId, groupIds, pending, go]);

  // pending 进入真实列表后立刻退场，否则它会一直盖住服务端的最新数据
  useEffect(() => {
    if (pending && channels.some((c) => c.id === pending.id)) setPending(null);
  }, [channels, pending]);

  const toggleAdapter = useCallback(
    (id: string) => {
      const g = model.groups.find((x) => x.id === id);
      const res = nextSelection(
        { open: openId, channel: activeChannel },
        { kind: "toggle", id, channelIds: (g?.channels ?? []).map((c) => c.id) },
      );
      setOpenId(res.open);
      go(res);
    },
    [model.groups, openId, activeChannel, go],
  );

  const selectChannel = useCallback(
    (ch: ChannelItem) => {
      const res = nextSelection(
        { open: openId, channel: activeChannel },
        { kind: "select", channel: ch, knownAdapters: groupIds },
      );
      setOpenId(res.open);
      go(res);
      // 抽屉里点完就收：否则右栏被遮住，用户还得再点一次遮罩
      setRailOpen(false);
    },
    [openId, activeChannel, groupIds, go],
  );

  const channelDeleted = useCallback(
    (adapterId: string) => {
      const res = nextSelection({ open: openId, channel: activeChannel }, { kind: "delete", adapterId });
      setOpenId(res.open);
      go(res);
    },
    [openId, activeChannel, go],
  );

  /** 新建渠道成功：直接落到它（否则用户看到"保存了，右边没变"）。 */
  const channelCreated = useCallback(
    (ch: ChannelItem) => {
      setPending(ch);
      const res = nextSelection(
        { open: openId, channel: null },
        { kind: "select", channel: ch, knownAdapters: groupIds },
      );
      setOpenId(res.open);
      go(res);
    },
    [openId, groupIds, go],
  );

  const agent = useLazyOverlay();

  /**
   * 渠道表单的目标。
   *
   * 抽屉常驻（不随 open 卸载），所以用 `open` + 目标对象表达，而不是"打开就挂载"：
   * 后者会丢掉关闭动画，也会让"改到一半切到另一条"变成两次挂载。
   */
  const [chanForm, setChanForm] = useState<{
    open: boolean;
    channel: ChannelItem | null;
    adapterId?: string;
  }>({ open: false, channel: null });

  const openCreateChannel = useCallback(
    (adapterId: string) => setChanForm({ open: true, channel: null, adapterId }),
    [],
  );
  const openEditChannel = useCallback(
    (ch: ChannelItem) => setChanForm({ open: true, channel: ch }),
    [],
  );
  const closeChannelForm = useCallback(() => setChanForm((f) => ({ ...f, open: false })), []);

  return (
    <div className="ms-ws" data-rail={railOpen ? "open" : undefined}>
      {/* 视觉上没有标题行；h1 保留给读屏与 layout 门禁（每页必须有可见 h1）。 */}
      <h1 className="a11y-only">模型服务</h1>

      <Rail
        model={model}
        openId={openId}
        focusId={focusId}
        activeChannel={activeId ?? 0}
        loading={adaptersQ.isLoading || channelsQ.isLoading}
        error={adaptersQ.error ?? channelsQ.error}
        onRetry={() => {
          void adaptersQ.refetch();
          void channelsQ.refetch();
        }}
        drawer={drawer}
        drawerOpen={railOpen}
        onCloseDrawer={() => setRailOpen(false)}
        onToggleAdapter={toggleAdapter}
        onSelectChannel={selectChannel}
        onCreateChannel={openCreateChannel}
        onCreateAdapter={agent.openOverlay}
      />

      <ModelPanel
        channel={activeChannel}
        group={activeGroup}
        onEditChannel={openEditChannel}
        onChannelDeleted={channelDeleted}
        onSelectChannel={selectChannel}
        onCreateChannel={openCreateChannel}
        onOpenRail={() => setRailOpen(true)}
      />

      <ChannelForm
        open={chanForm.open}
        channel={chanForm.channel}
        channels={channels}
        adapters={adapters}
        // 分组清单只影响"可用分组"多选。首屏可能还没有渠道（groups 为空），
        // 此时至少给 default —— 后端也只保证这个分组一定存在。
        groups={channelsQ.data?.groups ?? ["default"]}
        defaultAdapterId={chanForm.adapterId}
        onClose={closeChannelForm}
        onCreated={channelCreated}
      />

      {/* 懒挂载：首次点「新建适配器」才挂载，之后关闭只切 open —— 会话与草稿都留着。 */}
      {agent.mounted ? <AgentDialog open={agent.open} onClose={agent.closeOverlay} /> : null}
    </div>
  );
}
