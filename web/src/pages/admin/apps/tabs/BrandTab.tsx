import { useEffect, useRef, useState } from "react";

import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { IconPicker } from "@/components/common/IconPicker";
import { Input } from "@/components/common/Input";
import { Textarea } from "@/components/common/Textarea";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useAdminAppBrandQuery, useSaveAppBrandMutation } from "@/lib/api";
import type { AdminAppBrand, AdminAppItem } from "@/types";
import {
  brandFormOf,
  hasError,
  isBrandDirty,
  runeLen,
  validateBrand,
  type BrandForm,
  type FormErrors,
} from "../validation";

/**
 * 品牌信息（终端可见）。
 *
 * 先说清边界：这里填的东西会通过公开接口下发给终端应用，**不含任何经营数据**。
 * 顶部那条提示是刻意留的 —— 运营改之前必须知道"这是对外展示"。
 */
export function BrandTab({
  app,
  onDirtyChange,
}: {
  app: AdminAppItem;
  /** 上报「有未保存修改」，用于页签圆点；切页签不再卸载本面板（见 PageTabs.keepMounted）。 */
  onDirtyChange?: (dirty: boolean) => void;
}) {
  // 只拉品牌 10 个字段（1 条 SQL）：不再为回填表单去等一次近 7 日聚合。
  const q = useAdminAppBrandQuery(app.id);
  if (q.isPending) return <EmptyState variant="loading" title="加载中…" />;
  if (q.isError || !q.data) return <EmptyState variant="error" title="品牌信息加载失败" />;

  // 数据就绪后再挂载表单：useState 的初值只取一次，先挂载会拿到空值。
  return (
    <BrandForm key={app.id} app={app} initial={q.data} onDirtyChange={onDirtyChange} />
  );
}

function BrandForm({
  app,
  initial,
  onDirtyChange,
}: {
  app: AdminAppItem;
  initial: AdminAppBrand;
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const [form, setForm] = useState<BrandForm>(() => brandFormOf(initial));
  const [preview, setPreview] = useState(initial.logo_url);
  const [errors, setErrors] = useState<FormErrors>({});
  const save = useSaveAppBrandMutation();

  // 只在"脏/净"翻转时通知父级：父组件传下来的回调每次 render 都是新函数，
  // 不加这层守卫会变成 effect → setState → render → effect 的空转。
  const dirty = isBrandDirty(form, initial);
  const reported = useRef(dirty);
  useEffect(() => {
    if (reported.current === dirty) return;
    reported.current = dirty;
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  const set = <K extends keyof BrandForm>(k: K, v: BrandForm[K]) =>
    setForm((prev) => ({ ...prev, [k]: v }));

  const submit = () => {
    const e = validateBrand(form);
    setErrors(e);
    if (hasError(e)) return;
    save.mutate(
      {
        id: app.id,
        payload: {
          display_name: form.displayName.trim(),
          tagline: form.tagline.trim(),
          intro: form.intro.trim(),
          logo_icon: form.logoIcon,
          contact_name: form.contactName.trim(),
          contact_email: form.contactEmail.trim(),
          support_url: form.supportURL.trim(),
          privacy_url: form.privacyURL.trim(),
          terms_url: form.termsURL.trim(),
        },
      },
      {
        onSuccess: (brand) => {
          toastOk("品牌信息已保存");
          // 用服务端归一化后的值回填：既是"保存即脏值归零"的判据来源，
          // 也让运营看到真正入库的内容（前后空格已被 trim）。
          setForm(brandFormOf(brand));
          setPreview(brand.logo_url);
          setErrors({});
        },
        onError: (err) => toastApiError(err, "保存失败"),
      },
    );
  };

  return (
    <div className="tab-panel">
      <div className="alert-banner info">
        <span>
          以下信息会通过公开接口下发给终端应用（品牌名、图标、简介、客服与法务链接），不包含任何经营数据。
        </span>
      </div>

      <div className="form-grid">
        <Field
          label="展示名称"
          req
          htmlFor="brand-name"
          labelExtra={<span className="small muted">{runeLen(form.displayName)}/64</span>}
          hint={errors.displayName}
          hintWarn={!!errors.displayName}
        >
          <Input
            id="brand-name"
            value={form.displayName}
            onChange={(e) => set("displayName", e.target.value)}
            placeholder="默认应用"
          />
        </Field>

        <Field
          label="标语"
          htmlFor="brand-tagline"
          labelExtra={<span className="small muted">{runeLen(form.tagline)}/120</span>}
          hint={errors.tagline}
          hintWarn={!!errors.tagline}
          tip="一句话说明这个应用做什么，终端可能展示在启动页"
        >
          <Input
            id="brand-tagline"
            value={form.tagline}
            onChange={(e) => set("tagline", e.target.value)}
            placeholder="一句话说明这个应用做什么"
          />
        </Field>

        {/* 图标走全项目唯一的 IconPicker：内置图标 / 上传图片 / 清空三种取值
            与模型、渠道、供应商完全同一套。此前这里自造了上传按钮，后果是
            只支持上传（选不了内置）、accept 里写了后端不支持的 SVG、
            以及同一个文件连选两次不触发 change（input.value 未重置）。 */}
        <Field
          label="应用图标"
          span
          hint={errors.logoIcon}
          hintWarn={!!errors.logoIcon}
          tip="可选内置图标或上传图片（PNG / JPG / WebP，≤512KB）；存的是图标令牌，换存储或域名不影响"
        >
          <IconPicker
            value={form.logoIcon}
            valueUrl={preview}
            onChange={(token) => {
              set("logoIcon", token);
              if (!token) setPreview("");
            }}
            name={form.displayName || app.name}
            caption
          />
        </Field>

        <Field label="联系人" htmlFor="brand-contact" hint={errors.contactName} hintWarn={!!errors.contactName}>
          <Input
            id="brand-contact"
            value={form.contactName}
            onChange={(e) => set("contactName", e.target.value)}
            placeholder="客服/运营联系人"
          />
        </Field>

        <Field label="联系邮箱" htmlFor="brand-email" hint={errors.contactEmail} hintWarn={!!errors.contactEmail}>
          <Input
            id="brand-email"
            value={form.contactEmail}
            onChange={(e) => set("contactEmail", e.target.value)}
            placeholder="support@example.com"
          />
        </Field>

        <Field
          label="官网 / 支持链接"
          htmlFor="brand-support"
          hint={errors.supportURL}
          hintWarn={!!errors.supportURL}
          tip="终端会把它渲染成「获取帮助」入口"
        >
          <Input
            id="brand-support"
            value={form.supportURL}
            onChange={(e) => set("supportURL", e.target.value)}
            placeholder="https://example.com/help"
          />
        </Field>

        <Field label="隐私政策" htmlFor="brand-privacy" hint={errors.privacyURL} hintWarn={!!errors.privacyURL}>
          <Input
            id="brand-privacy"
            value={form.privacyURL}
            onChange={(e) => set("privacyURL", e.target.value)}
            placeholder="https://example.com/privacy"
          />
        </Field>

        <Field label="服务条款" htmlFor="brand-terms" hint={errors.termsURL} hintWarn={!!errors.termsURL}>
          <Input
            id="brand-terms"
            value={form.termsURL}
            onChange={(e) => set("termsURL", e.target.value)}
            placeholder="https://example.com/terms"
          />
        </Field>

        <Field
          label="简介"
          span
          htmlFor="brand-intro"
          labelExtra={<span className="small muted">{runeLen(form.intro)}/500</span>}
          hint={errors.intro}
          hintWarn={!!errors.intro}
        >
          <Textarea
            id="brand-intro"
            rows={4}
            value={form.intro}
            onChange={(e) => set("intro", e.target.value)}
            placeholder="纯文本，终端可能展示在「关于」页"
          />
        </Field>
      </div>

      <div className="form-actions">
        <button
          type="button"
          className="btn btn-text"
          onClick={() => {
            setForm(brandFormOf(initial));
            setPreview(initial.logo_url);
            setErrors({});
          }}
        >
          还原
        </button>
        <button type="button" className="btn btn-primary" disabled={save.isPending} onClick={submit}>
          {save.isPending ? "保存中…" : "保存品牌信息"}
        </button>
      </div>
    </div>
  );
}
