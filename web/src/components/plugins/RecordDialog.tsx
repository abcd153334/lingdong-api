import { useMemo, useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { SchemaFields } from "@/components/plugins/SchemaFields";
import { fieldKeyOfPath, toFormValues, toPayload, type FormValues } from "@/lib/pluginTypes";
import type { AdminAppPluginRecord, PluginEntity } from "@/types";

/**
 * 新建/编辑一条记录。
 *
 * 注意外层必须按记录 key 重建本组件（`key={record?.record_key ?? "new"}`）：
 * `Dialog` 关闭时不卸载子树，只有重挂才能让表单回到初始值 —— 否则"编辑 A、关掉、
 * 再新建"会带着 A 的内容（同 Apps.tsx:197 记过的坑）。
 *
 * 两个必传约束（都来自"宿主是应用管理弹窗"）：
 *  - `portal` + `layer={2}`：`.dialog` 带 transform，是 fixed 后代的包含块；不升到 body 时
 *    本弹窗会被定位到外层弹窗盒子里、并被 `.dl-body` 的 `overflow:hidden` 裁掉。
 *  - `idPrefix` 默认 `plugin-record`，由调用方按需覆盖：多个插件面板同时保活时，
 *    固定字面量会让字段 id 重复（`getByLabel` 命中两份、DOM id 唯一门禁也会红）。
 *    之所以留默认值而不是改成必传：调用方（插件独立页）在弹窗改造前就拿不到前缀，
 *    必传会让那次改动只能两处同时落地，任一处落后就编译不过。
 */
export function RecordDialog({
  entity,
  idPrefix = "plugin-record",
  open,
  record,
  submitting,
  problems,
  onClose,
  onSubmit,
}: {
  entity: PluginEntity;
  idPrefix?: string;
  open: boolean;
  record: AdminAppPluginRecord | null;
  submitting: boolean;
  /** 服务端 problems（path 形如 data.xxx） */
  problems: { path: string; message: string }[];
  onClose: () => void;
  onSubmit: (payload: Record<string, unknown>) => void;
}) {
  const [values, setValues] = useState<FormValues>(() => toFormValues(entity.fields, record?.data));
  const errors = useMemo(() => {
    const out: Record<string, string> = {};
    for (const p of problems) {
      const key = fieldKeyOfPath(p.path, "data.");
      // 字段级错误贴到字段下面；`data` 本身或顶层错误留在汇总里（见父页面）。
      if (key) out[key] = p.message;
    }
    return out;
  }, [problems]);

  const setField = (key: string, value: unknown) => setValues((prev) => ({ ...prev, [key]: value }));

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`${record ? "编辑" : "新建"}${entity.name}`}
      closeButton
      portal
      layer={2}
      actions={[
        { label: "取消", onClick: onClose },
        {
          label: submitting ? "保存中…" : "保存",
          variant: "primary",
          disabled: submitting,
          onClick: () => onSubmit(toPayload(entity, values)),
        },
      ]}
    >
      <SchemaFields
        fields={entity.fields}
        values={values}
        errors={errors}
        idPrefix={idPrefix}
        disabled={submitting}
        onChange={setField}
      />
    </Dialog>
  );
}
