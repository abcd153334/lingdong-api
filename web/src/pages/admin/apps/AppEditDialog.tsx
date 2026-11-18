import { useEffect, useRef, useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Textarea } from "@/components/common/Textarea";
import type { AdminAppItem, AppPayload } from "@/types";

import { hasError, runeLen, validateAppMeta, type FormErrors } from "./validation";

/**
 * 编辑应用的基本信息（名称 / 说明 / 自助签发开关）。
 *
 * 标识（slug）不在表单里：它已经写进接入方的部署配置，创建后不可改。
 *
 * 这个入口此前是**缺失**的：后端 `PUT /admin/apps/{id}` 早已实现，
 * 但前端没有任何调用方 —— 后果是「关闭自助签发」一旦点下去就永远改不回来，
 * 而这张卡片会一直挂着"关闭自助签发"的警示徽标。
 */
export function AppEditDialog({
  app,
  open,
  onClose,
  submitting,
  onSubmit,
}: {
  app: AdminAppItem | null;
  open: boolean;
  onClose: () => void;
  submitting: boolean;
  onSubmit: (payload: AppPayload) => void;
}) {
  const [name, setName] = useState(app?.name ?? "");
  const [desc, setDesc] = useState(app?.description ?? "");
  const [allowSelf, setAllowSelf] = useState(app?.allow_self_issue ?? true);
  const [errors, setErrors] = useState<FormErrors>({});

  // 每次打开重建（Dialog 关闭不卸载子树，否则第二次打开是上次的脏值）。
  const wasOpen = useRef(open);
  useEffect(() => {
    if (open && !wasOpen.current) {
      setName(app?.name ?? "");
      setDesc(app?.description ?? "");
      setAllowSelf(app?.allow_self_issue ?? true);
      setErrors({});
    }
    wasOpen.current = open;
  }, [open, app]);

  const close = () => {
    if (!submitting) onClose();
  };

  const submit = () => {
    const next = validateAppMeta({ name, description: desc });
    setErrors(next);
    if (hasError(next)) return;
    // slug 不传：后端忽略该字段，传了也只是噪音。
    onSubmit({ name: name.trim(), description: desc.trim(), allow_self_issue: allowSelf });
  };

  return (
    <Dialog
      open={open}
      onClose={close}
      title="编辑应用信息"
      actions={[
        { label: "取消", onClick: close, disabled: submitting },
        {
          label: submitting ? "保存中…" : "保存",
          variant: "primary",
          disabled: submitting || name.trim() === "",
          onClick: submit,
        },
      ]}
    >
      <div className="form-grid">
        <Field label="应用标识" span tip="创建后不可改：它已写进接入方的部署配置">
          <div className="mono">{app?.slug ?? "—"}</div>
        </Field>
        <Field label="名称" req span hint={errors.name} hintWarn={!!errors.name}>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="给运营看的名字" />
        </Field>
        <Field
          label="说明"
          span
          tip="只用于后台辨识，不对接入方展示"
          hint={errors.description}
          hintWarn={!!errors.description}
          labelExtra={<span className="small muted">{runeLen(desc)}/255</span>}
        >
          <Textarea value={desc} onChange={(e) => setDesc(e.target.value)} rows={3} />
        </Field>
        <Field
          label="自助签发"
          span
          tip="关闭后，任何登录用户都不能再为该应用新签发托管密钥（已签发的仍可用、轮换也不受影响）"
        >
          <div className="link-row">
            <label>
              <input
                type="checkbox"
                checked={allowSelf}
                onChange={(e) => setAllowSelf(e.target.checked)}
              />
              允许用户为自己的账号签发该应用的托管密钥
            </label>
          </div>
        </Field>
      </div>
    </Dialog>
  );
}
