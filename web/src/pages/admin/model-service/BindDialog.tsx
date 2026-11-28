import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { Field } from "@/components/common/Field";
import { SwitchRow } from "@/components/common/SwitchRow";
import { useAdminModelsQuery, useDeleteModelBindingMutation, useUpsertModelBindingMutation } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import type { AdminModelItem, ChannelItem } from "@/types";

/**
 * 渠道 ↔ 模型的单条绑定（新增 / 修改 / 解除）。
 *
 * 为什么是单条而不是"整组替换"：右栏永远只显示**一条渠道**的模型，
 * 整组接口要求先读全部渠道、改一处、再整组回传 —— 两名管理员同时改同一模型的
 * 不同渠道会互相覆盖，而且整组是硬删重建，其他渠道的权重/分层会被一并压平。
 *
 * 两种入口共用一个组件：
 *  - 行内「绑定设置」：已经知道是哪个模型（`model` 非空）；
 *  - 顶部「绑定已有模型」：先选模型（`model` 为空，走选择器）。
 */
export function BindDialog({
  open,
  channel,
  boundIds,
  model,
  onClose,
}: {
  open: boolean;
  channel: ChannelItem | null;
  /** 该渠道上已有的模型 id 集合（选择器里据此标「已绑定」） */
  boundIds: Set<string>;
  /** 指定模型时直接进入编辑态 */
  model: AdminModelItem | null;
  onClose: () => void;
}) {
  const [target, setTarget] = useState<AdminModelItem | null>(null);
  const [keyword, setKeyword] = useState("");
  const [form, setForm] = useState({ upstream: "", weight: "1", priority: "1", enabled: true });

  useEffect(() => {
    if (!open) return;
    setTarget(model);
    setKeyword("");
    setForm({
      upstream: model?.binding?.upstream_model ?? "",
      weight: String(model?.binding?.weight ?? 1),
      priority: String(model?.binding?.priority ?? 1),
      enabled: model?.binding?.enabled ?? true,
    });
  }, [open, model]);

  // 选择器只在"还没有目标模型"时才请求：已经有目标时再拉一页列表纯属浪费。
  const pick = useAdminModelsQuery(
    useMemo(
      () => ({ keyword: keyword.trim() || undefined, page: 1, page_size: 20 }),
      [keyword],
    ),
    open && !target,
  );

  const upsert = useUpsertModelBindingMutation();
  const remove = useDeleteModelBindingMutation();
  const existing = Boolean(target?.binding);

  const pickModel = (m: AdminModelItem) => {
    setTarget(m);
    setForm({ upstream: "", weight: "1", priority: "1", enabled: true });
  };

  const save = () => {
    if (!channel || !target) return;
    const weight = Number(form.weight);
    const priority = Number(form.priority);
    if (!Number.isFinite(weight) || weight < 1) return toast.error("权重需 ≥ 1");
    if (!Number.isFinite(priority) || priority < 1) return toast.error("分层优先级需 ≥ 1");
    upsert.mutate(
      {
        modelId: target.id,
        channelId: channel.id,
        body: {
          // 留空 = 与本模型同名：后端**不猜**上游叫什么（空值直接 400），
          // 所以"同名"这件事只能由这里补上。
          upstream_model: form.upstream.trim() || target.id,
          weight,
          priority,
          enabled: form.enabled,
        },
      },
      {
        onSuccess: (res) => {
          // 告警是**非阻断**的：绑定已保存，但这条渠道可能承接不了该模型
          // （多协议适配器缺端点 / 模态不匹配）。丢掉它们等于让运营在运行时才发现。
          for (const w of res.warnings ?? []) toast.warning(w.message);
          toast.success(existing ? "绑定已更新" : "已绑定到本渠道");
          onClose();
        },
        onError: (e) => toast.error(e instanceof ApiError ? e.message : "保存失败，请稍后重试"),
      },
    );
  };

  const unbind = async () => {
    if (!channel || !target) return;
    const ok = await confirmDialog({
      title: "解除绑定",
      body: (
        <div>
          <p>
            将把 <b>{target.id}</b> 从渠道 <b>{channel.name}</b> 上解绑。
          </p>
          <p className="muted small">
            模型本身与价格不受影响，其他渠道上的绑定也不受影响。
            {target.bindings <= 1 ? " 这是它唯一的渠道，解绑后将不再对外提供。" : ""}
          </p>
        </div>
      ),
      okText: "解除绑定",
      okVariant: "danger",
    });
    if (!ok) return;
    remove.mutate(
      { modelId: target.id, channelId: channel.id },
      {
        onSuccess: () => {
          toast.success("已解除绑定");
          onClose();
        },
        onError: (e) => toast.error(e instanceof ApiError ? e.message : "解绑失败，请稍后重试"),
      },
    );
  };

  const rows = pick.data?.items ?? [];

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={target ? `绑定 · ${target.id}` : "绑定已有模型"}
      size="lg"
      actions={
        target
          ? [
              ...(existing
                ? [{ label: "解除绑定", onClick: () => void unbind(), disabled: remove.isPending }]
                : []),
              { label: "取消", onClick: onClose },
              {
                label: existing ? "保存" : "绑定",
                variant: "primary" as const,
                disabled: upsert.isPending,
                onClick: save,
              },
            ]
          : [{ label: "关闭", onClick: onClose }]
      }
    >
      {/* 关闭态不渲染内容：弹窗常驻（保留开合动画），而里面的模型选择列表
          与右栏表格是同一批文字 —— 不裁掉的话 `getByText("GPT-4o")` 会命中 2 个，
          读屏也会在关闭的弹窗里念出整张表。FilterBar 的 Sheet 用同一条判据。 */}
      {!open ? null : !channel ? (
        <EmptyState title="请先选择一条渠道" desc="绑定是「模型 × 渠道」的关系，必须先有渠道。" />
      ) : target ? (
        <>
          <div className="muted small">
            模型 <b>{target.id}</b> 在渠道 <b>{channel.name}</b> 上的配置。权重与优先级只影响
            本渠道，不改模型本身与其他渠道。
          </div>
          <Field
            label="上游模型名"
            htmlFor="bd-upstream"
            hint="留空 = 与本模型同名。上游叫法不同时才需要填"
          >
            <input
              id="bd-upstream"
              type="text"
              className="mono"
              placeholder={target.id}
              value={form.upstream}
              onChange={(e) => setForm((f) => ({ ...f, upstream: e.target.value }))}
            />
          </Field>
          <div className="form-grid">
            <Field label="权重" htmlFor="bd-weight" hint="同优先级渠道按权重分摊流量">
              <input
                id="bd-weight"
                type="number"
                min={1}
                value={form.weight}
                onChange={(e) => setForm((f) => ({ ...f, weight: e.target.value }))}
              />
            </Field>
            <Field label="分层优先级" htmlFor="bd-priority" hint="数字小的先被选中；默认 1">
              <input
                id="bd-priority"
                type="number"
                min={1}
                value={form.priority}
                onChange={(e) => setForm((f) => ({ ...f, priority: e.target.value }))}
              />
            </Field>
          </div>
          <SwitchRow
            id="bd-enabled"
            label="启用这条绑定"
            tip="停用后该模型在本渠道上不参与选路，绑定本身保留"
            checked={form.enabled}
            onChange={(v) => setForm((f) => ({ ...f, enabled: v }))}
          />
        </>
      ) : (
        <>
          <Field label="搜索模型" htmlFor="bd-kw">
            <input
              id="bd-kw"
              type="search"
              placeholder="模型 ID 或名称"
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
            />
          </Field>
          {pick.isLoading ? (
            <EmptyState variant="loading" title="加载中…" />
          ) : rows.length === 0 ? (
            <EmptyState
              title="没有匹配的模型"
              desc="换个关键词；需要新模型时，先在本渠道里用「新建模型」建一个（建完即绑定）。"
            />
          ) : (
            <div className="bd-list">
              {rows.map((m) => {
                const bound = boundIds.has(m.id);
                return (
                  <button
                    key={m.id}
                    type="button"
                    className="bd-item"
                    disabled={bound}
                    onClick={() => pickModel(m)}
                  >
                    <EntityCell iconUrl={m.icon_url} name={m.display_name || m.id}>
                      <span className="mono small muted">{m.id}</span>
                    </EntityCell>
                    <span className="right small muted">{bound ? "已绑定" : "可绑定"}</span>
                  </button>
                );
              })}
            </div>
          )}
        </>
      )}
    </Dialog>
  );
}
