import { useEffect, useState } from "react";
import { toast } from "sonner";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Field } from "@/components/common/Field";
import { IconNameField } from "@/components/common/IconNameField";
import { Tag } from "@/components/common/StatusTag";
import {
  useAdminVendorsQuery,
  useCreateAdminVendorMutation,
  useDeleteAdminVendorMutation,
  useUpdateAdminVendorMutation,
} from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { toastApiError } from "@/lib/apiToast";
import { cn } from "@/lib/utils";
import type { VendorItem } from "@/types";

/** 名称上限（rune）：后端 `admin_model.go` 的 12 rune 校验是最终判据，这里只是提前拦。 */
const NAME_MAX = 12;

interface Draft {
  name: string;
  /** 图标 token（提交用） */
  icon: string;
  /** 该 token 的地址（预览用；新选的由选择器内部记） */
  iconUrl: string;
  color: string;
}

/**
 * 编辑态。
 *
 * 没有单独的 `view` 分支：**初始态就是 `null`**，渲染时按「第一个分组 / 新建」派生。
 * 这样「打开弹窗」天然回到初始态，不需要在某处手工清空五个 state
 * （旧实现就是共用一套 input，靠手清空，漏一处就把上一个分组的图标带进新建）。
 *
 * 判「有没有改动」也不靠 flag，而是拿当前草稿和**基线**逐字段比：
 * 基线 = 已保存行 或 空白草稿。flag 会在「改了又改回去」时骗人。
 *
 * 基线**跟着 mode 走**（而不是每次从列表行现算）：列表行是缓存快照，
 * 保存成功到 refetch 回来之间还是旧值 —— 现算的话「刚保存完」会被判成
 * 「有未保存改动」，紧接着关闭就会弹「放弃修改？」。回包行本身就是最新的
 * 服务端状态，用它当基线不需要额外等待，也不引入第二份数据。
 */
type Mode =
  | { kind: "create"; base: Draft; draft: Draft }
  | { kind: "edit"; id: string; base: Draft; draft: Draft };

export interface VendorManagerDialogProps {
  open: boolean;
  onClose: () => void;
}

const draftOf = (v: VendorItem): Draft => ({
  name: v.name,
  icon: v.icon,
  iconUrl: v.icon_url,
  color: v.color,
});

/**
 * 供应商分组管理（主从两栏）。
 *
 * 为什么是主从而不是「列表 + 各自编辑按钮」：分组的三个属性（名称/图标/品牌色）
 * 都属于同一个实体，把它们摊在一屏的左选中右编辑，改完一栏顺手切下一个 ——
 * 旧版是「顶部一个共用输入框 + 下面每行一个编辑按钮」，改 A 时输入框里是 B 的名字。
 *
 * 三件事只在后端成立，这里不重复判断：删除保护（内置/在用）、重名（唯一索引）、
 * 品牌色归一（`#RRGGBB` 大写）。前端只做「空名称 / 超长 / 无改动」这三条即时反馈。
 */
export function VendorManagerDialog({ open, onClose }: VendorManagerDialogProps) {
  const vendorsQuery = useAdminVendorsQuery();
  const createVendor = useCreateAdminVendorMutation();
  const updateVendor = useUpdateAdminVendorMutation();
  const deleteVendor = useDeleteAdminVendorMutation();

  const [mode, setMode] = useState<Mode | null>(null);

  // 每次打开都回到初始态：Dialog 关闭只是 display:none，不卸载子树，
  // 状态会原样留到下一次打开（表现得像"数据串了"）。
  useEffect(() => {
    if (open) setMode(null);
  }, [open]);

  const vendors = vendorsQuery.data?.items ?? [];
  const palette = vendorsQuery.data?.palette ?? [];
  const blankDraft = (): Draft => ({ name: "", icon: "", iconUrl: "", color: palette[0] ?? "" });

  // 选中项：mode 没指定时 = 第一个分组；一个分组都没有 = 新建。
  const selected: VendorItem | null =
    mode === null
      ? (vendors[0] ?? null)
      : mode.kind === "create"
        ? null
        : (vendors.find((v) => v.id === mode.id) ?? null);

  const draft: Draft = mode ? mode.draft : selected ? draftOf(selected) : blankDraft();
  // mode 一旦建立，基线就跟着它：初始态（mode === null）才用列表行。
  const baseline: Draft = mode
    ? mode.base
    : selected
      ? draftOf(selected)
      : blankDraft();
  const isCreate = selected === null;
  const dirty =
    draft.name !== baseline.name || draft.icon !== baseline.icon || draft.color !== baseline.color;

  const saving = createVendor.isPending || updateVendor.isPending;
  const name = draft.name.trim();
  const nameTooLong = Array.from(name).length > NAME_MAX;
  const canSave = name !== "" && !nameTooLong && dirty && !saving;

  /** 改草稿：第一次改动时把「派生初始态」落成显式 mode，之后全由 mode 驱动。 */
  const patchDraft = (patch: Partial<Draft>): void => {
    const next = { ...draft, ...patch };
    setMode(
      selected
        ? { kind: "edit", id: selected.id, base: baseline, draft: next }
        : { kind: "create", base: baseline, draft: next },
    );
  };

  const confirmDiscard = (): Promise<boolean> =>
    confirmDialog({
      title: "放弃未保存的修改？",
      body: "切换分组或关闭后，本次改动会丢失。",
      okText: "放弃修改",
      okVariant: "danger",
    });

  const selectVendor = async (v: VendorItem): Promise<void> => {
    if (selected?.id === v.id) return;
    if (dirty && !(await confirmDiscard())) return;
    setMode({ kind: "edit", id: v.id, base: draftOf(v), draft: draftOf(v) });
  };

  const startCreate = async (): Promise<void> => {
    if (isCreate) return;
    if (dirty && !(await confirmDiscard())) return;
    setMode({ kind: "create", base: blankDraft(), draft: blankDraft() });
  };

  const handleClose = async (): Promise<void> => {
    if (dirty && !(await confirmDiscard())) return;
    onClose();
  };

  const submit = (): void => {
    // 三项**全提交**：`icon: ""` 是"清空"（省略则被后端当成"不修改"，永远清不掉），
    // 品牌色同理。省字段换来的只有"改了没保存上"。
    const body = { name, icon: draft.icon, color: draft.color };
    if (isCreate) {
      createVendor.mutate(body, {
        onSuccess: (res) => {
          toast(`分组「${res.item.name}」已创建`);
          setMode({ kind: "edit", id: res.item.id, base: draftOf(res.item), draft: draftOf(res.item) });
        },
        onError: (e) => toastApiError(e),
      });
      return;
    }
    updateVendor.mutate(
      { id: selected.id, body },
      {
        onSuccess: (res) => {
          toast("分组已保存");
          // 用**回包行**同时刷新草稿与基线：不依赖整表 refetch，也就不会出现
          // 「提示保存成功、界面还是旧图标」与「刚保存完就提示有未保存改动」两种中间态。
          setMode({ kind: "edit", id: res.item.id, base: draftOf(res.item), draft: draftOf(res.item) });
        },
        onError: (e) => toastApiError(e),
      },
    );
  };

  const askDelete = async (): Promise<void> => {
    if (!selected || selected.builtin) return;
    const ok = await confirmDialog({
      title: "删除供应商分组",
      body: (
        <>
          确定删除分组 <b>{selected.name}</b>？其下仍有模型的需先在模型编辑中迁移。
        </>
      ),
      okText: "确认删除",
      okVariant: "danger",
    });
    if (!ok) return;
    const target = selected;
    deleteVendor.mutate(target.id, {
      onSuccess: () => {
        toast(`分组「${target.name}」已删除`);
        // 回到初始态 = 自动选下一个分组。列表在 invalidate 后重取，
        // 这一帧里可能还看得见刚删的行（它是已经不在库里的快照），刷新即消失。
        setMode(null);
      },
      onError: (e) => toastApiError(e),
    });
  };

  return (
    <Dialog open={open} onClose={() => void handleClose()} title="供应商分组" size="xl">
      {vendorsQuery.isError ? (
        <EmptyState
          icon="!"
          title="分组加载失败"
          desc={vendorsQuery.error instanceof ApiError ? vendorsQuery.error.message : "请稍后重试"}
        />
      ) : vendorsQuery.isPending ? (
        <EmptyState title="加载中…" />
      ) : (
        <div className="vnd">
          {/* 左栏：分组列表 + 新建 */}
          <div className="vnd-side">
            <div className="vnd-list">
              {vendors.length === 0 ? (
                <div className="vnd-empty">还没有分组</div>
              ) : (
                vendors.map((v) => (
                  <button
                    key={v.id}
                    type="button"
                    className={cn("vnd-item", selected?.id === v.id && "is-on")}
                    disabled={saving}
                    aria-current={selected?.id === v.id}
                    onClick={() => void selectVendor(v)}
                  >
                    <EntityIcon url={v.icon_url} name={v.name} color={v.color} />
                    <span className="vnd-item-main">
                      <span className="vnd-item-name">{v.name}</span>
                      <span className="vnd-item-meta">{v.model_count} 个模型</span>
                    </span>
                    {v.builtin ? <Tag tone="gray">内置</Tag> : null}
                  </button>
                ))
              )}
            </div>
            <button type="button" className="btn btn-ghost vnd-new" disabled={saving} onClick={() => void startCreate()}>
              新建分组
            </button>
          </div>

          {/* 右栏：当前分组的三个属性 */}
          <div className="vnd-main">
            <Field label="名称" htmlFor="vnd-name">
              <IconNameField
                id="vnd-name"
                icon={draft.icon}
                iconUrl={draft.iconUrl}
                onIconChange={(icon) => patchDraft({ icon })}
                color={draft.color}
                initialFrom="分组"
                text={draft.name}
                onTextChange={(v) => patchDraft({ name: v })}
                placeholder={`不超过 ${NAME_MAX} 字`}
                maxLength={NAME_MAX}
              />
            </Field>

            <div className="vnd-row">
              <span className="vnd-label">品牌色</span>
              <div className="vnd-swatches">
                {palette.map((c) => (
                  <button
                    key={c}
                    type="button"
                    className={cn("vnd-swatch", draft.color === c && "is-on")}
                    style={{ background: c }}
                    aria-label={`品牌色 ${c}`}
                    aria-pressed={draft.color === c}
                    onClick={() => patchDraft({ color: c })}
                  />
                ))}
                {/* 自定义色：色盘之外的颜色只能从这里来（原生取色器无法统一样式） */}
                <input
                  type="color"
                  className="vnd-color"
                  aria-label="自定义品牌色"
                  value={draft.color || "#6366F1"}
                  onChange={(e) => patchDraft({ color: e.target.value.toUpperCase() })}
                />
                <span className="vnd-hex">{draft.color || "未设置"}</span>
              </div>
            </div>

            <div className="vnd-meta">
              {isCreate
                ? "新建后可在模型编辑里把模型挂到这个分组"
                : `${selected.builtin ? "内置分组" : "自定义分组"} · ${selected.model_count} 个模型`}
            </div>

            <div className="vnd-acts">
              {!isCreate && !selected.builtin ? (
                <button
                  type="button"
                  className="btn btn-ghost-danger"
                  disabled={saving}
                  onClick={() => void askDelete()}
                >
                  删除
                </button>
              ) : null}
              <button type="button" className="btn btn-primary" disabled={!canSave} onClick={submit}>
                {saving ? "保存中…" : "保存"}
              </button>
            </div>
          </div>
        </div>
      )}
    </Dialog>
  );
}
