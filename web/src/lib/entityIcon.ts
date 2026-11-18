/**
 * 图标**令牌**的前端构造与判定。
 *
 * 这里**不做**「令牌 → 地址」的解析：那个映射的唯一实现在后端
 * `service/iconref`，所有响应都直接下发 `icon_url`（绝对地址）。
 *
 * 曾经这里有一份 `resolveIconURL`，把 `builtin:<key>` 拼成 `/icons/b/<key>`
 * 再套 `apiUrl()`。结果是同一条规则存在两处：后端改了路由形态、前端静默 404；
 * 跨域部署时前缀还算错一次。现在只有**写路径**需要令牌（提交表单、选中回填），
 * 渲染只认后端给的地址。
 */

/** 内置图标 token 前缀（与后端 brandicon.BuiltinPrefix 一致） */
const BUILTIN_PREFIX = "builtin:";

/** 上传图标 token 前缀（与后端 brandicon.UploadPrefix 一致） */
const UPLOAD_PREFIX = "upload:";

/** 内置图标 key 的形状（与后端 `builtinKeyRe` 一致）。 */
const BUILTIN_KEY_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

/**
 * 拼内置图标的 token（选择器选中时用；与后端 `brandicon.BuiltinToken` 对称）。
 *
 * 拼错的后果是「提交后被后端按 icon_invalid 拒掉」，所以这里也做形状校验：
 * catalog 的 key 恒为小写短横线形态，非法 key 直接给空串而不是拼一个必然失效的值。
 */
export function builtinIconToken(key: string): string {
  return BUILTIN_KEY_RE.test(key) ? `${BUILTIN_PREFIX}${key}` : "";
}

/** token 是否是内置图标（选择器里「内置图标 / 自定义图片」的来源说明用）。 */
export function isBuiltinIconToken(token: string): boolean {
  return token.startsWith(BUILTIN_PREFIX);
}

/** token 是否是上传图标（同上）。 */
export function isUploadIconToken(token: string): boolean {
  return token.startsWith(UPLOAD_PREFIX);
}
