import { useEffect, useRef, useState } from "react";

import { Drawer } from "@/components/common/Drawer";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useCreateAppNoticeMutation, useUpdateAppNoticeMutation } from "@/lib/api";
import type { AdminAppNotice, AppNoticePayload, AppNoticeUpdatePayload } from "@/types";
import { hasError, runeLen, toISO, validateNotice, type FormErrors, type NoticeForm } from "../validation";

/** RFC3339 → `datetime-local` 的值（浏览器本地时区，不带 Z）。 */
function toLocalInput(iso: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function draftOf(row: AdminAppNotice | null): NoticeForm {
  return {
    title: row?.title ?? "",
    content: row?.content ?? "",
    level: row?.level ?? "info",
    enabled: row?.enabled ?? true,
    startsAt: toLocalInput(row?.starts_at ?? null),
    endsAt: toLocalInput(row?.ends_at ?? null),
  };
}

/**
 * 公告编辑抽屉。
 *
 * `portal` 必须开：它挂在管理弹窗内部，而 `.dl-body` 是 overflow:auto 且有
 * transform —— 不 portal 会被直接裁掉。
 */
export function NoticeEditorDrawer({
  appId,
  open,
  row,
  onClose,
  onCreated,
}: {
  appId: number;
  open: boolean;
  /** null = 新建 */
  row: AdminAppNotice | null;
  onClose: () => void;
  /** 新建成功（列表需要回到第 1 页，否则新行看不见） */
  onCreated?: () => void;
}) {
  return (
    <Drawer open={open} onClose={onClose} portal title={row ? "编辑公告" : "新建公告"}>
      <NoticeForm appId={appId} open={open} row={row} onClose={onClose} onCreated={onCreated} />
    </Drawer>
  );
}

function NoticeForm({
  appId,
  open,
  row,
  onClose,
  onCreated,
}: {
  appId: number;
  open: boolean;
  row: AdminAppNotice | null;
  onClose: () => void;
  onCreated?: () => void;
}) {
  const [form, setForm] = useState<NoticeForm>(() => draftOf(row));
  const [errors, setErrors] = useState<FormErrors>({});
  const create = useCreateAppNoticeMutation();
  const update = useUpdateAppNoticeMutation();
  const busy = create.isPending || update.isPending;

  // 每次「打开」重建草稿。Drawer 关闭只加 hidden class、不卸载子树，
  // 不重置的话：新建第二条会带着上一条的标题正文；点了取消再进来还是脏草稿。
  // 用边沿检测而不是 [row] 依赖：否则列表 refetch 换了对象会冲掉正在输入的内容。
  const wasOpen = useRef(open);
  useEffect(() => {
    if (open && !wasOpen.current) {
      setForm(draftOf(row));
      setErrors({});
    }
    wasOpen.current = open;
  }, [open, row]);

  const set = <K extends keyof NoticeForm>(k: K, v: NoticeForm[K]) =>
    setForm((prev) => ({ ...prev, [k]: v }));

  const submit = () => {
    const e = validateNotice(form);
    setErrors(e);
    if (hasError(e)) return;
    const base = {
      title: form.title.trim(),
      content: form.content.trim(),
      level: form.level,
      starts_at: toISO(form.startsAt),
      ends_at: toISO(form.endsAt),
    };
    const onError = (err: unknown) => toastApiError(err, "保存失败");
    if (row) {
      // 更新不带 enabled：上下架由列表开关走 PATCH，混进 PUT 会覆盖别人的处置。
      const payload: AppNoticeUpdatePayload = base;
      update.mutate(
        { id: appId, nid: row.id, payload },
        {
          onSuccess: () => {
            toastOk("公告已保存");
            onClose();
          },
          onError,
        },
      );
      return;
    }
    const payload: AppNoticePayload = { ...base, enabled: form.enabled };
    create.mutate(
      { id: appId, payload },
      {
        onSuccess: () => {
          toastOk("公告已创建");
          onCreated?.();
          onClose();
        },
        onError,
      },
    );
  };

  return (
    <div className="tab-panel">
      <Field
        label="标题"
        req
        htmlFor="nt-title"
        labelExtra={<span className="small muted">{runeLen(form.title)}/100</span>}
        hint={errors.title}
        hintWarn={!!errors.title}
      >
        <Input
          id="nt-title"
          value={form.title}
          onChange={(e) => set("title", e.target.value)}
          placeholder="例如：11 月 12 日 02:00 起网关例行升级"
        />
      </Field>

      <div className="form-grid">
        <Field label="级别" htmlFor="nt-level">
          <Select
            id="nt-level"
            value={form.level}
            onChange={(e) => set("level", e.target.value as NoticeForm["level"])}
            items={[
              ["info", "普通"],
              ["warn", "提醒"],
              ["critical", "重要"],
            ]}
          />
        </Field>
        {row ? (
          // 编辑态不提供上下架开关：它与列表里的开关是同一个动作，
          // 放进这里只能靠"保存"生效，而保存是整单提交 —— 正是覆盖的来源。
          <Field label="状态" tip="上下架请在列表里直接切换，改这里不会生效">
            <div className="small muted">{row.enabled ? "已启用" : "已停用"}</div>
          </Field>
        ) : (
          <Field label="状态" tip="停用后不再通过公开接口下发，但历史记录保留">
            <SwitchRow
              label=""
              ariaLabel="启用该公告"
              checked={form.enabled}
              onChange={(v) => set("enabled", v)}
            />
          </Field>
        )}
        <Field
          label="开始时间"
          htmlFor="nt-start"
          hint={errors.startsAt}
          hintWarn={!!errors.startsAt}
          tip="留空 = 立即生效"
        >
          <Input
            id="nt-start"
            type="datetime-local"
            value={form.startsAt}
            onChange={(e) => set("startsAt", e.target.value)}
          />
        </Field>
        <Field
          label="结束时间"
          htmlFor="nt-end"
          hint={errors.endsAt}
          hintWarn={!!errors.endsAt}
          tip="留空 = 长期有效"
        >
          <Input
            id="nt-end"
            type="datetime-local"
            value={form.endsAt}
            onChange={(e) => set("endsAt", e.target.value)}
          />
        </Field>
      </div>

      <Field
        label="内容"
        req
        htmlFor="nt-content"
        labelExtra={<span className="small muted">{runeLen(form.content)}/2000</span>}
        hint={errors.content}
        hintWarn={!!errors.content}
        tip="纯文本：终端渲染环境不可控（WebView/原生），平台不解析 Markdown/HTML"
      >
        <Textarea
          id="nt-content"
          rows={6}
          value={form.content}
          onChange={(e) => set("content", e.target.value)}
          placeholder="升级期间可能出现短暂 5xx，建议客户端重试；已提交的异步任务不受影响。"
        />
      </Field>

      <div className="form-actions">
        <button type="button" className="btn btn-text" onClick={onClose}>
          取消
        </button>
        <button type="button" className="btn btn-primary" disabled={busy} onClick={submit}>
          {busy ? "保存中…" : "保存"}
        </button>
      </div>
    </div>
  );
}
