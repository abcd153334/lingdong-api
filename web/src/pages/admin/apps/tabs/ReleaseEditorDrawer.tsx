import { useEffect, useRef, useState } from "react";

import { Drawer } from "@/components/common/Drawer";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useCreateAppReleaseMutation, useUpdateAppReleaseMutation } from "@/lib/api";
import type { AdminAppRelease, AppReleasePayload } from "@/types";
import { hasError, runeLen, validateRelease, type FormErrors, type ReleaseForm } from "../validation";

/**
 * 版本编辑抽屉。
 *
 * 新建一律落草稿：发布是独立动作。已发布的版本**不允许改版本号** ——
 * 终端上报的"我装的是 2.3.0"一旦与库里的记录对不上，就再也无法排查
 * "用户到底装的是哪个包"。
 */
export function ReleaseEditorDrawer({
  appId,
  open,
  row,
  onClose,
  onCreated,
}: {
  appId: number;
  open: boolean;
  /** null = 新建 */
  row: AdminAppRelease | null;
  onClose: () => void;
  /** 新建成功（列表需要回到第 1 页） */
  onCreated?: () => void;
}) {
  return (
    <Drawer open={open} onClose={onClose} portal title={row ? "编辑版本" : "新建版本"}>
      <ReleaseForm appId={appId} open={open} row={row} onClose={onClose} onCreated={onCreated} />
    </Drawer>
  );
}

function ReleaseForm({
  appId,
  open,
  row,
  onClose,
  onCreated,
}: {
  appId: number;
  open: boolean;
  row: AdminAppRelease | null;
  onClose: () => void;
  onCreated?: () => void;
}) {
  const [form, setForm] = useState<ReleaseForm>({
    version: row?.version ?? "",
    releaseNotes: row?.release_notes ?? "",
    downloadURL: row?.download_url ?? "",
    forceUpdate: row?.force_update ?? false,
  });
  // 每次打开重建草稿：否则"新建版本"第二次打开还带着刚才那版的版本号，
  // 一保存必撞 `app_release_version_exists`，看起来像后端拒绝。
  const wasOpen = useRef(open);
  useEffect(() => {
    if (open && !wasOpen.current) {
      setForm({
        version: row?.version ?? "",
        releaseNotes: row?.release_notes ?? "",
        downloadURL: row?.download_url ?? "",
        forceUpdate: row?.force_update ?? false,
      });
    }
    wasOpen.current = open;
  }, [open, row]);
  const [errors, setErrors] = useState<FormErrors>({});
  const create = useCreateAppReleaseMutation();
  const update = useUpdateAppReleaseMutation();
  const busy = create.isPending || update.isPending;
  // 草稿之外不能改版本号（后端同样会拒，前端先禁掉避免白提交一次）。
  const versionLocked = row !== null && row.status !== 1;

  const set = <K extends keyof ReleaseForm>(k: K, v: ReleaseForm[K]) =>
    setForm((prev) => ({ ...prev, [k]: v }));

  const submit = () => {
    const e = validateRelease(form);
    setErrors(e);
    if (hasError(e)) return;
    const payload: AppReleasePayload = {
      version: form.version.trim(),
      release_notes: form.releaseNotes.trim(),
      download_url: form.downloadURL.trim(),
      force_update: form.forceUpdate,
    };
    const onError = (err: unknown) => toastApiError(err, "保存失败");
    if (row) {
      update.mutate(
        { id: appId, rid: row.id, payload },
        { onSuccess: () => { toastOk("版本已保存"); onClose(); }, onError },
      );
      return;
    }
    create.mutate(
      { id: appId, payload },
      {
        onSuccess: () => {
          toastOk("已保存为草稿，确认无误后再发布");
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
        label="版本号"
        req
        htmlFor="rl-version"
        hint={errors.version}
        hintWarn={!!errors.version}
        tip={
          versionLocked
            ? "该版本已对外发布过，版本号不可修改"
            : "形如 1.2.3，可带 -beta.1 后缀；同一应用内唯一"
        }
      >
        <Input
          id="rl-version"
          value={form.version}
          disabled={versionLocked}
          onChange={(e) => set("version", e.target.value)}
          placeholder="2.4.0"
        />
      </Field>

      <Field
        label="下载地址"
        req
        htmlFor="rl-url"
        hint={errors.downloadURL}
        hintWarn={!!errors.downloadURL}
        tip="必须是 http(s) 绝对地址：终端与平台不同源，相对地址必然 404"
      >
        <Input
          id="rl-url"
          value={form.downloadURL}
          onChange={(e) => set("downloadURL", e.target.value)}
          placeholder="https://download.example.com/app-2.4.0.exe"
        />
      </Field>

      <Field
        label="更新说明"
        req
        htmlFor="rl-notes"
        labelExtra={<span className="small muted">{runeLen(form.releaseNotes)}/4000</span>}
        hint={errors.releaseNotes}
        hintWarn={!!errors.releaseNotes}
      >
        <Textarea
          id="rl-notes"
          rows={6}
          value={form.releaseNotes}
          onChange={(e) => set("releaseNotes", e.target.value)}
          placeholder="修复批量下载偶发失败；新增应用级公告能力。"
        />
      </Field>

      <Field label="强制更新" tip="开启后终端必须升级才能继续使用；谨慎开启，它会阻断旧版本的全部用户">
        <SwitchRow
          label="强制更新"
          checked={form.forceUpdate}
          onChange={(v) => set("forceUpdate", v)}
        />
      </Field>

      <div className="form-actions">
        <button type="button" className="btn btn-text" onClick={onClose}>
          取消
        </button>
        <button type="button" className="btn btn-primary" disabled={busy} onClick={submit}>
          {busy ? "保存中…" : row ? "保存" : "保存草稿"}
        </button>
      </div>
    </div>
  );
}
