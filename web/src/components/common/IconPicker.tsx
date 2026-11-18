import { useEffect, useRef, useState } from "react";

import { EntityIcon, type EntityIconSize } from "./EntityIcon";
import { Icon } from "./Icon";
import { isBuiltinIconToken, isUploadIconToken } from "@/lib/entityIcon";
import { IconLibrary } from "./IconLibrary";
import { Popover } from "./Popover";

export interface IconPickerProps {
  /**
   * 触发器按钮的 DOM id（由 `<Field>` 自动注入）。
   *
   * 必须落到「选择」按钮上：Field 的 label 用 `htmlFor` 指向它，
   * 不接的话点 label 毫无反应 —— 而图标没有原生输入框可关联，
   * 这是唯一能让 label 生效的挂点。
   */
  id?: string;
  /** 自己设的图标 token（`""` = 未设置，由 fallback / 首字接管）；**写回**用 */
  value: string;
  /**
   * `value` 对应的可渲染地址（实体的 `icon_url`）。
   *
   * 与 `value` 成对传：令牌只用于提交，渲染一律用地址。`value` 非空但这里为空，
   * 说明该图标在服务端已不可解析（对象被清理），此时预览会退回首字 ——
   * 正好提示运营「这张图没了，重选一张」。
   */
  valueUrl?: string;
  onChange: (token: string) => void;
  /**
   * 继承来的展示值（模型 = 所属供应商的图标）。
   *
   * 只用于**预览**：`value` 为空时触发器显示它，让运营看到"现在实际长什么样"。
   * 保存时提交的仍是 `value`（`""` = 继续继承），不是这个预览值 ——
   * 把预览值写回去就等于把"继承"固化成"自定义"，之后供应商换图标它不再跟随。
   */
  fallback?: string;
  /** `fallback` 对应的可渲染地址（模型 = 所属供应商的 `icon_url`） */
  fallbackUrl?: string;
  /** 继承说明文案（默认「沿用供应商」） */
  fallbackLabel?: string;
  /** 首字占位用的名字 */
  name?: string;
  /** 品牌色（首字占位底色） */
  color?: string;
  /** 砖的尺寸（沿用 EntityIcon 的档位，不新造档） */
  size?: EntityIconSize;
  /** 是否在砖下方显示来源小字（默认否：只进 title，名字行里才有位置） */
  caption?: boolean;
  disabled?: boolean;
  className?: string;
}

/**
 * 图标选择器：**一块可点的图标砖 + 下拉弹层**。
 *
 * 覆盖三类取值：内置图标 / 上传图片 / 空（首字占位或继承）。
 * 全项目只有这一个入口，模型、渠道、供应商都用它 —— 各写一个的结果是
 * "有的地方能上传、有的地方只能选内置"，而运营分不清哪是哪。
 *
 * 三个必须保持的细节：
 *
 *  1. `aria-label` **自带动作语义**（更换图标 / 收起图标库）。`Field` 会用
 *     `<label for>` 把它读成字段名「图标」，页面上还有供应商、适配器等等选择，
 *     只剩「图标」两个字的按钮读屏分不清是干什么的。可见内容只有一块图，
 *     所以要靠 aria-label 补全（label-in-name 依然成立：它是纯图标按钮）。
 *  2. 弹层走 `Popover`（portal 到 body）：`.dialog` 带 transform，会成为
 *     `position:fixed` 后代的包含块，就地定位会被弹窗的 scale(.95) 二次缩放、
 *     还会被 `.dl-body` 的 overflow 裁剪。
 *  3. 选中/清空后**主动把焦点交回砖**：`onPick` 会让弹层卸载，
 *     不交还的话焦点掉到 body，键盘用户下一次 Tab 从整页开头重新走。
 *
 * 品牌色不在这里：颜色是供应商的实体属性，只属于供应商分组管理。
 */
export function IconPicker({
  id,
  value,
  valueUrl,
  onChange,
  fallback,
  fallbackUrl,
  fallbackLabel = "沿用供应商",
  name,
  color,
  size = "md",
  caption = false,
  disabled = false,
  className,
}: IconPickerProps) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);

  /**
   * 「刚选中、还没保存」这一帧的预览地址。
   *
   * 为什么必须有本地状态：`value` 是令牌，令牌要变成地址得经过后端投影，
   * 而此刻它还没被保存、没有任何实体响应会带它的 `icon_url`。图库/上传回包
   * 是唯一知道地址的地方，所以选中时就把它记下来。
   *
   * `token` 一并记住是为了在父组件回写（保存成功 → `value` 变成新令牌）后
   * 认出「这就是我刚选的那张」，不做无谓的清除 —— 否则保存瞬间预览会闪一下首字。
   */
  const [picked, setPicked] = useState<{ token: string; url: string } | null>(null);
  useEffect(() => {
    // 外部把 value 改成了别的值（换实体 / 重置表单 / 撤销）→ 本地预览作废。
    if (picked && picked.token !== value) setPicked(null);
  }, [picked, value]);

  const ownUrl = picked ? picked.url : valueUrl || "";
  const preview = ownUrl || fallbackUrl || "";
  // 来源说明：说清"现在这个图标是从哪来的"，而不是只说"已设置"——
  // 后者在"继承了供应商图标"和"自己选了内置图标"两种情况下看不出区别。
  const state = value
    ? isUploadIconToken(value)
      ? "自定义图片"
      : isBuiltinIconToken(value)
        ? "内置图标"
        : "已设置"
    : fallbackUrl || fallback
      ? fallbackLabel
      : "使用首字";

  const close = (): void => setOpen(false);

  /** 选中 / 清空后统一收尾：提交、关面板、焦点回砖。 */
  const commit = (token: string, url = ""): void => {
    setPicked(token ? { token, url } : null);
    onChange(token);
    setOpen(false);
    triggerRef.current?.focus();
  };

  const trigger = (
    <button
      id={id}
      ref={triggerRef}
      type="button"
      className="ipick"
      aria-haspopup="dialog"
      aria-expanded={open}
      aria-label={open ? "收起图标库" : "更换图标"}
      title={caption ? undefined : state}
      disabled={disabled}
      onClick={() => setOpen((v) => !v)}
    >
      <EntityIcon url={preview} name={name} color={color} size={size} />
      {/* 角标：纯装饰，语义已经在 aria-label 里 */}
      <span className="ipick-badge" aria-hidden="true">
        <Icon name={value ? "edit" : "plus"} size={9} />
      </span>
    </button>
  );

  return (
    <>
      {caption ? (
        <span className="ipick-wrap">
          {trigger}
          <span className="ipick-cap">{state}</span>
        </span>
      ) : (
        trigger
      )}
      <Popover
        open={open}
        onClose={close}
        anchorRef={triggerRef}
        className="ipop-pop"
        role="dialog"
        ariaLabel="选择图标"
        prefer="below"
        gap={6}
        maxHeight={380}
      >
        <IconLibrary value={value} onPick={commit} onClear={() => commit("")} />
      </Popover>
    </>
  );
}
