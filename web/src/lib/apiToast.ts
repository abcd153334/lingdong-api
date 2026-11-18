import { toast } from "sonner";

import { ApiError } from "./apiClient";

/**
 * 模型 / 供应商域的业务码 → 中文提示。
 *
 * 后端已经给了 `message`，但这些码对应的原话要么太泛（"参数错误"）、
 * 要么运营看不懂（`vendor_in_use`）；而**同一个错误码在两个页面出现两种说法**
 * 是最糟的结果，所以表只有一份，模型页与供应商分组弹窗共用。
 */
const ENTITY_ERRORS: Record<string, string> = {
  model_exists: "模型 ID 已存在",
  model_protected: "该模型已被渠道绑定或作为默认模型引用，无法删除",
  vendor_protected: "内置供应商分组不可删除",
  vendor_in_use: "该分组下仍有模型，请先迁移后再删除",
  vendor_exists: "分组名已存在",
};

/**
 * 统一的错误提示：业务码优先取本地文案，否则用后端原话，
 * 最后才回落到调用方给的兜底句。
 *
 * 顺序不能倒：后端原话是「可照做的原因」（配额、权限、具体哪个字段不合法），
 * 用笼统的"操作失败"盖住它，运营就只能靠猜。
 */
export function toastApiError(error: unknown, fallback = "操作失败，请稍后重试"): void {
  const code = error instanceof ApiError ? error.errorCode : "";
  toast.error((code && ENTITY_ERRORS[code]) || (error instanceof ApiError ? error.message : fallback));
}
