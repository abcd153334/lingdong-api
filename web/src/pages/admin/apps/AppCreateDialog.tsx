import { useState } from "react";

import { Dialog } from "@/components/common/Dialog";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Textarea } from "@/components/common/Textarea";
import type { AppPayload } from "@/types";

import { hasError, runeLen, validateApp, type FormErrors } from "./validation";

/**
 * 新建应用。
 *
 * 只收「身份字段」：品牌、公告、版本都在创建后的管理弹窗里配。
 * 创建时就要求填品牌会让"先建个空应用占住标识"这个高频动作变重。
 *
 * 表单状态是**每次打开重建**的：Dialog 关闭只给节点加 hidden class、不卸载子树，
 * 所以残留的表单会让第二次"新建"带着上一次的 slug，一点创建就撞
 * `app_slug_exists` —— 而应用没有删除入口，运营只能以为系统坏了。
 * 重建由调用方的 key 完成（见 Apps.tsx），组件内不再写 reset effect。
 */
export function AppCreateDialog({
  open,
  onClose,
  submitting,
  onSubmit,
}: {
  open: boolean;
  onClose: () => void;
  submitting: boolean;
  onSubmit: (payload: AppPayload) => void;
}) {
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [desc, setDesc] = useState("");
  // 默认 true：与后端列默认值一致，新建即「自助接入」。
  const [allowSelf, setAllowSelf] = useState(true);
  const [errors, setErrors] = useState<FormErrors>({});

  // 提交中不允许关闭：请求还在飞、框已经关掉，成功 toast 会突然冒出来，
  // 运营无法判断到底建没建成（而重复创建会产生第二个应用）。
  const close = () => {
    if (!submitting) onClose();
  };

  const submit = () => {
    const next = validateApp({ slug, name, description: desc });
    setErrors(next);
    if (hasError(next)) return;
    onSubmit({
      slug: slug.trim(),
      name: name.trim(),
      description: desc.trim(),
      allow_self_issue: allowSelf,
    });
  };

  return (
    <Dialog
      open={open}
      onClose={close}
      title="新建应用"
      actions={[
        { label: "取消", onClick: close, disabled: submitting },
        {
          label: submitting ? "创建中…" : "创建",
          variant: "primary",
          disabled: submitting || name.trim() === "" || slug.trim() === "",
          onClick: submit,
        },
      ]}
    >
      <div className="form-grid">
        <Field
          label="应用标识"
          req
          span
          tip="接入方按它调用签发接口（/apps/{标识}/hosted-key）。创建后不可改，改了等于让已接入的应用全部失效"
          hint={errors.slug}
          hintWarn
        >
          <Input
            value={slug}
            onChange={(e) => setSlug(e.target.value)}
            placeholder="小写字母、数字与连字符，如 my-app"
          />
        </Field>
        <Field label="名称" req span hint={errors.name} hintWarn>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="给运营看的名字" />
        </Field>
        <Field
          label="说明"
          span
          tip="只用于后台辨识，不对接入方展示"
          hint={errors.description}
          hintWarn
          labelExtra={<span className="small muted">{runeLen(desc)}/255</span>}
        >
          <Textarea value={desc} onChange={(e) => setDesc(e.target.value)} rows={3} />
        </Field>
        <Field
          label="自助签发"
          span
          tip="关闭后，任何登录用户都不能再为该应用新签发托管密钥（已签发的仍可用、轮换也不受影响）。只对需要平台审批接入的敏感应用关掉"
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
