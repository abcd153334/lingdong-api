import { useCallback, useEffect, useMemo, useState } from "react";
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
import { PresetRestoreDialog } from "./model-service/PresetRestoreDialog";
import { Rail } from "./model-service/Rail";
import { groupChannels } from "./model-service/grouping";
import { nextState, ORPHANS, type FlyoutOpen } from "./model-service/state";

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
 *  3. 只有「在看哪条渠道」进 URL（`?c`）：它是唯一值得发给同事的状态。
 *     浮层开在哪个适配器是**瞬态**，进 URL 会立刻引出"两处高亮谁优先 / 关抽屉后残留 /
 *     孤儿桶要第三个参数"三个新问题，收益为零；
 *  4. **点适配器不动右栏**：只开浮层。这是本页最容易写错的一条 ——
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
  // groupChannels 每次都重排一遍；不 memo 的话依赖 `model` 的 effect 每帧都跑。
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

  /** 浮层开在哪。桌面 = 二级面板；≤899 复用为"抽屉钻取到哪个适配器"，关抽屉即复位。 */
  const [open, setOpen] = useState<FlyoutOpen>("");

  /** `?c` 已写但渠道还没解析出来（深链首帧）：此时不要先按全量发一次请求。 */
  const channelPending = !!search.c && !activeChannel;

  const go = useCallback(
    (c?: number) => {
      // 只写 c，整体替换 search：本页只有这一个参数，spread 旧值会把刚判为失效的 c 带回来。
      void navigate({
        search: () => (c ? { c } : {}),
        replace: true,
        resetScroll: false,
      });
    },
    [navigate],
  );

  // 纠偏 ①：`?c` 指向已不存在的渠道 → 清掉，回到全部模型。
  // 必须等渠道列表成功：首帧 channels=[] 会把合法的深链当场清掉。
  useEffect(() => {
    if (!channelsQ.isSuccess || pending) return;
    if (search.c && !activeId) go();
  }, [channelsQ.isSuccess, search.c, activeId, pending, go]);

  // 纠偏 ②：浮层指向的适配器已不在清单里（被删/停用后重拉）→ 关掉浮层。
  useEffect(() => {
    if (!open || open === ORPHANS) return;
    if (adaptersQ.isSuccess && !groupIds.includes(open)) setOpen("");
  }, [open, adaptersQ.isSuccess, groupIds]);

  // pending 进入真实列表后立刻退场，否则它会一直盖住服务端的最新数据
  useEffect(() => {
    if (pending && channels.some((c) => c.id === pending.id)) setPending(null);
  }, [channels, pending]);

  /** 关抽屉同时复位钻取：否则下次打开会直接停在渠道列表里，用户以为适配器没了。 */
  const closeDrawer = useCallback(() => {
    setRailOpen(false);
    setOpen("");
  }, []);

  const openRail = useCallback(() => {
    setOpen("");
    setRailOpen(true);
  }, []);

  const toggleAdapter = useCallback(
    (id: string) => {
      // 抽屉里点卡片 = 钻取（同一块地方换内容），不关抽屉
      if (drawer) {
        setOpen((prev) => (prev === id ? "" : id));
        return;
      }
      setOpen((prev) => nextState({ open: prev, c: activeId }, { kind: "toggleAdapter", id }).open);
    },
    [drawer, activeId],
  );

  const toggleOrphans = useCallback(() => {
    setOpen((prev) => nextState({ open: prev, c: activeId }, { kind: "openOrphans" }).open);
  }, [activeId]);

  const showAll = useCallback(() => {
    setOpen("");
    go();
  }, [go]);

  const selectChannel = useCallback(
    (ch: ChannelItem) => {
      // 选中即关：浮层是"选完就走"的导航，留着会压住刚切过去的表格
      setOpen("");
      setRailOpen(false);
      go(ch.id);
    },
    [go],
  );

  /** 渠道删掉了：URL 指向的行已经不存在，直接回全部模型（停在空渠道视图没有意义）。 */
  const channelDeleted = useCallback(() => {
    setOpen("");
    go();
  }, [go]);

  /** 新建渠道成功：直接落到它（否则用户看到"保存了，右边没变"）。 */
  const channelCreated = useCallback(
    (ch: ChannelItem) => {
      setPending(ch);
      setOpen("");
      go(ch.id);
    },
    [go],
  );

  const agent = useLazyOverlay();

  /**
   * 「恢复出厂」弹窗。
   *
   * 打开前先关抽屉与浮层：二者 z-index 低于弹窗，留着会压在遮罩下面变成
   * "看得见但点不动"的假可点区域。
   */
  const [restoreOpen, setRestoreOpen] = useState(false);
  const openRestore = useCallback(() => {
    setOpen("");
    setRailOpen(false);
    setRestoreOpen(true);
  }, []);
  const closeRestore = useCallback(() => setRestoreOpen(false), []);

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

  /**
   * 任何"要开弹层"的动作都必须**先关浮层**。
   *
   * `--z-pop`(70) 高于抽屉/弹窗(60)，而页面层的 `useOverlayOpen()` 恒为 true
   * （Provider 只在 Dialog/Drawer 内部），浮层不会自动让位 —— 不关就会盖在
   * 刚打开的抽屉上面。
   */
  const openCreateChannel = useCallback((adapterId: string) => {
    setOpen("");
    setRailOpen(false);
    setChanForm({ open: true, channel: null, adapterId });
  }, []);
  const openEditChannel = useCallback((ch: ChannelItem) => {
    setOpen("");
    setChanForm({ open: true, channel: ch });
  }, []);
  const onCreateAdapter = useCallback(() => {
    setOpen("");
    setRailOpen(false);
    agent.openOverlay();
  }, [agent]);
  const closeChannelForm = useCallback(() => setChanForm((f) => ({ ...f, open: false })), []);

  return (
    <div className="ms-ws" data-rail={railOpen ? "open" : undefined}>
      {/* 视觉上没有标题行；h1 保留给读屏与 layout 门禁（每页必须有可见 h1）。 */}
      <h1 className="a11y-only">模型服务</h1>

      <Rail
        model={model}
        open={open}
        activeChannel={activeChannel}
        loading={adaptersQ.isLoading || channelsQ.isLoading}
        error={adaptersQ.error ?? channelsQ.error}
        onRetry={() => {
          void adaptersQ.refetch();
          void channelsQ.refetch();
        }}
        drawer={drawer}
        drawerOpen={railOpen}
        onCloseDrawer={closeDrawer}
        onToggleAdapter={toggleAdapter}
        onToggleOrphans={toggleOrphans}
        onShowAll={showAll}
        onSelectChannel={selectChannel}
        onCreateChannel={openCreateChannel}
        onCreateAdapter={onCreateAdapter}
        onDrillBack={() => setOpen("")}
        onRestorePresets={openRestore}
      />

      <ModelPanel
        channel={activeChannel}
        channelPending={channelPending}
        onEditChannel={openEditChannel}
        onChannelDeleted={channelDeleted}
        onOpenRail={openRail}
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

      <PresetRestoreDialog open={restoreOpen} onClose={closeRestore} />
    </div>
  );
}
