import { useMemo, useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { SchemaForm } from "@/components/plugins/SchemaForm";
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
 *  - `idPrefix` 由调用方给：多个插件面板同时保活时，固定字面量会让字段 id 重复
 *    （`getByLabel` 命中两份、DOM id 唯一门禁也会红）。
 */
export function RecordDialog({
  entity,
  idPrefix,
  open,
  record,
  submitting,
  problems,
  onClose,
  onSubmit,
  readOnly = false,
}: {
  entity: PluginEntity;
  idPrefix: string;
  open: boolean;
  record: AdminAppPluginRecord | null;
  submitting: boolean;
  /** 只读详情：同一套布局，但不给保存入口（字段全部 disabled）。 */
  readOnly?: boolean;
  /** 服务端 problems（path 形如 data.xxx） */
  problems: { path: string; message: string }[];
  onClose: () => void;
  /**
   * `keepOpen` 来自「保存并继续」：新建成功后不关窗、清空表单再填下一条。
   * 宿主靠重挂本组件（换 key）实现清空 —— 这里不自己 reset，避免两套清空逻辑。
   */
  onSubmit: (payload: Record<string, unknown>, opts?: { keepOpen?: boolean }) => void;
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
      title={readOnly ? `查看${entity.name}` : `${record ? "编辑" : "新建"}${entity.name}`}
      closeButton
      portal
      layer={2}
      actions={
        readOnly
          ? [{ label: "关闭", onClick: onClose }]
          : [
              { label: "取消", onClick: onClose },
              // 「保存并继续」只在新建时给：编辑一条已存在的记录，继续填"下一条"
              // 是另一件事（等于新建），放在同一个窗口里会让人以为在改当前这条。
              ...(record
                ? []
                : [
                    {
                      label: "保存并继续",
                      disabled: submitting,
                      onClick: () => onSubmit(toPayload(entity, values), { keepOpen: true }),
                    },
                  ]),
              {
                label: submitting ? "保存中…" : "保存",
                variant: "primary" as const,
                disabled: submitting,
                onClick: () => onSubmit(toPayload(entity, values)),
              },
            ]
      }
    >
      {readOnly && record ? (
        // 编号与版本是排障要用的，普通表单里看不到它们（不在字段表里）。
        <p className="muted small plugin-view__meta">
          编号 <span className="mono">{record.record_key}</span> · 版本 {record.version} · 更新于{" "}
          {record.updated_at}
        </p>
      ) : null}
      <SchemaForm
        entity={entity}
        values={values}
        errors={errors}
        idPrefix={idPrefix}
        disabled={submitting || readOnly}
        onChange={setField}
      />
    </Dialog>
  );
}
