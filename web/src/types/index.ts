import type { Currency } from "@/lib/money";

/** 统一响应体（S0 §6.3：{code,message,data}，code=0 为成功）
 *  追加 error_code（C-06）：机器可判读的业务错误码，成功或未定义时省略 */
export interface ApiResponse<T> {
  code: number;
  message: string;
  error_code?: string;
  data: T;
}

/** T21 分页响应 */
export interface PageResult<T> {
  items: T[];
  total: number;
  page: number;
  page_size: number;
}

/** 主题 */
export type ThemeMode = "light" | "dark";

/** 用户角色：0=管理员，1=普通用户 */
export type UserRole = 0 | 1;

export interface User {
  id: number;
  username: string;
  display_name?: string;
  role: UserRole;
  /** 余额（微；1 货币单位 = scale 微） */
  balance?: number;
  /** 登录/门户用户信息（后端原样返回，空串表示未绑定） */
  email?: string;
  phone?: string;
  group?: string;
  /**
   * 头像 data URL（`data:image/webp;base64,...`）。空串 / 缺省 = 未设置，
   * 界面渲染**打包进产物**的默认头像（`components/common/UserAvatar.tsx` 是唯一渲染点）。
   */
  avatar_url?: string;
  /**
   * 初始口令未修改（后端 `must_change_password`）。
   *
   * 只在"超管账号由系统生成口令"时为 true：前端据此把用户送到改密页，
   * 后端还会对所有业务端点返回 403 `password_change_required` —— 前端跳转只是体验，
   * 不是安全边界。
   */
  must_change_password?: boolean;
}

/** 收件渠道：验证码与联系方式的唯一维度（后端 verify.ChannelEmail/ChannelSMS） */
export type ContactChannel = "email" | "sms";

/** 验证码场景（与后端 verify.Scene* 一一对应） */
export type VerifyScene = "register" | "login" | "password_reset" | "contact_bind";

/**
 * 认证策略（`GET /api/portal/config` 的 `auth` 段）。
 *
 * 前端**不自己猜**「要不要验证码」「能不能注册」：全站默认值在服务端
 * `service/authcfg` 一处，前端只按这里下发的开关渲染。两处各写一套默认值
 * 的结果是「后端关了图形验证码，前端还在要」/「后端要求联系方式验证，
 * 前端表单里没有这个输入框」。
 */
export interface AuthConfig {
  /** 注册准入：open 开放 / invite 仅邀请 / closed 关闭 */
  register_mode: "open" | "invite_only" | "closed";
  /** 注册是否强制验证联系方式 */
  contact_verify: "required" | "off";
  /** 策略白名单：允许用哪些渠道（**不代表已配好发送服务**） */
  contact_channels: ContactChannel[];
  /**
   * 生效渠道集合 = 白名单 ∩ 传输就绪，按优先级排序（当前 sms 在前），
   * 首项即默认。注册页与找回抽屉只认这个；恒为数组，不会是 null。
   */
  available_contact_channels: ContactChannel[];
  /** 各场景的验证码策略：always 总是 / after_fails 失败 N 次后 / off 关闭 */
  captcha_register: "always" | "after_fails" | "off";
  captcha_login: "always" | "after_fails" | "off";
  captcha_reset: "always" | "after_fails" | "off";

  /** 登录失败多少次后转人机验证（服务端退避，不锁账号） */
  login_fail_threshold: number;
}

/** 登录返回的门户用户（contract: POST /api/portal/login） */
export interface PortalUser {
  id: number;
  username: string;
  email: string;
  phone: string;
  group: string;
  role: UserRole;
  balance: number;
  /** 头像 data URL；空串 = 未设置（渲染默认头像） */
  avatar_url?: string;
  /** 临时口令未改：登录收尾要据此把人送去改密页（而不是首页） */
  must_change_password?: boolean;
}

/** GET /api/portal/me */
export interface MeInfo {
  id: number;
  username: string;
  role: UserRole;
  status: number;
  group: string;
  /**
   * 用户分组的计费折扣（**微比例**：1e6 = 不打折，900000 = 九折）。
   *
   * 单值下发：全站折扣表（group_discount 全表）等于把分层定价公开，
   * 门户只需要知道"我自己这一档"。用 micro 比例而不是 0.9 这类浮点，
   * 是因为它要参与扣费乘算 —— 金额路径里不出现 float64。
   */
  group_discount: number;
  /** 分组是否真的配置了折扣（false = 回落 1e6，前端可提示「未享折扣」） */
  group_configured: boolean;
  balance: number;
  frozen: number;
  used: number;
  /** 累计结算次数（只在 settle/reclaim 时 +1，预检拒绝不计） */
  request_count: number;
  /** 名下 API 密钥总数（**含已停用**） */
  key_count: number;
  /** 其中处于启用状态的数量（status=1） */
  key_count_enabled: number;
  email: string;
  phone: string;
  /** 头像 data URL；空串 = 未设置（渲染默认头像） */
  avatar_url: string;
  /**
   * 临时口令未改（`must_change_password`）。
   *
   * 为 true 时后端只放行「改密 / 登出 / 踢会话 / 读自己」四条路径
   * （`middleware.passwordSetupExempt`），其余一律 403 `password_change_required`。
   * 前端据此把 /settings 收窄成只改密 —— 跳转只是体验，不是安全边界。
   */
  must_change_password?: boolean;
}

/** POST /api/portal/auth/register */
export interface RegisterResult {
  id: number;
  username: string;
}

/**
 * POST /api/portal/auth/login
 *
 * 没有 token：凭证在 httpOnly Cookie 里，JS 读不到也不需要读。
 * `expires_in` 是会话绝对有效期（秒），仅用于前端提示「N 天后需重新登录」。
 */
export interface LoginResult {
  user: User;
  expires_in: number;
}

/**
 * 登录第一步「需要二次验证」的响应。
 *
 * **此时没有任何 Cookie**：密码只是第一因素，服务端在第二因素通过前不下发会话票
 * （发了就等于把 2FA 降级成一个可以跳过的提示）。前端必须拿着 `challenge`
 * 去 `/portal/auth/login/mfa` 换会话。
 */
export interface LoginMFAChallenge {
  mfa_required: true;
  /** 一次性挑战令牌（5 分钟；绑出口 IP；错 5 次即作废） */
  challenge: string;
  /** 挑战剩余秒数 */
  expires_in: number;
}

/** 登录响应：直接成功，或要求二次验证 */
export type LoginResponse = LoginResult | LoginMFAChallenge;

/** 二次验证状态（GET /api/portal/auth/mfa/status） */
export interface MFAStatus {
  enabled: boolean;
  /** off | optional | required_for_admin */
  policy: string;
  /** 未使用的备份码数量（0 表示需要重新生成） */
  backup_codes_left: number;
}

/** 绑定第一步：密钥（明文只此一次）与 otpauth 地址 */
export interface MFASetupResult {
  secret: string;
  otpauth_uri: string;
}

/** 开启成功 / 重生成备份码：一次性返回的明文备份码 */
export interface MFABackupCodesResult {
  backup_codes: string[];
}

/** POST /api/portal/auth/password/reset/verify：校验验证码换一次性凭据 */
export interface ResetTicketResult {
  /** 一次性重置凭据（10 分钟；只能兑换一次） */
  reset_ticket: string;
  expires_in: number;
}

/** POST /api/portal/auth/register/contact-code 与 password/reset/contact-code */
export interface VerifyCodeResult {
  expires_in: number;
}

/** POST /api/portal/auth/contact/bind */
export interface BindingsResult {
  email: string;
  phone: string;
}

/** GET /api/portal/auth/sessions 行（设备列表；不含票据） */
export interface SessionItem {
  id: number;
  ip: string;
  ua: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  /** 是否为当前请求所用会话（前端标「本机」且禁掉「退出」） */
  current: boolean;
}

/** GET /api/admin/users 行 */
export interface AdminUserRow {
  id: number;
  username: string;
  email: string;
  phone: string;
  group: string;
  status: number;
  role: UserRole;
  balance: number;
  used: number;
  request_count: number;
  key_count: number;
  /** 用户级余额上限原值（微）：0 = 未单独设置（跟随全局配置），非 0 为覆盖值 */
  balance_limit: number;
  created_at: string;
}

/* ------------------ S3 定价目录 / API KEY / 模型目录 ------------------ */

/**
 * 模态枚举（8 类）。
 *
 * `music` 是最后加的一类（决策 H）：它是**异步按任务**计费的音乐生成（SUNO 等），
 * 与 `audio`（同步 TTS/ASR，按次或按字符）在计费口径、产物形态、提交端点上全不同，
 * 合并成一类会让「音乐模型」出现在 TTS 的下拉里，提交后必然被上游拒绝。
 */
export type Modality =
  | "chat"
  | "image"
  | "video"
  | "audio"
  | "music"
  | "embedding"
  | "rerank"
  | "moderation";

/**
 * 计费方式（与后端 models.billing_mode 同值）。
 *
 * 用字符串枚举而不是 0/1/2：数字枚举在 JSON 里对不上号（0 到底是"按量"还是
 * "未设置"？），日志里打印出来也没人能读懂。
 */
export type BillingMode = "tokens" | "call" | "spec";

/**
 * 规格价目（图片按张 / 视频按秒 / 任务按次 / 音频按秒）。
 *
 * 键集必须与后端 `model.SpecPrices` 逐字一致：后端对 spec_prices 做**严格解码**
 * （未知键直接 400），所以这里多写一个键、或者继续用旧的 `*_quota` 键名，
 * 保存时会被拒 —— 这是刻意的，多余的键只会掩盖"到底哪个价生效"。
 *
 * ⚠️ **不是所有字段都是金额**。这里同时住着三种量纲，混用一次就是真账错：
 *
 *	金额（微） 1 元 = 1e6 微：所有 *price / tier_price 类
 *	数量（原值）张 / 秒：max_n、max_seconds、ref_image_free、long_after_seconds
 *	倍率（微比例）1500000 = 1.5 倍：long_video_multiplier
 *
 * 另有 `allow_auto_duration` 是**布尔开关**，不属于上述任何量纲：它不参与计价，
 * 只决定「这个模型是否接受 seconds=-1」。别把它当金额换算。
 *
 * 量纲登记表在 `@/lib/specForm` 的 `SPEC_FIELDS`，读写两侧共用它。
 * 历史事故：把 max_n 当金额 ×1e6（4 → 4000000），图片模型 100% 保存失败。
 */
export interface SpecPrices {
  /** per_image 按张 / per_video 按秒 / per_task 按任务（存量）/ per_second 按秒 / per_clip 按条（音乐、歌词） */
  kind: "per_image" | "per_video" | "per_task" | "per_second" | "per_clip" | string;
  /** 图像档位单价（微/张）：键 1K / 2K / 3K / 4K（白名单来自后端 billing.ImageTierKeys） */
  tier_price?: Record<string, number>;
  /** 档位未命中时的兜底单价（微/张） */
  /** 单次产出上限（**数量**，不是金额）：per_image 是张数（留空 = 128）；per_clip 是单次任务的计费条数 */
  max_n?: number | null;
  /** 视频档位每秒单价（微/秒）：键 480p / 720p / 1080p / 2k（没有 4k） */
  second_price?: Record<string, number>;
  /** 模型级时长上限（**秒**，不是金额）；缺省只受全局 3600 秒约束 */
  max_seconds?: number | null;
  /**
   * 固定计费档位（480p / 720p / 1080p / 2k）：上游没有 resolution 参数、
   * 画质由模型自身决定时（如可灵按 mode 出片）用它对不传 resolution 的请求计价。
   *
   * 必须同时命中 `second_price` 的同名键，否则计费层找不到单价。
   */
  resolution_fixed?: string | null;
  /** 超时阈值（**秒**）；与 `long_video_multiplier` 必须成对 */
  long_after_seconds?: number | null;
  /** 超时倍率（**微比例**，1.5 倍 = 1500000，不是金额） */
  long_video_multiplier?: number | null;
  /**
   * 允许「智能时长」（上游自己决定时长，如 Ark 的 `duration=-1`）。
   *
   * **布尔开关**，不是金额也不是秒数。不开启时后端会拒绝 `seconds=-1`：
   * 其它厂商的上游会直接 reject 一个 -1，那会被记成渠道故障 → 误熔断健康渠道。
   */
  allow_auto_duration?: boolean | null;
  /** 参考图单价（微/张）与免计张数（**张**，0 = 无免费，上限 128） */
  ref_image_price?: number | null;
  ref_image_free?: number | null;
  /** 参考音频单价（微/个） */
  ref_audio_price?: number | null;
  /** 参考视频按条（微/条）与按秒（微/秒，按档位取键）—— 两者互斥 */
  ref_video_price?: number | null;
  ref_video_second_price?: Record<string, number>;
  /** 一次任务的固定价（微/次）；音乐/歌词的**存量**口径，与交付条数无关 */
  per_task_price?: number | null;
  /** 每条单价（微/条）：音乐、歌词按**实际交付条数**结算，0 条交付免单 */
  per_clip_price?: number | null;
  /** 音频每秒单价（微/秒）：per_second 必填；0 = 显式免费 */
  per_second_price?: number | null;
  /** 单次最低消费（微）：上游「最低按 x 秒收」时必填 */
  min_price?: number | null;
}

/**
 * 9 条价格 lane（微）。
 *
 * `null`/缺省 = 未配置，`0` = 显式免费 —— 两者语义不同，所以后端用指针存。
 * 前端表单提交时也必须区分：清空输入框 = null（不改），输 0 = 免费。
 */
export interface ModelPrices {
  input?: number | null;
  output?: number | null;
  cache?: number | null;
  cache_create?: number | null;
  cache_create_1h?: number | null;
  image?: number | null;
  audio?: number | null;
  audio_output?: number | null;
  call?: number | null;
}

/**
 * 模型营销标签（对外形状）。
 *
 * 只有两项：文案与**已解析**的渲染色。后端已经把 12 色板的 key / 自定义 hex
 * 统一成 `#RRGGBB`，前端不再维护第二张色表，也不做任何 token → hex 映射。
 * 已到期的标签不会出现在这里（由后端过滤）。
 */
export interface ModelTag {
  label: string;
  color_hex: string;
}

/** 管理台标签：多三项，都用于编辑回填与过期提示（服务端算好，前端不碰时钟）。 */
export interface AdminModelTag extends ModelTag {
  /** 入库原值：12 色板 key 或 `#RRGGBB`（大写），用于色块选中态 */
  color: string;
  /** 到期时刻（UTC RFC3339）；缺省 = 长期有效 */
  until?: string;
  /** 业务时区的 `YYYY-MM-DD`（表单日期框回填用） */
  until_date?: string;
  /** 是否已过期（已过期 = 不会对外展示，但数据保留） */
  expired: boolean;
}

/** 写入口：不含派生字段；数组顺序即展示顺序（没有 order 字段）。 */
export interface ModelTagInput {
  label: string;
  color: string;
  until?: string;
}

/** 管理台色板项（由后端下发，前端不得硬编码色值） */
export interface TagColorOption {
  key: string;
  label: string;
  hex: string;
}

/** GET /api/pricing 的模型条目 */
export interface PricingModel {
  id: string;
  display_name: string;
  vendor: string;
  vendor_name: string;
  vendor_color: string;
  /**
   * 图标地址（绝对地址，后端 `iconref` 拼好）。
   *
   * 已经是**派生后的最终值**（模型自己的，否则回落到所属供应商的），前端直接
   * `<img src>`。空串 = 这家没有配图标，按首字占位 —— 不要自己再拼回落链，
   * 回落规则散到前端就等于两套真相源，管理台与门户会显示不同的图标。
   */
  icon_url: string;
  modality: Modality;
  /** 计费方式：tokens 按量 / call 按次 / spec 按规格 */
  billing_mode: BillingMode;
  /** 按量/按次价目（微）；展示时用 amountOf/moneyOf 格式化，不做任何换算 */
  prices: ModelPrices;
  /** 按规格价目（微）；未配置为 null */
  spec_prices: SpecPrices | null;
  is_default: boolean;
  endpoints: string[] | null;
  /**
   * 已配置的价格是否全为 0。决定是否打「免费」徽标，**不参与计价** ——
   * 计价永远读具体 lane 的数值，不看这个布尔。
   */
  is_free: boolean;
  /** 营销标签（已过滤到期项）。无标签时是空数组，永远不是 null */
  tags: ModelTag[];
  /** 创建时间（Unix 秒），OpenAI list 的 created 就用它 */
  created_at: number;
}

/**
 * 用户端供应商分组（GET /api/pricing 的 vendors 段 / GET /api/portal/vendors）。
 *
 * 只有四个键：priority / sort / builtin / model_count 属运营数据，后端不下发。
 * 管理台用的是 {@link VendorItem}（另一个端点），两者不要混用。
 */
export interface PricingVendor {
  id: string;
  name: string;
  color: string;
  /** 图标地址；空串 = 首字占位 */
  icon_url: string;
}

/** 供应商分组（与计费分组正交） */
export interface VendorItem {
  id: string;
  name: string;
  color: string;
  /** 图标 token（**编辑回填**用）；空串 = 首字占位（「其他」分组故意不设图标） */
  icon: string;
  /** 同一个图标的可渲染地址（空串 = 首字占位） */
  icon_url: string;
  priority: number;
  sort: number;
  builtin: boolean;
  model_count: number;
}

/**
 * GET /api/admin/vendors 的响应。
 *
 * `palette` 是后端 `vendorPalette`（新建分组未传色时的取值来源）。
 * 前端**不得**自己再写一份色值表：两份色值抄错一位的表现是
 * "色盘 Highlights 选了 A、落库成了 B"，而且只在新建时暴露。
 */
export interface AdminVendorsResult {
  items: VendorItem[];
  /** 取色盘（`#RRGGBB` 大写，至少一项；对外数组恒非 null） */
  palette: string[];
}

/**
 * 供应商创建/编辑请求体。
 *
 * 三项都可选：`undefined` = 不改（创建时 = 用出厂默认）。
 * 后端对内置分组同样接受这三项 —— 内置只挡删除，不挡改名/换图标/换色。
 */
export interface AdminVendorPayload {
  name?: string;
  icon?: string;
  /** `#RRGGBB`（后端存大写） */
  color?: string;
}

/**
 * 内置图标（GET /api/admin/icons 的 builtin 项）。
 *
 * `url` 由后端拼好（`iconref`）：图片路由的形态只在服务端定义一次，
 * 前端不再自己拼 `/api/icons/b/<key>` —— 两条规则必然在跨域部署时对不上。
 */
export interface BrandIconItem {
  key: string;
  /** 英文品牌名（选择器里显示，避免中英混排错位） */
  name: string;
  group: "llm" | "media" | "platform";
  /** 中文/其它别名，供搜索（如「通义」「千问」） */
  aliases: string[];
  /** 绝对地址（含 per-key 版本参数，换图后不会命中旧缓存） */
  url: string;
}

/** 已上传图标（GET /api/admin/icons 的 uploaded 项） */
export interface UploadedIconItem {
  id: string;
  /** 可直接写进实体 icon 列的 token（`upload:<id>`）：**写回**用，不用于渲染 */
  icon: string;
  /** 可渲染的绝对地址（渲染只认它） */
  url: string;
  size: number;
  created_at: number;
}

/** GET /api/admin/icons */
export interface AdminIconsResult {
  builtin: BrandIconItem[];
  uploaded: UploadedIconItem[];
}

/** GET /api/pricing */
export interface PricingSnapshot {
  models: PricingModel[];
  vendors: PricingVendor[];
  /** 每模态默认模型（模态 → 模型 ID）；由后端唯一派生，前端不得自行推断。 */
  defaults?: Record<string, string>;
  /**
   * 1 货币单位 = 多少微（后端 money.Scale，恒 1e6）。
   *
   * **必须用它格式化**，不要在 JS 里写死 1000000：两边各写一个常数，
   * 某天改动只会改一边，症状是全站金额差 10 的倍数。
   */
  scale: number;
  /** 结算币种（CNY/USD）与展示符号（符号跟着币种走，不可自定义） */
  currency?: string;
  currency_symbol?: string;
  /** 另一种币种（「≈ $10.00」参考展示用；后端下发而非前端算） */
  other_currency?: string;
  /** 参考汇率，只用于次币种展示，永不参与扣费 */
  cny_per_usd: number;
}

/** API KEY 对外视图（GET /api/portal/keys） */
export interface ApiKeyItem {
  id: number;
  key_prefix: string;
  name: string;
  group: string;
  status: number;
  /** 密钥类型："user" 自建 / "hosted" 应用托管（托管卡片的操作集完全不同） */
  kind: "user" | "hosted";
  /** 托管密钥所属应用 id；自建为 0/缺省 */
  app_id?: number;
  /** 归属应用的 slug / name（自建为空串）。后端直出，前端不必再调 /portal/apps 反查 */
  app_slug?: string;
  app_name?: string;
  /** 托管密钥的权限点；自建无此字段 */
  scopes?: string[];
  /**
   * status=2 的成因，决定文案与恢复路径：
   * - `admin_block`：平台封禁，轮换不会解除
   * - `expired`：闲置超期，轮换即可恢复
   * - 空串：启用中
   */
  disabled_reason?: string;
  /** 该 KEY 的累计消费上限（微）；0 = 不限制 */
  amount_limit: number;
  /** 该 KEY 已消费金额（微） */
  used: number;
  models_allowed: string[] | null;
  modalities_allowed: Modality[] | null;
  allow_ips: string[] | null;
  rpm_limit: number;
  concurrency_limit: number;
  expired_at: string | null;
  allow_user_quota_query: boolean;
  created_at: string;
  reset_secret_at: string | null;
}

/** POST /api/portal/keys（一次性明文） */
export interface CreateKeyResult {
  key: ApiKeyItem;
  plaintext: string;
}

/** POST /api/portal/keys/{id}/reset 与 GET .../plaintext */
export interface KeySecretResult {
  plaintext: string;
}

/** 创建/编辑 API KEY 请求体（编辑为整体替换） */
export interface KeyPayload {
  name: string;
  /**
   * 已废弃：分组由账号决定，密钥不能单独设置。
   *
   * 后端对非空 group 直接 400（分组同时决定折扣倍率与可用渠道池）。
   * 字段保留为可选只是为了让旧调用点编译通过，**不要**再传值。
   */
  group?: string;
  /** 消费上限（微）；0 = 不限制 */
  amount_limit: number;
  expired_at: string | null;
  models_allowed: string[] | null;
  modalities_allowed: Modality[] | null;
  allow_ips: string[] | null;
  rpm_limit: number;
  concurrency_limit: number;
  allow_user_quota_query: boolean;
}

/** 管理台模型行（GET /api/admin/models） */
export interface AdminModelItem extends PricingModel {
  /** 管理台独占：启停状态与「该模型挂了几条渠道」（用户端不下发这两个字段）。 */
  enabled: boolean;
  /**
   * 模型**自己设的**图标 token（不含回落）。
   *
   * `icon_url`（继承自 PricingModel）是派生后的展示地址，`icon_own` 才是编辑表单要回填的。
   * 两者混用的后果：表单把「继承来的供应商图标」当成自己设的，一保存就固化成
   * 模型独立图标，之后供应商换图标它不再跟着变。
   */
  icon_own: string;
  /** 模型描述（编辑表单回填用；缺失时按空串处理） */
  description?: string;
  /** 模型级上下文窗口（0 = 继承助手全局设置） */
  context_window: number;
  /** 模型级单次输出上限（0 = 继承助手全局设置） */
  max_output_tokens: number;
  bindings: number;
  /** 模型能力声明（NULL = 未声明，前端据此显示「按事实推荐」按钮） */
  capabilities: string[] | null;
  /** 营销标签（**含已过期项**，运营要能续期；过期项带 `expired`）。对外口径是 `ModelTag` */
  tags: AdminModelTag[];
  /** 乐观锁版本号：编辑保存时必须回传，否则并发改价会静默覆盖 */
  version: number;
  /**
   * 「出厂内置」标记（空串 = 运营自建）。
   *
   * 只用于展示与入口可见性：列表据此标出哪些行会被「恢复出厂」覆盖。
   * 不是编辑开关——内置行照样能改名改价。
   */
  preset_group?: string;
  /**
   * 该模型在**当前筛选渠道**上的绑定（仅 `channel_id` 过滤时下发）。
   *
   * 右栏「渠道下的模型」每一行都要显示这条渠道上的上游名/权重/启停；
   * 不随列表下发就得对每行再发一次 GET，20 行 = 20 次往返。
   */
  binding?: AdminModelBinding;
}

/** 模型在某渠道上的绑定（渠道视角的差异配置）。 */
export interface AdminModelBinding {
  upstream_model: string;
  weight: number;
  priority: number;
  enabled: boolean;
}

export interface AdminModelsResult {
  items: AdminModelItem[];
  vendors: VendorItem[];
  /** 内置 12 色板（后端唯一下发点：前端不得自建色值表） */
  tag_palette: TagColorOption[];
  total?: number;
  page?: number;
  page_size?: number;
}

/** 创建/编辑模型请求体 */
/**
 * 创建/编辑模型请求体。
 *
 * 价格一律按**微**提交（与后端 `adminModelReq` 逐字同键）。所有可选字段的
 * "不改动"语义都是 **undefined（键不出现）**：后端用指针接收，缺省 = 沿用库内现值。
 * 想写 0（免费）必须显式给 0，不能靠空字符串。
 */
export interface AdminModelPayload {
  id: string;
  /** 乐观锁版本号（编辑时回传；缺省 = 后端不校验，供脚本使用） */
  version?: number;
  display_name: string;
  vendor: string;
  modality: Modality;
  /** 新建必填；编辑缺省 = 沿用库内现值 */
  billing_mode?: BillingMode;
  enabled?: boolean;
  input_price?: number | null;
  output_price?: number | null;
  cache_price?: number | null;
  cache_create_price?: number | null;
  cache_create_price_1h?: number | null;
  image_price?: number | null;
  audio_price?: number | null;
  audio_output_price?: number | null;
  call_price?: number | null;
  /**
   * 要**显式清空**（写 NULL）的价格通道名。
   *
   * 为什么需要它：通道用 `*int64` 表达，`null`/缺省同时意味着"本次没提交"，
   * 后端会用库内现值补齐 —— 于是「把某个通道的单独定价删掉再保存」永远不生效。
   * 名单把"不改"和"要清空"分开。
   */
  clear_prices?: string[];
  /** null/缺省 = 不改动；非空 = 整块覆盖 */
  spec_prices?: SpecPrices | null;
  description?: string;
  /**
   * 图标 token（`""` | `builtin:<key>` | `upload:<id>`）。
   *
   * 语义：`undefined`（键不出现）= 不改；`""` = 清空（回到"沿用供应商图标"）。
   * 表单提交的是 `icon_own` 而不是展示值 `icon`，见 {@link AdminModelItem.icon_own}。
   */
  icon?: string;
  /** 缺省 = 不改动；`[]` = 明确清空 */
  endpoints?: string[] | null;
  capabilities?: string[] | null;
  /**
   * 营销标签（数组顺序即展示顺序，最多 3 个）。
   *
   * 语义：`undefined`（键不出现）= 不改；`[]` = 清空。表单**恒提交**（含 `[]`），
   * 否则运营删光标签后永远清不掉。
   */
  tags?: ModelTagInput[];
  /**
   * 模型级上下文窗口 / 输出上限（0 = 继承助手全局设置）。
   *
   * 语义要点：`undefined`（键不出现）= 不改；显式 0 = 改回"继承"。
   * 前端表单留空时**必须发 0**，否则清空过一次就再也清不掉。
   */
  context_window?: number;
  max_output_tokens?: number;
  /**
   * **仅创建时生效**：同一事务里把新模型绑到一条渠道上。
   *
   * 为什么是单对象而不是 `bindings` 数组：这个键名在结构里已被
   * `AdminModelItem.bindings`（绑定**数量**）占用，复用会让数组塞进数字；
   * 而"在一个渠道下新建模型"只会用到一条（多渠道请建完再逐条绑）。
   */
  initial_binding?: {
    channel_id: number;
    upstream_model: string;
    weight: number;
    enabled: boolean;
  };
}

/**
 * 保存绑定返回的一条非阻断告警。
 *
 * 语义：**配置已保存成功**，但该渠道可能承接不了这个模型（例如多协议适配器的
 * 内置档案里没有这个平台模型 ID，渠道也没配 model_overrides）。
 * 不做成硬错误是因为绑定与渠道配置分属两个页面，合理的操作顺序可能相反。
 */
export interface BindingWarning {
  channel_id: number;
  upstream_model: string;
  message: string;
}

/** PUT /api/admin/models/{id}/bindings/{channelId} 响应 */
export interface BindingSaveResult {
  bindings: number;
  /** 恒为数组（空为 []），前端直接取 .length */
  warnings: BindingWarning[];
}

/**
 * 上游参考价条目（GET /api/admin/upstream/search）。
 *
 * 双量纲是刻意的：`*_usd` 是上游挂牌价（可对账），`*_micro` 是按站点汇率折算后的
 * 本地量纲（可直接填进 `price_*`）。两者都可为 null —— null 表示上游没报这一档，
 * **不是 0**（0 在本地语义里是"显式免费"）。
 */
export interface UpstreamPriceItem {
  id: string;
  name: string;
  provider_id: string;
  provider_name: string;
  modality: Modality;
  deprecated: boolean;
  /** 同名模型在多少家来源有报价（面板据此显示「另有 N 家」） */
  provider_count: number;
  input_usd: number | null;
  output_usd: number | null;
  cache_usd: number | null;
  input_micro: number | null;
  output_micro: number | null;
  cache_micro: number | null;
}

/** 本次查询的来源与口径（currency/fx_rate 是后端实际换算用的，展示符号必须取它） */
export interface UpstreamSearchMeta {
  source: string;
  fetched_at: string;
  /** true = 本次刷新失败，返回的是上一次的快照 */
  stale: boolean;
  currency: Currency;
  fx_rate: number;
  /** 实际命中的关键词（剥离日期后缀后可能与请求词不同） */
  matched_keyword: string;
  /** 去重后命中的模型数（含有价与无价） */
  total: number;
  /** 其中有公开报价的数量 */
  with_price: number;
}

export interface UpstreamSearchResult {
  items: UpstreamPriceItem[];
  meta: UpstreamSearchMeta;
}

/* ------------------ S4 渠道与适配器 ------------------ */

/** 渠道状态枚举（G23 冻结）：1 启用 / 2 手动禁用 / 3 自动禁用 */
export type ChannelStatus = 1 | 2 | 3;

/** 多密钥调度模式（对齐 mock-data.js L172-L178：polling / random） */
export type MultiKeyMode = "polling" | "random";

/** channels.health 健康度快照 */
export interface ChannelHealth {
  /** 渠道图标 token（看板渠道健康行） */
  icon?: string;
  fail_rate: number;
  success_count: number;
  fail_count: number;
  last_test_at?: string | null;
  last_error?: string;
}

/** 熔断滑窗内的实时样本（真实流量口径，与 health 的测试计数不同源） */
export interface ChannelWindow {
  total: number;
  fails: number;
  fail_rate: number;
}

/** 熔断策略（system_config.monitor，随渠道列表下发） */
export interface MonitorPolicy {
  auto_disable: boolean;
  /** 失败率阈值（0–1） */
  fail_threshold: number;
  /** 窗口分钟数，同时也是最小样本数 */
  fail_window: number;
  auto_enable: boolean;
  probe_interval_min: number;
}

/** 渠道绑定视图项（派生自 model_bindings，只读） */
export interface ChannelBoundModel {
  model: string;
  /** 模型生效图标地址（已回落供应商） */
  icon_url?: string;
  alias: string;
  weight: number;
  /**
   * 绑定**自己**的启停（与渠道启停是两件事）。
   *
   * 必须下发：删除渠道的拦截判据是「任意绑定（含停用）」。只给启用绑定时
   * 前端会算出「0 个模型」直接放行、后端 409 —— 运营看到"空渠道却删不掉"。
   */
  enabled: boolean;
}

/** GET /api/admin/channels 行 */
/**
 * 渠道级声明式适配配置（与后端 relay.AdapterConfig 一一对应）。
 * 用来接「结构接近 OpenAI、但路径/鉴权/字段不同」的上游，免写插件。
 */
export interface AdapterConfigDoc {
  /** 注入密钥的头名，默认 Authorization（如 x-api-key） */
  auth_header?: string;
  /** 密钥前缀，默认 "Bearer "；"none" 表示直填密钥 */
  auth_prefix?: string;
  /** canonical 端点路径 → 上游实际路径（可带 query） */
  path_overrides?: Record<string, string>;
  /** 点分路径 → 值；null 表示删除该键；"$model" 引用上游模型名 */
  request_patch?: Record<string, unknown>;
  /** 仅非流式 chat 生效：从非标准响应里取正文与 token */
  response_path?: AdapterConfigResponsePath;
}

export interface AdapterConfigResponsePath {
  content: string;
  prompt_tokens?: string;
  completion_tokens?: string;
}

export interface ChannelItem {
  id: number;
  name: string;
  /** 图标 token（**编辑回填**用）；空串 = 首字占位 */
  icon: string;
  /** 同一个图标的可渲染地址；空串 = 首字占位 */
  icon_url: string;
  adapter_id: string;
  base_url: string;
  /** 渠道自己配置的上游官网（"" = 未配置，回落到适配器默认）；编辑回填用 */
  website?: string;
  /** 可直接跳转的**有效**官网（自己配置的优先，否则适配器默认）；空 = 不显示跳转 */
  website_url?: string;
  /** 「出厂内置」标记（空串 = 运营自建）：只用于展示与「恢复出厂」入口可见性 */
  preset_group?: string;
  multi_key_mode: MultiKeyMode;
  groups: string[];
  weight: number;
  priority: number;
  rpm_limit: number;
  max_concurrency: number;
  /**
   * **本机**当前并发占用 / 信号量上限（进程内快照）。
   *
   * 多副本部署时它不是全局值：3 副本 × 20 的真实上限是 60 并发。
   * 只用来判断「本机这一份是不是已经打满」，不能当全局已用并发。
   */
  inflight: number;
  inflight_limit: number;
  status: ChannelStatus;
  auto_ban: boolean;
  balance_cny: number | null;
  /** 物理条数（含无法解密的条目） */
  key_count: number;
  /** **可用**条数（解密成功且启用）：选路实际会用的就是这些 */
  key_enabled_count: number;
  /** 失败时是否换同渠道另一把密钥重试（默认关） */
  switch_key_retry: boolean;
  /** 乐观锁版本：密钥面板提交时要带回来，否则 CAS 永远成立 */
  version: number;
  health: ChannelHealth;
  /** 声明式适配配置；后端保证下发（零值为 {}） */
  adapter_config: AdapterConfigDoc;
  /**
   * 适配器私有配置原文（形状由适配器定义，如 AutoDL 的工作流映射）。
   * null/缺省 = 未配置，由适配器内置默认兜底；脏数据后端也会回 null（不带病下发）。
   */
  adapter_options?: Record<string, unknown> | null;
  /** 滑窗内有流量时才下发；缺省 = 窗口内无真实调用 */
  window?: ChannelWindow;
  bound_models: ChannelBoundModel[];
}

/**
 * 适配器能力视图（内置 + 我的）。
 *
 * 两项在同一个接口里下发是刻意的：内置与规格都是「渠道能选的适配器」，
 * 分成两个接口会让渠道下拉必须等两个请求都回来才能渲染。
 */
export interface AdapterInfo {
  id: string;
  name: string;
  /** 中文展示名（内置来自静态元数据表，规格取 label） */
  label: string;
  vendor: string;
  /**
   * 图标**可渲染地址**（空 = 首字占位）。
   *
   * 只有内置适配器有值：自建规格画供应商的标会让人以为它是官方直连。
   * 可选是因为渠道下拉等旧调用方的桩数据没有这个字段 —— 缺了就是首字，不是错。
   */
  icon_url?: string;
  caps: string[];
  status: number;
  /** builtin = 随进程发布；custom = AI/人工生成的规格 */
  source: "builtin" | "custom";
  /** 能不能改（内置是 Go 代码，不可改） */
  editable: boolean;
  /** 能不能删（内置不可删；规格在仍被渠道引用时后端会拒） */
  deletable: boolean;
  /** 规格主键（仅 custom） */
  spec_id?: number;
  /** 规格版本（仅 custom，编辑要用它做乐观锁） */
  version?: number;
  /** 引用该适配器的渠道数 */
  channel_count: number;
  /** 是否支持渠道级声明式配置（插件不支持：鉴权与路径由插件清单决定） */
  supports_declarative?: boolean;
  /**
   * 是否接受渠道私有配置（channels.adapter_options）。
   * 前端据此才渲染自己的配置块 —— 不认识的适配器不显示，避免"配了没人读"。
   */
  supports_options?: boolean;
  /**
   * 适配器的渠道默认 Base URL（空 = 无默认值）。
   *
   * 前端只在「切换适配器且当前值为空 / 仍是上一个适配器的默认值」时预填：
   * 已经有值的（运营手填过、或是编辑既有渠道）绝不覆盖。
   */
  default_base_url?: string;
  /** 该适配器的默认上游官网（空 = 无默认值）；渠道表单用它做占位与跳转兜底 */
  website?: string;
  /** 规格声明的端点（内置恒为空数组） */
  endpoints: string[];
}

/** 规格详情（编辑抽屉用：带 DSL 原文与读期告警） */
export interface AdapterSpecItem {
  id: number;
  slug: string;
  label: string;
  vendor: string;
  adapter_id: string;
  status: number;
  version: number;
  endpoints: string[];
  caps: string[];
  channel_count: number;
  owner_id: number;
  warnings: string[];
  updated_at: string;
  spec?: Record<string, unknown>;
}

/** 规格保存入参（slug 不可改，所以不在 payload 里） */
export interface AdapterSpecPayload {
  label: string;
  vendor: string;
  status: number;
  /** 乐观锁：必须回传详情里的 version，否则并发覆盖会静默丢改动 */
  version: number;
  /** DSL 原文（前端只做 JSON.parse，语义校验一律服务端） */
  spec: Record<string, unknown>;
}

/** 规格影响面（删除确认框） */
export interface AdapterSpecImpact {
  channel_count: number;
  channels: { id: number; name: string; status: number }[];
  models: { id: string; name: string; endpoints: string[] }[];
  missing_endpoints: string[];
}

export interface AdminChannelsResult {
  items: ChannelItem[];
  adapters: AdapterInfo[];
  groups: string[];
  /** 熔断策略（P2-22：解释「为什么还没熔断」） */
  monitor?: MonitorPolicy;
  /** 匹配总数（分页时由服务端下发；不分页时等于 items 长度） */
  total?: number;
  page?: number;
  page_size?: number;
}

/** 创建/编辑渠道请求体（编辑时 keys 缺省 = 不修改密钥池） */
export interface ChannelPayload {
  name: string;
  /**
   * 图标 token（`""` | `builtin:<key>` | `upload:<id>`）。
   * 语义与模型一致：`undefined` = 不改；`""` = 清空（回到首字占位）。
   */
  icon?: string;
  adapter_id: string;
  base_url: string;
  /** 上游官网；恒提交，空串 = 清空（回到适配器默认） */
  website?: string;
  multi_key_mode: MultiKeyMode;
  groups: string[];
  weight: number;
  priority: number;
  rpm_limit: number;
  max_concurrency: number;
  auto_ban: boolean;
  switch_key_retry?: boolean;
  keys?: string[];
  /** null = 清空已有配置；缺省（undefined）= 不修改 */
  adapter_config?: AdapterConfigDoc | null;
  /**
   * 适配器私有配置（同上语义：undefined = 不修改，null/{} = 清空回内置默认）。
   * 只在该适配器的 supports_options 为 true 时才允许下发，否则后端 400。
   */
  adapter_options?: Record<string, unknown> | null;
}

/** POST /api/admin/channel/test */
export interface ChannelTestResult {
  ok: boolean;
  message: string;
  latency_ms: number;
  /** 命中的是**原始数组下标**（第几把），不是"可用池里的第几个" */
  key_index: number;
  key_count: number;
  key_id: string;
  key_alias: string;
  key_fingerprint: string;
}

/**
 * 渠道密钥池的一枚密钥（管理台视图）。
 *
 * **没有明文字段**：明文只在录入那一刻出现在请求体里，之后任何接口都拿不回来。
 */
export interface ChannelKeyItem {
  index: number;
  /** 解密失败时为空（算不出指纹） */
  key_id: string;
  /** 前 6 + 后 4；短 key 一律 ****** */
  fingerprint: string;
  alias: string;
  enabled: boolean;
  decrypt_ok: boolean;
  /** 冷却原因：auth（需换 key）/ rl（限流，等一会儿）/ 空 = 未冷却 */
  cooldown_reason: string;
  cooldown_remaining_sec: number;
  last_error_status: number;
  /** RFC3339；空 = 从未失败或 Redis 不可用 */
  last_error_at: string;
}

export interface ChannelKeysResult {
  version: number;
  items: ChannelKeyItem[];
  key_count: number;
  broken_count: number;
  key_enabled_count: number;
}

/**
 * 密钥池变更操作（`PATCH /api/admin/channels/:id/keys` 的 ops 元素）。
 *
 * `replace` 是**原地替换**：位置、别名、启用态都继承旧行，只有明文与 keyID 变。
 * `replace_all` 是整池重建，必须独占一批（与增量操作混提交后端会 400）。
 */
export type ChannelKeyOp =
  | { op: "add"; key: string }
  /** `alias` 可随替换一起写：一次编辑同时改别名与明文必须是一条原子 op。 */
  | { op: "replace"; key_id: string; key: string; alias?: string }
  | { op: "replace_all"; keys: string[] }
  | { op: "remove"; key_id?: string; index?: number }
  | { op: "meta"; key_id: string; alias?: string; enabled?: boolean }
  | { op: "reset_cooldown"; key_id: string };

export interface ChannelKeysPayload {
  version: number;
  ops: ChannelKeyOp[];
}

/* ---------------------- S5 调用记录（GET /api/portal/usage） ---------------------- */

/**
 * 一次调用的结算记录（门户 GET /api/portal/usage，后端 `logs.PortalItem`）。
 *
 * 耗时三件套的语义（不要混用）：
 * - `latency_ms`：**终态总耗时** = finished_at − started_at（含重试与排队）；进行中为 0。
 * - `elapsed_ms`：服务端算的「已进行多久」，进行中时 = 现在 − started_at；终态等于 latency_ms。
 * - `service_ms`：最后一次服务调用的耗时。总耗时远大于它 = 慢在网关/重试；两者接近 = 慢在服务端。
 *
 * 这个类型**只描述门户能拿到的东西**：渠道 / 适配器 / IP / 服务商单号 / 计费快照
 * 一律不在契约里（后端也不下发）。管理台用 `AdminCallDetail`，字段更宽。
 */
export interface UsageItem {
  id: number;
  key_id: number;
  /** 用户自己那把 KEY 的名字（不是密钥内容） */
  key_name?: string;
  created_at: string;
  model: string;
  /** 模型图标地址（后端已回落供应商；空 = 首字占位） */
  model_icon_url?: string;
  modality: string;
  /** pending 进行中 / settled 已结算 / released 已释放 / rejected 预检拒绝（0 费） */
  status: string;
  http_status: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_tokens?: number;
  image_tokens?: number;
  audio_input_tokens?: number;
  audio_output_tokens?: number;
  /** 本次扣费金额（微） */
  charge: number;
  latency_ms: number;
  /** 服务端算的实时耗时（毫秒），前端只做本地单调递增 */
  elapsed_ms?: number;
  /** 服务耗时（最后一次调用的耗时）；与 latency_ms 的差额 = 排队与重试 */
  service_ms?: number;
  first_token_ms?: number;
  /** 实际尝试次数，解释总耗时与服务耗时的差额 */
  attempts?: number;
  started_at?: string | null;
  finished_at?: string | null;
  is_stream: boolean;
  /** 分类文案（空/缺省 = 成功）：固定枚举，不是服务商原文 */
  error_summary?: string;
  request_id?: string;
  /** sync / async / precheck */
  call_kind?: string;
  /** 对外任务号（`task_` 前缀；缺省 = 同步调用）。不是内部自增 id */
  task_id?: string;
  /** 预检期被拒（0 费），前端用于区分「没打出去」与「打出去失败」 */
  is_rejected?: boolean;
  video_resolution?: string;
  video_seconds?: number;
  /** 命中的档位单价（微，折扣前） */
  tier_price?: number;
  /** 两级降额之前应收（微） */
  charge_before_cap?: number;
  /** 结算口径的异常标记；门户只会拿到 reclaimed（其余是平台风控内部状态） */
  anomaly?: string;
  /**
   * 行内快照的结算币种（金额列必须用它选符号）。
   *
   * 金额本身是微，不需要任何换算基准 —— 只需要"当时用哪个币种记的账"。
   */
  currency?: string;
}

/**
 * 调用记录详情里的媒体项（门户版）。
 *
 * **没有 url**：产物存放在服务商的 CDN 上，域名本身就在说明"背后是谁"。
 * 门户一律走 `/api/portal/usage/:id/media/:index` 代理取字节。
 */
export interface CallMediaItem {
  kind: string;
  mime?: string;
  bytes?: number;
  /** 异步任务产物序号（-1 = 非任务产物，走媒体代理） */
  artifact_index: number;
  note?: string;
}

/** 调用记录详情里的报文（门户版：**用户自己发的** + **平台交给用户的**） */
export interface CallIODetail {
  /** 客户端原始请求体（已脱敏）。用户本来就知道自己发了什么，属于他本人数据 */
  req_raw?: string;
  req_bytes: number;
  req_truncated?: boolean;
  /** 请求内容来源：raw 原样 / rebuilt multipart 重构 / snapshot 任务快照 */
  req_source?: "raw" | "rebuilt" | "snapshot" | "none";
  /** 本行正文口径：full 全文 / meta 结构摘要 */
  body_mode?: "full" | "meta";
  /**
   * 平台**实际返回给客户端**的响应体（已脱敏）。
   *
   * 注意语义：这不是上游原文。产物直链已换成本站地址、协议转码后的客户端形状
   * 才是用它；失败调用时它是平台写出的错误 JSON。上游原文只给管理台。
   */
  resp_body?: string;
  resp_bytes: number;
  sse_frames: number;
  /** 从响应体抽出的纯文本输出（上限 8 KiB）；没有可预览文本时缺省 */
  output_text?: string;
  media?: CallMediaItem[];
}

/** 管理台版报文（含上游原文；门户永远拿不到 upstream_* 与 resp_raw） */
export interface AdminCallIODetail extends Omit<CallIODetail, "resp_body"> {
  req_sha256?: string;
  resp_raw?: string;
  resp_error?: string;
  /** 客户端真正收到的响应体（与 resp_raw 不同时才单独存） */
  delivered_raw?: string;
  /** 响应来源：raw 上游原文 / aggregate 流式拼接 / usage 任务兜底 / none */
  resp_source?: "raw" | "aggregate" | "usage" | "none";
  /** 实际发给上游的报文（平台重建后）；与 req_raw 是两份不同的东西 */
  upstream_raw?: string;
  upstream_bytes?: number;
  upstream_truncated?: boolean;
  upstream_source?: "tap" | "tap-meta" | "tap-skipped" | "snapshot";
  upstream_url?: string;
  /** 上游 HTTP 状态；0 = 未收到响应（连接失败/超时） */
  upstream_status?: number;
  /** failover 实际出网次数（报文记最后一次） */
  upstream_attempts?: number;
}

/** 调用记录详情关联的异步任务（任务被清理时缺省） */

/**
 * 门户调用记录详情（GET /api/portal/usage/:id，后端 `logs.PortalDetailView`）。
 *
 * 只有「列表投影 + 计费解释 + 报文摘要」。渠道 / 适配器 / IP / 服务商单号 /
 * 计费快照 / 报文原文**都不在契约里**（后端不下发，这里也不声明）。
 */
export interface PortalCallDetail extends UsageItem {
  /** 计费方式：tokens 按量 / call 按次 / spec 按规格（与 models.billing_mode 同值） */
  billing_mode?: BillingMode;
  /** 图片档位与张数（解释"按张计费"的依据） */
  image_spec?: string;
  image_quality?: string;
  image_n?: number;
  audio_duration_secs?: number;
  audio_chars?: number;
  rerank_docs?: number;
  /** 视频档位与秒数 */
  video_resolution?: string;
  video_seconds?: number;
  ref_images?: number;
  ref_videos?: number;
  ref_audios?: number;
  ref_video_seconds?: number;
  tier_source?: string;
  /** 冻结金额（微） */
  frozen?: number;
  call_io?: CallIODetail;
  /**
   * 关联的异步任务：**就是任务对象本身**（与 /v1/tasks/{id} 同一个结构体）。
   *
   * 没有 frozen/settled —— 金额属于调用记录（本对象外层已有），
   * 所以这里是 TaskView 而不是 TaskItem。
   */
  task?: TaskView;
}

/**
 * 管理台调用记录详情（GET /api/admin/logs/:id，后端 `logs.Detail`）。
 *
 * 在门户契约之上补回排障字段：管理台**应该**看到渠道、适配器、IP、服务商单号与报文原文。
 * 用 extends 而不是复制一份，是为了保证"两边共有的字段"永远同名同类型。
 */
export interface AdminCallDetail extends Omit<PortalCallDetail, "call_io"> {
  /**
   * 任务失败原文（上游/平台记录），**只有管理台下发**。
   *
   * 任务对象本身的 `error` 恒为分类文案（门户读的是同一个结构体，原文进去就是泄漏），
   * 所以排障原文跟着调用记录走，作为伴随字段。
   */
  task_error_msg?: string;
  user_id?: number;
  username?: string;
  key_name?: string;
  channel_id?: number;
  channel_name?: string;
  /** 渠道图标地址（门户不下发渠道，所以只在管理台详情里有） */
  channel_icon_url?: string;
  adapter_id?: string;
  ip?: string;
  group?: string;
  total_tokens?: number;
  /** 上游成本（微） */
  cost?: number;
  request_digest?: string;
  raw_usage?: string;
  usage_semantic?: string;
  video_seconds_source?: string;
  /** 服务商开单号：与对方对账时唯一的共同标识 */
  upstream_request_id?: string;
  /** 服务商返回的错误原文（脱敏后）；门户改用 error_summary 分类文案 */
  error_msg?: string;
  /** 截顶原因（global / frozen3x / balance）：风控口径，仅管理台 */
  cap_reason?: string;
  /** 计费来源：upstream / mixed / estimated / none / unknown */
  usage_source?: string;
  pricing_snapshot?: Record<string, unknown>;
  call_io?: AdminCallIODetail;
}

/** 门户「类型分布」项（S17）：在 `usages` 上按用户与时间窗实时聚合，与明细同源 */
export interface ModalityStat {
  modality: string;
  requests: number;
  /** 消耗金额（微） */
  charge: number;
}

/** GET /api/portal/usage 响应（分页 + 类型分布） */
export interface PortalUsageResult {
  items: UsageItem[];
  total: number;
  page: number;
  page_size: number;
  modality_dist: ModalityStat[];
}

/** GET /api/admin/users/{id}/keys 行（管理台只读；永不含明文） */
export interface AdminUserKeyItem {
  id: number;
  key_prefix: string;
  name: string;
  group: string;
  status: number;
  amount_limit: number;
  used: number;
  models_allowed: string[];
  modalities_allowed: string[];
  rpm_limit: number;
  concurrency_limit: number;
  expired_at: string | null;
  created_at: string;
}

/* ------ S7 异步任务 ------ */

/** 异步任务状态（S7 冻结枚举） */
export type TaskStatus = "queued" | "running" | "succeeded" | "failed" | "expired" | "canceled";

/**
 * 任务对象（全站唯一形状；后端 `taskview.View`）。
 *
 * 出现在四处，且**是同一个结构体**：提交 202（/v1 各模态）、查询详情/列表
 * （/v1/tasks、/api/portal/tasks、/api/admin/tasks）、取消与重试的返回、
 * 以及调用记录详情里的 `task` 字段。
 *
 * id 是十进制字符串、时间是 Unix 秒、error 恒为对象（成功为 null）、
 * contents 元素同时带 url 与 kind —— 前端不再为每个面写一套解析。
 */
export interface TaskView {
  /** 对外任务号：`task_` 前缀的 ULID，**不是数字**（不要 parseInt / 当 key 排序） */
  id: string;
  object: "task";
  model: string;
  /** 模型图标地址（后端已回落供应商） */
  model_icon_url?: string;
  modality: string;
  status: TaskStatus;
  progress: number;
  kind?: string;
  /** 失败/超时/取消的**分类文案**（不是服务商原文）；成功时为 null */
  error: { message: string } | null;
  /** 已转存产物清单：**仅任务详情接口返回**（列表为摘要，避免 N+1 查询）。 */
  contents: TaskArtifact[];
  /** Unix 秒（不是 RFC3339）：与 /v1 的其它时间戳同一口径 */
  created_at: number;
  /** 只有终态才有 */
  completed_at?: number;
  deadline_at?: number;
  usage: { images: number; seconds: number; ref_images: number };
}

/**
 * 门户/应用面任务对象（`taskview.PortalItem`）：任务对象 + 资金字段。
 *
 * 资金字段**只**在任务端点下发，调用记录里嵌的任务对象没有它 —— 那不是缺字段，
 * 是不同资源：金额属于"这次调用扣了多少"，在调用记录里由记录自己给。
 */
export interface TaskItem extends TaskView {
  /** 预扣金额（微） */
  frozen: number;
  /** 结算金额（微） */
  settled: number;
}

/**
 * 管理台任务行（GET /api/admin/tasks*）。
 *
 * 在门户契约之上补回归属与供应链字段：运营排障需要知道"这条是谁的、
 * 走的哪个渠道、对方给了什么任务号"。这些字段**永远不下发门户**。
 */
export interface AdminTaskItem extends TaskItem {
  user_id: number;
  key_id: number;
  /** 任务归属的应用（经 key_id 派生）；0 = 平台直连（用户自建密钥） */
  app_id: number;
  /** 应用展示名（display_name 优先）；0 或查不到时为空串 */
  app_name?: string;
  channel_id: number;
  /** 服务商标识（如 duomi / metaso），仅管理台可见 */
  platform: string;
  upstream_task_id?: string;
  /** 上游/平台记录的原始错误串（排障用）；对外只给分类文案 error.message */
  error_msg?: string;
}

/** 任务产物条目（序号 / 地址 / 类型 / 字节数；不含存储路径或上游签名链接） */
export interface TaskArtifact {
  /** 对外序号：0 起、稠密编号（失败与过期条目已被剔除，不会跳号） */
  index: number;
  /** 取数地址：门户/管理台各是自己的路由组，/v1 是绝对地址 */
  url: string;
  content_type: string;
  bytes: number;
  /** 后端按 MIME 派生：image / video / audio / text / file */
  kind: string;
}

/* ------ S8 文件上传 / 临时托管 ------ */

/** 门户文件视图（GET /api/portal/files；跨当前用户全部 KEY，仅未过期件） */
export interface UploadItem {
  /** ULID（26 位） */
  id: string;
  /** 原始文件名 */
  filename: string;
  /** 直链展示成分（服务端清洗后的安全名） */
  safe_name: string;
  size_bytes: number;
  /** 服务端嗅探的内容类型 */
  content_type: string;
  /** 完整公网直链（https://api.xxx/f/{id}/{safe_name}） */
  url: string;
  key_id: number;
  /** 该文件所属 KEY 的名称 */
  key_name: string;
  created_at: string;
  expires_at: string;
  /** 剩余秒数；≤0 表示已过期 */
  remaining_seconds: number;
}

/** GET /api/portal/files/config（门户文件总闸与配额快照） */
export interface UploadConfig {
  enabled: boolean;
  daily_limit_per_key: number;
  retention_hours: number;
  user_total_mb: number;
  concurrency: number;
  /**
   * 支持的后缀 → 类别 → 单文件上限，一次给全。
   *
   * 服务端下发而不是前端维护映射：两份表必然漂移，漂移的表现是
   * 「明明支持的类型被前端拦住」或「选了才报 415」。
   */
  types: UploadTypeInfo[];
  /** 各类别的单文件上限（MB），用于顶部提示文案 */
  limits: Record<string, number>;
  /** 当前账号当日上传次数 */
  today_count: number;
  /** 当前账号在存字节数 */
  total_bytes: number;
  /**
   * 今日次数的计数主体。门户恒为 "account"（没有 KEY 上下文）。
   *
   * 服务端下发而不是前端推断：跳过这一项就只能写死「本 KEY」，
   * 而门户里根本没有 KEY —— 那句文案会让用户去找一把不存在的密钥。
   */
  quota_scope?: "account" | "key";
}

/** 一种可上传的后缀及其类别与体积上限 */
export interface UploadTypeInfo {
  ext: string;
  kind: string;
  max_mb: number;
}

/* ---------------------- S9 钱包 · 支付 · 兑换 · 邀请返利 ---------------------- */

/**
 * 订单状态（S9 冻结枚举）：
 * pending 待支付 / paid 已支付 / refunding 退款中 / expired 已过期 / refunded 已退款 / need_manual 人工处理
 *
 * refunding 只出现在「原路退款已提交、结果待确认」期间：额度已扣、钱可能已退出，
 * 既不能当成 paid（会让客服误以为没退款），也不能当成 refunded（会误导用户「钱到账了」）。
 */
export type OrderStatus = "pending" | "paid" | "refunding" | "expired" | "refunded" | "need_manual";

/** 退款方式：gateway 原路退回 / balance 仅回退平台余额 */
export type RefundMode = "gateway" | "balance";
/** 退款状态：pending 已受理待确认 / success 已退款 / failed 退款失败（额度已回滚） */
export type RefundStatus = "pending" | "success" | "failed";

/** 套餐条目（基础/赠送/到账总额均服务端复算，前端不做算术） */
export interface WalletPackage {
  id: number;
  name: string;
  /** 售价（分，支付渠道最小单位） */
  price_cent: number;
  /** 基础到账 = 售价（分→微），由服务端算 */
  base: number;
  /** 赠送金额（微） */
  bonus: number;
  /** 到账总额（微）= 基础 + 赠送 */
  credited: number;
  /** 卡片角标；空串 = 门户不渲染任何角标 DOM */
  badge?: PackageBadge | "";
}

/**
 * 收银台支付方式选项。
 *
 * 只有「方式」一个维度：用户选的是「微信还是支付宝」，不是「走哪家聚合支付」。
 * 承接渠道由服务端按方式路由（service/order.resolveChannel 唯一实现），
 * 前端既拿不到 channel_id，也不需要它 —— 下单只回传 method。
 */
export interface PayOption {
  /** 支付方式枚举 code（wechat / alipay / aggregate），直接回传给下单接口 */
  method: string;
  /** 支付方式中文名（微信支付 / 支付宝 / 聚合支付），由后端单点提供 */
  label: string;
}

/** GET /api/portal/packages（套餐 + 钱包配置 + 支付方式） */
export interface WalletPackagesResult {
  items: WalletPackage[];
  custom_topup_enabled: boolean;
  /** 最低充值金额（微） */
  min_topup: number;
  /** 余额上限（微）；0 = 不限制 */
  balance_limit: number;
  /** 1 货币单位 = 多少微（格式化用，不参与到账计算） */
  scale: number;
  currency: string;
  currency_symbol: string;
  cny_per_usd: number;
  pay_options: PayOption[];
}

/**
 * 订单视图（**用户端**）。
 *
 * 只有平台该让用户看到的东西：订单号、金额、到账额度、状态、支付方式、退款进度。
 * 不含 pay_channel / pay_channel_id / pay_protocol —— 走哪家收单机构是供应链信息，
 * 用户只需要知道「我用微信付的」。管理台要那些字段，用 `AdminOrderItem`。
 */
export interface OrderItem {
  id: number;
  out_trade_no: string;
  trade_no?: string;
  /** 订单金额（分） */
  amount_cent: number;
  /** 到账金额（微） */
  credited: number;
  /** 订单币种（下单时固化） */
  currency?: string;
  status: OrderStatus;
  pay_method: string;
  /** 支付方式中文名（历史订单存原文、新订单存枚举码，展示一律用它） */
  pay_method_label?: string;
  subject?: string;
  /** 实时套餐名（含已下架套餐）；下单原文在 subject 里冻结 */
  package_name?: string;
  expire_at?: string | null;
  created_at: string;
  paid_at: string | null;
  /* —— 退款（仅在发起过退款后有值） —— */
  refund_status?: RefundStatus;
  refund_mode?: RefundMode;
  /**
   * **真的回到渠道**的钱（分）。仅回退平台余额（balance 模式）时为 0 ——
   * 这种情况一分钱都没退给用户，只是把额度扣回来了。
   */
  refund_amount_cent?: number;
  /** 实际扣回的平台额度（微）：balance 模式真正产生的动作金额 */
  refund_credited?: number;
  refund_reason?: string;
  refund_at?: string | null;
  refund_done_at?: string | null;
}

/** 管理台订单视图：在用户端字段之外补上归属、渠道与排障信息 */
export interface AdminOrderItem extends OrderItem {
  user_id: number;
  username?: string;
  /** 订单归属的应用；0 = 平台直连（门户/开放 API 用户自充） */
  app_id: number;
  /** 应用展示名（display_name 优先）；0 或查不到时为空串 */
  app_name?: string;
  /** 下单时的渠道展示快照（可能已改名，仅用于展示） */
  pay_channel: string;
  /** 稳定渠道 ID（筛选与对账用） */
  pay_channel_id: string;
  pay_protocol: string;
  /** 最近一次创建支付失败的原因（非空表示可点「重试」） */
  pay_error?: string;
  /** 上游退款单号（仅原路退款成功/受理后才有） */
  refund_no?: string;
  /** 退款异常原因（含「结果未知」提示） */
  refund_error?: string;
}

/** POST /api/admin/order/refund 的返回：退款结果三态，管理台据此提示 */
export interface OrderRefundOutcome {
  order_id: number;
  refund_mode: RefundMode;
  refund_status: RefundStatus;
  refund_no?: string;
  refund_amount_cent: number;
  /** 从余额扣回的金额（微） */
  balance_deducted: number;
  message: string;
}

/**
 * POST /api/portal/topup、/orders/{id}/continue、/orders/{id}/pay 的返回体。
 *
 * - cashier_url / pay_url：移动端或跳转式收银台的地址（两者同值，cashier_url 为兼容旧字段）
 * - qr_ref：上游二维码**原值**（可能是图片地址，也可能是二维码内容），不能直接当 img src
 * - expires_in：剩余有效期秒数。前端用「接收时刻 + expires_in」倒计时，
 *   **不要**用本地时钟减 expire_at —— 用户本机时间快 10 分钟就会一进页面就超时
 * - auto_redirect：绑定的收银台是 hosted 页（Stripe 一类），拿到地址即跳转。
 *   「哪种协议要自动跳转」是后端知识，前端不再按 pay_protocol 判断。
 */
export interface TopupResult {
  id: number;
  out_trade_no: string;
  amount_cent: number;
  currency?: string;
  credited: number;
  pay_method: string;
  /** 支付方式中文名（历史订单的 pay_method 是原文，这里统一给可直接展示的文案） */
  pay_method_label: string;
  cashier_url: string;
  pay_url: string;
  qr_ref: string;
  expires_in: number;
  auto_redirect?: boolean;
}

/** 二维码展示形态：image = 后端已代理成 data URL；content = 二维码内容（前端降级复制）；none = 无二维码 */
export type QRMode = "image" | "content" | "none";

export interface QRPayload {
  mode: QRMode;
  data_url?: string;
  content?: string;
}

/** GET /api/portal/orders/{id}/qrcode */
export interface QRResult {
  qr: QRPayload;
  expires_in: number;
}

/** POST /api/portal/orders/{id}/verify（「我已支付，立即核对」） */
export interface VerifyResult {
  order: OrderItem;
  /** false = 本次查单失败，**不代表未支付** */
  query_ok: boolean;
}

/** 管理台订单筛选用的渠道元信息（**含停用渠道**，否则停用后就筛不出历史订单） */
export interface PayChannelBrief {
  id: string;
  name: string;
  protocol: string;
  enabled: boolean;
  methods: string[];
}

/** 管理台订单列表：在分页四字段之外额外带渠道元信息 */
export interface AdminOrdersResult extends PageResult<AdminOrderItem> {
  pay_channels: PayChannelBrief[];
}

/** 返利行状态（服务端算好下发，前端不碰时钟） */
export type AffState = "cooling" | "available" | "transferred" | "voided";

/** GET /api/portal/aff（邀请返利面板） */
export interface AffInfo {
  aff_code: string;
  aff_count: number;
  /** 已到期、可转入余额的返利（微） */
  aff_available: number;
  /** 冷却中、暂不可转出的返利（微） */
  aff_cooling: number;
  /** 历史累计返利（微，含已转出与冷却中，不含已作废） */
  aff_history: number;
  enabled: boolean;
  /** 最低转出金额（微） */
  min_transfer: number;
  /** 邀请人返利比例（微比例，1e6 = 100%） */
  inviter_rate: number;
  /** 被邀请人每笔赠金（微） */
  invitee_amount: number;
  /** 参与返利的充值笔数（前 N 笔） */
  max_rewards: number;
  /** 返利冷却期（小时，24 ~ 720） */
  hold_hours: number;
  /** 配置缺失/越界，服务端已按收敛值工作：页面必须提示，不能显示成「正常」 */
  degraded: boolean;
}

/**
 * 返利行的受益人身份。
 * inviter = 我邀请的人充值，我按比例拿到的返利；
 * invitee = 我被邀请，我的充值按笔拿到的赠金。
 */
export type AffRole = "inviter" | "invitee";

/** GET /api/portal/aff/records 行（订单粒度，对方用户名已脱敏） */
export interface AffRecord {
  /** 交易对手：role=inviter 时是被邀请人，role=invitee 时是我的邀请人 */
  counterpart: string;
  role: AffRole;
  order_id: number;
  /** 本单实付（微） */
  base_amount: number;
  /** 我这笔拿到的返利（微） */
  reward: number;
  state: AffState;
  available_at: string;
  created_at: string;
}

/** GET /api/portal/transactions 行（资金流水，含余额快照保证跨页一致） */
export interface TransactionItem {
  id: number;
  type: string;
  /** 变动金额（微，带符号） */
  amount: number;
  /** 变动后余额（微） */
  balance_after: number;
  reason: string;
  created_at: string;
}

/* ---------------------- S10 系统配置中心与通知 ---------------------- */

/** 站点品牌（GET /api/portal/config 的 site 段）= 后端 sitecfg.Site */
export interface SiteInfo {
  site_name: string;
  logo_image: string;
  /** 浏览器标签图标（dataURI）；空则前端回落 Logo 图片，再回落站名首字 */
  favicon_image: string;
  site_url: string;
  /** 对外 API 地址（origin，**不含 /v1**）；首条为主，供回调/产物直链/示例 */
  api_base_urls: string[];
  contact_email: string;
  footer_text: string;
  icp_beian: string;
  /** 结算币种（CNY/USD），站点级锁定。 */
  currency?: string;
  /** 展示符号（由币种派生，不可自定义：改符号没有任何合法用途） */
  currency_symbol?: string;
}

/**
 * 对外地址的展示口径（GET /api/portal/config 的 api 段）。
 *
 * 三条来源必须能区分：运营没填地址时门户展示的是兜底值，
 * 用户复制走一条「看起来是平台域名、实际不通」的地址比空着更糟。
 */
export interface ApiEndpoints {
  /** 已按「配置 → 站点地址 → 请求 origin」回落好的地址（origin，不含 /v1） */
  bases: string[];
  source: "config" | "site_url" | "request";
}

/** GET /api/portal/config（公开） */
export interface PortalConfig {
  site: SiteInfo;
  api: ApiEndpoints;
  /** 结算口径（币种/符号/微刻度），随 site 一起下发供首屏金额展示 */
  currency: string;
  currency_symbol: string;
  /** 1 货币单位 = 多少微（后端 money.Scale）。前端格式化唯一依据。 */
  scale: number;
  other_currency: string;
  cny_per_usd: number;
  about_content: string;
  agreement_content: string;
  /** 认证策略（注册准入 / 验证码 / 会话），登录注册页据此渲染 */
  auth: AuthConfig;
  /**
   * 运营模式：false = 纯服务端（门户站点关闭，仅管理员可登录）。
   *
   * 缺字段按**开启**处理（与后端 conf.Bool 的默认值同语义）：
   * 把正常站点挡在门外，比多放一个请求糟糕得多。
   */
  portal_enabled?: boolean;
  custom_topup_enabled: boolean;
  min_topup: number;
  /** 邮件通道就绪（用户中心「绑定邮箱」入口据此禁用；与注册渠道策略无关） */
  email_enabled: boolean;
  /** 短信通道就绪（用户中心「绑定手机」入口据此禁用；与注册渠道策略无关） */
  sms_enabled: boolean;
}

/** 公告类型（5 枚举，冻结） */
export type AnnouncementType = "default" | "ongoing" | "success" | "warning" | "error";

/** 公告条目（**配置侧**模型：管理台编辑并写回 system_config 的就是它）；id 为已读锚（同 id 编辑内容不重现，新 id 才重现） */
export interface AnnouncementItem {
  id: number;
  content: string;
  publishDate: string;
  type: AnnouncementType;
  extra: string;
}

/**
 * 门户侧条目 = 配置条目 + 当前用户已读状态。
 *
 * 必须与 AnnouncementItem 分开：把 read 塞进配置模型，管理台保存时会把
 * 「某个用户的已读状态」写回 system_config。
 */
export interface PortalAnnouncementItem extends AnnouncementItem {
  read: boolean;
}

/**
 * GET /api/portal/announcements 与 POST /read 的**同一份**响应。
 *
 * 不再下发 `unread`：未读 = items.filter(i => !i.read).length，接口只保留一处表达，
 * 避免「徽标数字」与「条目状态」各算一遍后对不上。
 */
export interface AnnouncementsResult {
  enabled: boolean;
  items: PortalAnnouncementItem[];
}

/* ------ S15 门户主页聚合 ------ */

/** 用户区间用量汇总（今日 / 本月同构） */
export interface UserUsageStat {
  /** 请求数（含 pending 与 rejected） */
  requests: number;
  /** 失败数（released + rejected） */
  errors: number;
  /** 进行中（只有 today 有意义；month 直接沿用 today 的值） */
  pending: number;
  /** 区间消耗（微） */
  charge: number;
  prompt_tokens: number;
  completion_tokens: number;
}

/**
 * 用户趋势单点。
 *
 * **两个金额都是微**，可以直接相减、同图绘制，页面不需要任何换算。
 * 历史教训：这里曾下发 `recharge_cent`（分）+ `charge`（微），于是每个消费方
 * 都要自己乘一万；漏一处就是「充值那条线贴着 0」。
 */
export interface UserTrendPoint {
  date: string;
  /** 消耗（微） */
  charge: number;
  /** 充值（微，净额：含赠送、已扣退款） */
  recharge: number;
}

/** GET /api/portal/overview?days=7|30（余额/冻结由 /api/portal/me 独占提供，故不在此结构内） */
export interface PortalOverview {
  today: UserUsageStat;
  month: UserUsageStat;
  /** 本月净充值（微） */
  month_recharge: number;
  trend: UserTrendPoint[];
  active_tasks: number;
  /** 取数时刻（业务时区 RFC3339）。页面不轮询，这就是数字的真实年龄 */
  generated_at: string;
}

/**
 * GET /api/portal/defaults —— 各模态的默认模型与「是否被任一密钥授权」。
 *
 * 键固定为 8 类模态；`model` 为空串表示该模态没有配默认模型。
 * 这是默认模型的**唯一真相源**：不要再从 /api/pricing 里筛 is_default
 * （那个字段没有写入口，永远不会有值）。
 */
export type PortalDefaults = Record<
  string,
  { model: string; any_key_allowed: boolean }
>;

/** 支付渠道条目（system_config.pay_channels） */
export interface PayChannel {
  id: string;
  name: string;
  /** 支付协议：epay / xunhupay / 空（未接入的占位渠道） */
  protocol: string;
  enabled: boolean;
  methods: string[];
  params: Record<string, string>;
}

/**
 * 支付渠道参数声明（后端 `payment.ParamSpec`）。
 *
 * 参数表单由它驱动，**不再遍历 `params` 里存了哪些键** —— 否则同一个协议下
 * 老渠道与新建渠道的字段集不一致，运营在 A 卡能填的框在 B 卡找不到。
 */
/** 支付渠道参数显隐条件（只在渲染层生效，后端不据此跳过校验） */
export interface PayChannelShowWhen {
  /** 虚拟开关的 key（不是渠道参数键，不落库） */
  field: string;
  /** 开关取该值时才显示本字段 */
  value: string;
}

/** 表单里的虚拟单选控件：不落库，只控制字段可见性 */
export interface PayChannelSwitchOption {
  value: string;
  label: string;
}

export interface PayChannelSwitch {
  key: string;
  label: string;
  default: string;
  options: PayChannelSwitchOption[];
}

export interface PayChannelParamSpec {
  key: string;
  label: string;
  /**
   * 分组标识。**当前恒为 "basic"**（高级设置折叠区已移除）。
   *
   * 保留这个字段是因为前端是独立构建的：旧 bundle 用 `group === "basic"`
   * 过滤参数，后端一旦不下发就会出现「参数区整片空白且不报错」。
   */
  group: "basic" | "advanced";
  required: boolean;
  /** 密码框 + 「已设置」标记 + 落库脱敏 */
  secret: boolean;
  /** 历史兼容：true 表示不渲染（本版本已无字段使用） */
  hidden: boolean;
  /**
   * 用多行文本框（PEM 类字段）。与 secret 组合时：secret 只控制**落库脱敏**
   * （回显 `******`），不控制输入掩码 —— 六行星号既无法核对也无法编辑。
   */
  multiline?: boolean;
  placeholder?: string;
  /** 问号气泡文案（直观可推断的字段留空） */
  tip?: string;
  default?: string;
  /** 只在虚拟开关取到某值时显示（微信的验签材料二选一） */
  show_when?: PayChannelShowWhen;
}

/** 支付渠道协议声明（后端 `payment.ChannelSchema`） */
export interface PayChannelSchema {
  protocol: string;
  label: string;
  /** false = 只有占位、无下单实现（不允许新增） */
  implemented: boolean;
  /** single = 一渠道一方式（虎皮椒/官方直连）；multi = 一渠道多方式（易支付） */
  method_mode: "single" | "multi";
  /**
   * 该协议**允许**的支付方式白名单（如 wxpay 只允许 wechat）。
   *
   * 缺失 = 不限制（连旧后端时的兜底）。`method_mode` 只表达「选几个」，
   * 表达不了「能选哪些」——所以两者需要同时存在。
   */
  methods?: string[];
  /** 方式区问号文案（各协议理由不同，不能在前端写死） */
  method_help?: string;
  /** 虚拟单选控件（目前只有微信的「验签方式」） */
  switches?: PayChannelSwitch[];
  params: PayChannelParamSpec[];
}

/** 渠道专属参数（键 → 值；必填键随渠道而定） */
export type ChannelParams = Record<string, string>;

/**
 * 云存储（system_config.storage）
 *
 * `active` 是**写入位置**的唯一判据：none = 本地磁盘，其余为对应云后端。
 * 缺这个键时后端直接拒绝写入（503 storage_not_configured），不会回落本地。
 */
export interface StorageSettings {
  active: string;
  channels: Record<string, ChannelParams>;
}

/**
 * 文件上传配置（system_config.uploads；与任务产物保留相互独立）
 *
 * `types` + `limits` + `compress` 是本次新增的三项，也是唯一真相源：
 * 后端不再有扩展名白名单，也没有「全局单文件上限」这种并行口径。
 */
export interface UploadsSettings {
  enabled: boolean;
  daily_limit_per_key: number;
  retention_hours: number;
  user_total_mb: number;
  concurrency: number;
  /** 后缀 → 文件类别（image / audio / video / document / text） */
  types: Record<string, string>;
  /** 类别 → 单文件上限（MB） */
  limits: Record<string, number>;
  compress: UploadCompress;
}

/** 图片压缩：只作用于文件上传接口 */
export interface UploadCompress {
  enabled: boolean;
  /** 1–100，仅对 JPEG / WebP 生效 */
  quality: number;
  /** original 保持原后缀；jpeg / webp 统一转码 */
  format: "original" | "jpeg" | "webp";
}

/** 单场景邮件模板（变量 {{site_name}} / {{code}} 发送时填充） */
export interface EmailTpl {
  subject: string;
  body: string;
}

/** 邮件通知（system_config.email） */
export interface EmailSettings {
  enabled: boolean;
  smtp_host: string;
  smtp_port: number;
  encryption: string;
  username: string;
  password: string;
  from_addr: string;
  sender_name: string;
  scenes: Record<string, boolean>;
  templates: Record<string, EmailTpl>;
}

/** 短信通知（system_config.sms） */
export interface SmsSettings {
  enabled: boolean;
  active: string;
  daily_limit: number;
  channels: Record<string, ChannelParams>;
}

/** 邀请返利（system_config.aff；数值项仅启用时校验） */
export interface AffSettings {
  enabled: boolean;
  /** 邀请人返利比例（微比例，1e6 = 100%；上限 100%） */
  inviter_rate: number | null;
  /** 被邀请人每笔赠金（微，且不超过该笔实付） */
  invitee_amount: number | null;
  /** 参与返利的充值笔数（1 ~ 100） */
  max_rewards: number | null;
  /** 返利冷却期（小时，0 ~ 720；0 = 不冷却） */
  hold_hours: number | null;
  /** 最低转出金额（微） */
  min_transfer: number | null;
  /** 单邀请人 24h 最多绑定几个新用户（0/缺省 = 20） */
  daily_invite_cap?: number | null;
}

/** 渠道监控全局熔断参数（system_config.monitor） */
export interface MonitorSettings {
  auto_disable: boolean;
  fail_threshold: number;
  fail_window: number;
  auto_enable: boolean;
  probe_interval_min: number;
}

/** 站点公告（system_config.announcements） */
export interface AnnouncementSettings {
  enabled: boolean;
  items: AnnouncementItem[];
}

/** 默认限流兜底（system_config.rate_limit_defaults） */
export interface RateLimitDefaults {
  rpm: number;
  concurrency: number;
}

/** 人机验证（`system_config.auth_captcha`） */
export interface AuthCaptchaSettings {
  /**
   * 只有登录可配。注册与找回恒为「每次都要」（后端常量，不进配置）。
   * after_fails 的阈值是 `auth_login.fail_threshold`，不在这里。
   */
  scenes: { login: "always" | "after_fails" | "off" };
}

/**
 * 登录失败处置（`system_config.auth_login`）。
 *
 * 刻意没有「锁定 X 分钟」：账号维度的硬锁等于给攻击者一个锁死任意账号的开关。
 * 达阈值后的处置是「要求人机验证 + 服务端随机退避」，只有单 IP 维度才有硬闸。
 */
export interface AuthLoginSettings {
  /** 第几次失败起要求人机验证 */
  fail_threshold: number;
  /** 失败计数窗口阶梯（秒）；第 n 次失败对应第 n 档 */
  backoff_seconds: number[];
  /** 单 IP 失败硬闸（唯一会返回 429 的地方） */
  ip_fail_limit: number;
  ip_fail_window_sec: number;
  /** 达阈值后服务端随机退避上限（毫秒） */
  delay_max_ms: number;
}

/** 会话生命周期（`system_config.auth_session`） */
export interface AuthSessionSettings {
  absolute_ttl_sec: number;
  idle_ttl_sec: number;
  max_active: number;
  touch_interval_sec: number;
}

/** 注册准入（`system_config.auth_register`） */
export interface AuthRegisterSettings {
  mode: "open" | "invite_only" | "closed";
  contact_verify: "required" | "off";
  contact_channels: ContactChannel[];
}

/** 二次验证（`system_config.auth_mfa`）。required_for_admin：管理员访问 /api/admin/* 前必须先绑定 TOTP，
 * 保存该值时后端要求**操作者本人已绑定**（否则 400），未绑定会被拦到 /mfa/setup。 */
export interface AuthMfaSettings {
  policy: "off" | "optional" | "required_for_admin";
}

/** GET /api/admin/config 的 config 段（32 键全量；敏感叶子以 Mask 脱敏回显） */
export interface AdminConfigValues {
  /** 运营模式总开关：false = 纯服务端（门户站点关闭，仅管理员可登录） */
  portal_enabled?: boolean;
  /**
   * 发布清单地址（release.json，一键更新的"有没有新版本"来源）。
   * 空串 = 不检查；保存时要求 https + 公网地址。
   */
  release_manifest_url?: string;
  site: SiteInfo;
  /**
   * 计费分组折扣（**微比例**：1000000 = 不打折，900000 = 九折）。
   *
   * 键 = 计费分组名（与用户/密钥的 group 字段对应）。这就是"分层定价"的全部配置，
   * 没有倍率、没有刻度、没有第二处能打折的地方。
   */
  group_discount: Record<string, number>;
  /** 参考汇率（只用于次币种展示与 OpenAI 协议报价，永不参与扣费） */
  cny_per_usd: number;
  /** 余额上限（微）；0 = 不限制 */
  balance_limit: number;
  /** 最低充值金额（微） */
  min_topup: number;
  custom_topup_enabled: boolean;
  pay_channels: PayChannel[];
  retry_times: number;
  file_retention_days: number;
  poll_deadline_min: number;
  logs_retention_days: number;
  archive_retention_days: number;
  audit_retention_days: number;
  rate_limit_defaults: RateLimitDefaults;
  auth_captcha: AuthCaptchaSettings;
  auth_login: AuthLoginSettings;
  auth_session: AuthSessionSettings;
  auth_register: AuthRegisterSettings;
  auth_mfa: AuthMfaSettings;
  auth_event_retention_days: number;
  storage: StorageSettings;
  uploads: UploadsSettings;
  email: EmailSettings;
  sms: SmsSettings;
  aff: AffSettings;
  monitor: MonitorSettings;
  announcements: AnnouncementSettings;
  about_content: string;
  agreement_content: string;
  modality_defaults: Record<Modality, string>;
}

/** GET /api/admin/config */
export interface AdminConfigResult {
  config: AdminConfigValues;
  /** 敏感字段脱敏占位（提交时原样回传表示未改动） */
  mask: string;
  /**
   * 各协议的参数声明（后端 `payment.Schemas()`）。
   *
   * 前端据此渲染渠道参数表单；缺失时回落到 `lib/payChannelSchema.ts` 的 FALLBACK_SCHEMAS
   * （滚动发布期间可能连到旧后端）。
   */
  pay_channel_schemas?: PayChannelSchema[];
  /**
   * 新增渠道用的空白参数模板（按协议）。
   *
   * **必须放在 config 外层**：前端把 `config` 当 27 键全量包原样提交，
   * 塞进去会多写一个后端白名单外的键，保存直接报错。
   *
   * @deprecated 新前端改用 `pay_channel_schemas`；仅作旧前端兜底，下个版本删除。
   */
  pay_channel_templates?: Record<string, Record<string, string>>;
  /** 回调地址前缀：`{notify_base}{渠道ID}/notify` */
  notify_base?: string;
  /** 可选支付方式（后端单点，前端不维护映射表） */
  pay_method_options?: { value: string; label: string }[];
  /** 孤儿分组（有账号挂在上面但没配折扣 → 按不打折计费），供分组折扣表单上方提示 */
  orphan_groups?: string[];
  /**
   * 云存储渠道 schema（唯一真相源是后端 `systemconfig.storageSchemes`）。
   *
   * 前端不再自带一份渠道字段列表：两份列表必然漂移，而漂移的表现是
   * 「后端支持的字段在前端填不了」（例如 COS 的私有化域名）。
   */
  storage_schemes?: StorageScheme[];
  /**
   * 当前**实际生效**的写入位置（后端 `storage.Effective()`）。
   *
   * 与 `config.storage.active` 不同：那个是表单草稿值，这个是进程里正在用的值。
   * `writable=false` 表示没有可用配置 —— 此时上传已经 503，页面必须显性提示，
   * 否则「保存过云存储」与「现在真的在写云」看起来一模一样。
   */
  storage_effective?: StorageEffective;
}

/** 当前生效的存储写入位置 */
export interface StorageEffective {
  active: string;
  label: string;
  bucket: string;
  endpoint: string;
  writable: boolean;
}

/** 云存储渠道渲染 schema（fields = [键, 标签, 是否必填]） */
export interface StorageScheme {
  id: string;
  name: string;
  fields: [string, string, string][];
}

/** POST /api/admin/storage/test（探针结果；连通失败也返回 200） */
export interface StorageTestResult {
  ok: boolean;
  backend: string;
  message: string;
}

/* ---------------------- S11 管理台数据面与可观测 ---------------------- */

/** 看板今日指标：**全部金额单位微**（充值、消耗、成本同一量纲，可直接相减） */
export interface BoardToday {
  requests: number;
  /** 今日充值（微） */
  recharge: number;
  /** 今日消耗（微） */
  charge: number;
  /** 今日上游成本（微） */
  cost: number;
  /** 成本是否已采集（false 时成本/毛利按未采集展示） */
  cost_collected: boolean;
  success_rate: number;
  channels_online: number;
  channels_total: number;
  /** 启用但未配价的模型数（§4.9.6 看板计数） */
  price_missing_count: number;
}

/** 趋势点（近 7 日）—— 三列都是微 */
export interface BoardTrendPoint {
  date: string;
  /** 充值（微） */
  recharge: number;
  /** 消耗（微） */
  charge: number;
  /** 上游成本（微） */
  cost: number;
}

/** 排行项（label 展示名 / value 指标值） */
export interface BoardRank {
  label: string;
  value: number;
  /** 实体图标地址（模型榜/失败榜=模型图标，渠道榜=渠道图标；用户榜没有） */
  icon_url?: string;
}

/** 渠道健康（含熔断态） */
export interface BoardChannelHealth {
  id: number;
  name: string;
  /** 渠道图标地址（空 = 首字占位） */
  icon_url?: string;
  status: ChannelStatus;
  auto_ban: boolean;
  fail_rate: number;
  success_count: number;
  fail_count: number;
}

/** 用户聚合明细行（分组取用户当前计费分组） */
export interface BoardUserRow {
  user_id: number;
  username: string;
  group: string;
  requests: number;
  /** 消耗（微） */
  charge: number;
}

/** 单档聚合（d7 / d30），看板切换零请求 */
export interface BoardRange {
  user_top: BoardRank[];
  model_top: BoardRank[];
  users: BoardUserRow[];
}

/** GET /api/admin/data/board */
export interface BoardResult {
  today: BoardToday;
  trend: BoardTrendPoint[];
  fail_top: BoardRank[];
  /** 兼容旧后端响应：字段可能缺失（前端端独立部署窗口），消费处须按空态兜底 */
  channel_top?: BoardRank[];
  channels: BoardChannelHealth[];
  boards: { d7: BoardRange; d30: BoardRange };
}

/** 派生事件类型（§4.9.9，由后端按 status + http_status 派生） */
/**
 * 事件类型（查询层派生）。
 *
 * `rejected:*` 是预检期拒绝（一次都没打到上游、0 费），与 `error`（打出去后失败）
 * 的责任方完全不同，前端必须能分开看 —— 否则「失败率高」会被误读成渠道问题。
 */
export type LogEventKind =
  | "consume"
  | "client_abort"
  | "error"
  | "upstream_error"
  | "pending"
  | "rate_limited"
  | "auth_error"
  | "insufficient_balance"
  | "invalid_request"
  | string;

/**
 * 容量体检快照（GET /api/admin/data/capacity，B7）。
 *
 * `free_ratio` 为 -1 表示磁盘信息不可用（此时 `error` 非空），不要当成 0% 可用去渲染告警条：
 * 那会把「监控瞎了」显示成「磁盘满了」，处置动作完全相反。
 */
export interface CapacitySnapshot {
  files: number;
  bytes: number;
  disk_total: number;
  disk_avail: number;
  free_ratio: number;
  warn: boolean;
  warning: string;
  at: string;
  error?: string;
  stats_error?: string;
}

/** GET /api/admin/data/capacity 的完整响应（快照 + 阈值 + 开关状态） */
export interface AdminCapacityResult {
  snapshot: CapacitySnapshot;
  thresholds: { warn_percent: number; min_free_gb: number };
  task_retention_days: number;
  metrics_enabled: boolean;
}

/** GET /api/admin/logs 明细行 */
export interface AdminLogItem {
  id: number;
  created_at: string;
  user_id: number;
  username: string;
  key_id: number;
  key_name: string;
  /** 这条调用归属的应用（经 key_id 派生）；0 = 平台直连 */
  app_id: number;
  /** 应用展示名（display_name 优先）；0 或查不到时为空串 */
  app_name?: string;
  model: string;
  /** 模型图标地址（已回落供应商） */
  model_icon_url?: string;
  channel_id: number;
  channel_name: string;
  /** 渠道图标地址（渠道没有回落语义，空 = 首字） */
  channel_icon_url?: string;
  /** 适配器标识：spec:<slug> 表示数据驱动规格（排障时先看这个） */
  adapter_id: string;
  modality: string;
  kind: LogEventKind;
  status: string;
  http_status: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  /** 扣费（微） */
  charge: number;
  /** 上游成本（微） */
  cost?: number;
  cache_tokens?: number;
  image_tokens?: number;
  audio_input_tokens?: number;
  audio_output_tokens?: number;
  /** 总耗时（finished_at − started_at）；进行中为 0，用 elapsed_ms 展示 */
  latency_ms: number;
  elapsed_ms?: number;
  upstream_ms?: number;
  first_token_ms?: number;
  attempts?: number;
  started_at?: string | null;
  finished_at?: string | null;
  error_msg: string;
  is_stream: boolean;
  ip: string;
  request_id?: string;
  upstream_request_id?: string;
  call_kind?: string;
  task_id?: number;
  group?: string;
  /** 计费来源与异常标记（S8 整改，可选） */
  usage_source?: string;
  anomaly?: string;
}

/** GET /api/admin/logs/analytics */
export interface LogAnalyticsResult {
  kinds: BoardRank[];
  errors: { label: string; value: number; share: number }[];
}

/** GET /api/admin/audits */
export interface AdminAuditItem {
  id: number;
  action: string;
  admin_id: number;
  actor: string;
  target_type: string;
  target_id: string;
  detail: Record<string, unknown> | null;
  created_at: string;
}

/** GET /api/admin/audits/meta */
export interface AuditMetaResult {
  actions: { action: string; value: number }[];
  retention_days: number;
}

/**
 * GET /api/admin/auth-events 的列表行（认证事件审计）。
 *
 * `label` 是后端补的中文名：事件白名单在 Go 侧，前端再维护一份映射迟早会漂，
 * 而漂了的表现只是下拉里多一个英文 key，不报错。`detail` 可能为 null。
 */
export interface AuthEventItem {
  id: number;
  uid: number;
  account: string;
  event: string;
  label: string;
  result: "success" | "fail";
  reason: string;
  ip: string;
  ua: string;
  session_id: number;
  detail: Record<string, unknown> | null;
  created_at: string;
}

/** GET /api/admin/auth-events/meta：可选事件（编译期白名单）+ 保留天数 */
export interface AuthEventMetaResult {
  events: { event: string; label: string }[];
  retention_days: number;
}

/** S16 兑换码派生状态（唯一来源为后端 redeem.StatusOf） */
export type RedeemStatus = "available" | "used" | "expired";

/** GET /api/admin/redeems 的列表行 */
export interface RedeemItem {
  id: number;
  code: string;
  batch: string;
  /** 面额（微） */
  amount: number;
  status: RedeemStatus;
  used_by: number | null;
  used_at: string | null;
  expires_at: string | null;
  created_at: string;
}

/** POST /api/admin/redeems */
export interface RedeemGenerateResult {
  created: number;
  /**
   * 本批明文码（按生成顺序）。
   *
   * 可选：老后端只回 created，前端需回落到"按批次查列表"。
   * 有了它，运营生成完就能直接复制，不必再去列表页按批次筛（列表是分页的，
   * 一屏 20 条，生成 500 条时第一批根本看不到）。
   */
  codes?: string[];
}

/** POST /api/admin/redeems/delete */
export interface RedeemDeleteResult {
  deleted: number;
  skipped: number;
}


/* ---------------------- 充值套餐（后台配置） ---------------------- */

/**
 * 套餐角标：三种取值共用同一视觉（.pkg-ribbon），只换文案。
 * 不做差异化配色 —— 每加一种就要同步改 CSS，收益不抵维护成本。
 */
export type PackageBadge = "recommend" | "hot" | "value";

/** 管理台套餐行（额度由服务端按当前充值比例复算，前端不自己算业务值） */
export interface AdminPackageItem {
  id: number;
  name: string;
  /** 售价（分） */
  price_cent: number;
  /** 基础到账与服务端复算值（微） */
  base: number;
  bonus: number;
  credited: number;
  enabled: boolean;
  sort_order: number;
  badge: PackageBadge | "";
  /** 引用计数：**含已删除订单**，与「删除保护」同一口径 */
  order_count: number;
  created_at: string;
  updated_at: string;
}

/** GET /api/admin/packages：分页 + 计算所需的配置快照（前端不硬编码常量） */
export interface AdminPackagesResult {
  items: AdminPackageItem[];
  total: number;
  page: number;
  page_size: number;
  /** 余额上限（微）；0 = 不限制 */
  balance_limit: number;
  /** 1 货币单位 = 多少微（格式化用） */
  scale: number;
  currency: string;
  currency_symbol: string;
  /** 售价区间（分）与赠送上限（微），都是服务端下发的边界，前端不硬编码 */
  min_price_cent: number;
  max_price_cent: number;
  max_bonus: number;
}

export interface AdminPackagesParams {
  page?: number;
  page_size?: number;
  keyword?: string;
  enabled?: "" | "0" | "1";
  sort?: string;
}

/** POST/PUT /api/admin/packages 的请求体（enabled 仅创建时生效） */
export interface PackagePayload {
  name: string;
  /** 售价（分） */
  price_cent: number;
  /** 赠送金额（微） */
  bonus: number;
  enabled?: boolean;
  sort_order?: number;
  badge?: PackageBadge | "";
}

/** 保存成功但存在运营风险时的提示（当前只有「到账额度超充值上限」一种） */
export interface PackageWarning {
  code: string;
  message: string;
}

export interface PackageWriteResult {
  item: AdminPackageItem;
  warning: PackageWarning | null;
}

/* ============================================================
   应用管理（对外 API 的归属主体）
   ============================================================ */

/** 管理台应用列表的一行（含聚合口径与卡片展示字段） */
/**
 * GET /api/admin/apps/options 的下拉项（**不含**卡片那套聚合）。
 *
 * 与 `AdminAppItem` 分开是刻意的：卡片视图每行要打四五条聚合查询，
 * 而筛选下拉只需要 id 与名字。两个形状共用一个类型会让"加个字段"变成
 * 一次全量聚合的代价。
 */
export interface AdminAppOption {
  id: number;
  slug: string;
  name: string;
  /** 对外品牌名（可能为空串）：渲染按 `display_name || name` */
  display_name: string;
  /** 1 启用 / 2 停用（停用也要能筛：历史订单/日志指向它） */
  status: number;
}

export interface AdminAppItem {
  id: number;
  slug: string;
  name: string;
  description: string;
  /** 1 启用 / 2 停用 */
  status: number;
  /** false = 关闭了「用户自助签发托管密钥」（只有运营能发凭证） */
  allow_self_issue: boolean;
  /** allow_self_issue 的派生展示值 */
  access_mode: "self_service" | "approval";
  display_name: string;
  tagline: string;
  logo_url: string;
  /** 维护模式：与 status 正交，开启后托管密钥调用 /v1 返回 503 */
  maintenance_mode: boolean;
  maintenance_message: string;
  created_at: string;
  key_count: number;
  active_key_count: number;
  user_count: number;
  /** 累计消费（微） */
  used: number;
  /** 已支付订单总额（分） */
  paid_cent: number;
  paid_count: number;
  /** 近 7 日调用次数 */
  calls_7d: number;
  /** 此刻生效的公告数 */
  notice_count: number;
  latest_release: AdminAppLatestRelease | null;
}

export interface AdminAppLatestRelease {
  version: string;
  published_at: string;
  force_update: boolean;
}

/**
 * 应用统计（概览图 + 在线指标）。
 *
 * 只有"随时间与流量变化"的数字。四格汇总（消费/充值/密钥/用户）不在这里 ——
 * 它们由列表接口随卡片下发，弹窗直接用列表行，所以卡片与弹窗不可能对不上。
 */
export interface AdminAppStats {
  trend: { date: string; requests: number; errors: number; charge: number }[];
  online: {
    online: "online" | "idle" | "offline";
    last_call_at: string;
    calls_24h: number;
    errors_24h: number;
    error_rate: number;
    /** 微 */
    charge_24h: number;
  };
}

/**
 * 应用品牌（GET / PUT /admin/apps/{id}/brand 同形）。
 *
 * 比对外品牌多一个 `logo_icon`：图标控件靠令牌回填（URL 反解不出令牌）。
 * 令牌不下发给终端 —— 它编码了存储位置，终端拿到也没用。
 */
export interface AdminAppBrand {
  display_name: string;
  tagline: string;
  intro: string;
  logo_icon: string;
  logo_url: string;
  contact_name: string;
  contact_email: string;
  support_url: string;
  privacy_url: string;
  terms_url: string;
}

/** PUT /admin/apps/{id}/brand 的请求体（logo 传令牌，不传 URL） */
export interface AppBrandPayload {
  display_name: string;
  tagline: string;
  intro: string;
  logo_icon: string;
  contact_name: string;
  contact_email: string;
  support_url: string;
  privacy_url: string;
  terms_url: string;
}

export interface AdminAppNotice {
  id: number;
  title: string;
  content: string;
  level: "info" | "warn" | "critical";
  enabled: boolean;
  /** RFC3339；null = 不限 */
  starts_at: string | null;
  ends_at: string | null;
  /** 此刻是否会下发给终端 */
  active: boolean;
  created_at: string;
}

export interface AppNoticePayload {
  title: string;
  content: string;
  level: AdminAppNotice["level"];
  /** 只在**创建**时生效；上下架走 PATCH /notices/:nid/status，见 AppNoticeUpdatePayload。 */
  enabled: boolean;
  starts_at: string | null;
  ends_at: string | null;
}

/**
 * PUT 的请求体：**不含 enabled**。
 *
 * 编辑表单里没有上下架开关，提交它只会制造跨端点覆盖：运营 A 刚点下架、
 * 运营 B 的旧弹窗一保存就把公告重新上架并推向全量终端。
 */
export type AppNoticeUpdatePayload = Omit<AppNoticePayload, "enabled">;

export interface AdminAppRelease {
  id: number;
  version: string;
  release_notes: string;
  download_url: string;
  force_update: boolean;
  /** 1 草稿 / 2 已发布 / 3 已撤回 */
  status: number;
  published_at: string | null;
  created_at: string;
}

export interface AppReleasePayload {
  version: string;
  release_notes: string;
  download_url: string;
  force_update: boolean;
}

/**
 * 版本列表响应：分页字段 + 「当前对外下发」的版本 id。
 *
 * 同一应用允许并存多个已发布（hotfix 回填是合法操作），对外只下发
 * published_at 最新的那个；不给这个 id，UI 无法标出到底哪个在生效。
 */
export interface AdminAppReleasePage {
  items: AdminAppRelease[];
  total: number;
  page: number;
  page_size: number;
  current_release_id: number | null;
}

/** POST/PUT /api/admin/apps 的请求体（slug 仅创建时生效，创建后不可改） */
export interface AppPayload {
  slug?: string;
  name: string;
  description?: string;
  /** 是否允许任意登录用户自助签发托管密钥（缺省=true）。关闭后新签发 403，轮换不受影响。 */
  allow_self_issue?: boolean;
}

/** 应用下的一把托管密钥（后台视角：无明文、无密文） */
export interface AdminAppKeyItem {
  id: number;
  user_id: number;
  username: string;
  key_prefix: string;
  status: number;
  /** 该密钥累计消费（微） */
  used: number;
  scopes: string[];
  created_at: string;
  /** 轮换时间；未轮换为空串 */
  rotated_at: string;
}

/* ============================================================
   应用插件（声明式）
   ============================================================ */

/**
 * 插件字段描述。
 *
 * 这是**后端编译下来的产物**，不是前端对 manifest 的解释：前端拿到它就能渲染，
 * 不需要知道 manifest 里 `columns` 写的是字段名、`type` 有哪些取值。
 */
export interface PluginField {
  key: string;
  label: string;
  /** string / text / int / bool / enum / date / link */
  type: string;
  required: boolean;
  maxLen: number;
  options?: string[];
  default?: unknown;
  /** 字段下方的常驻说明（填什么）；与 error 同一位置，错误优先 */
  help?: string;
  /** 输入框占位提示 */
  placeholder?: string;
  /** 表单里禁改（**不是安全边界**，服务端判据仍只有 manifest 与安装状态） */
  readOnly?: boolean;
  /** 仅 int：允许范围 */
  min?: number;
  max?: number;
}

/** 表单分段（投影后每个字段恰好出现一次，未声明的字段已被追加到末段） */
export interface PluginFormSection {
  /** 空串 = 不渲染段头（未声明 form 的实体就是这种） */
  title: string;
  columns: 1 | 2;
  fields: string[];
}

/** 列表列（已解析成 label/type，前端不再回查字段表） */
export interface PluginColumn {
  key: string;
  label: string;
  type: string;
}

/** 可排序白名单的一项（`order` 是该字段被选中时的默认方向） */
export interface PluginSort {
  key: string;
  label: string;
  type: string;
  order: "asc" | "desc";
}

/**
 * 可筛选白名单的一项。
 *
 * `op` 是**行为声明**：前端按它选控件（eq→下拉/开关三态、range→两个输入框、
 * contains→搜索框），服务端按它拼 WHERE。不要在类型上再推一遍算子逻辑。
 */
export interface PluginFilter {
  key: string;
  label: string;
  type: string;
  op: "eq" | "range" | "contains";
}

export interface PluginEntity {
  key: string;
  name: string;
  /** 业务唯一字段；空串表示没有 */
  unique: string;
  fields: PluginField[];
  list: {
    columns: PluginColumn[];
    /** create / update / delete / export / batch_delete */
    actions: string[];
    /** 可排序白名单（首项是默认排序）；空数组 = 只能按创建顺序 */
    sort: PluginSort[];
    /** 可筛选白名单 */
    filters: PluginFilter[];
  };
  /** 表单布局：恒至少一段，前端直接遍历 */
  form: { sections: PluginFormSection[] };
  /** 可直接对外读取的字段 key；空数组 = 这个对象不对外 */
  public: string[];
}

/** 插件目录项（列表用，不含 manifest 全文） */
export interface AdminAppPluginItem {
  key: string;
  name: string;
  version: string;
  api_version: number;
  /** 1 可用 / 2 下架 */
  status: number;
  source: string;
  icon: string;
  icon_url: string;
  installs: number;
  lock_version: number;
  updated_at: string;
}

/** 某应用已安装的一行（含已卸载：它的入口要用来"重新启用 / 清除数据"） */
export interface AdminAppPluginInstall {
  plugin_key: string;
  name: string;
  version: string;
  plugin_version: string;
  icon: string;
  /** 1 启用 / 2 停用 / 3 已卸载 */
  status: number;
  /** 对外只读开关：开启后终端可免凭证读取 manifest 白名单字段 */
  public_enabled: boolean;
  config: Record<string, unknown>;
  lock_version: number;
  record_count: number;
  updated_at: string;
}

/** 泛化路由页面的唯一数据源 */
export interface AdminAppPluginDetail {
  install: AdminAppPluginInstall;
  /** 所属应用（页面标题要写清"哪个应用的哪个插件"） */
  app: { id: number; slug: string; name: string };
  plugin: {
    key: string;
    name: string;
    version: string;
    api_version: number;
    icon: string;
    icon_url: string;
    config: PluginField[];
    entities: PluginEntity[];
    limits: { maxRecords: number };
  };
  /** 目录已升级、本应用仍按旧版写入过：页面顶部出横幅 */
  drift: { installed: string; current: string } | null;
  /** 当前 manifest 声明为必填、但这条安装里没有值的配置键（升级新增必填项后会出现） */
  config_missing: string[];
  record_count: number;
  catalog_disabled: boolean;
}

/** 插件记录：对外身份只有 record_key（内部 id 不下发） */
export interface AdminAppPluginRecord {
  record_key: string;
  data: Record<string, unknown>;
  version: number;
  created_at: string;
  updated_at: string;
}

/** 导入/预检结果：Applied=false 表示这是预检 */
export interface AdminAppPluginImportResult {
  key: string;
  name: string;
  version: string;
  lock_version: number;
  created: boolean;
  applied: boolean;
  problems: { path: string; message: string }[];
  breaking: { path: string; message: string }[];
  affected: { entity: string; field: string; records: number; reason: string }[];
}


/* ============================================================
   开放接口手册（管理台只读）
   ============================================================ */

/** 目录里的一条端点契约（与后端 apicontract.Endpoint + Detail 一一对应） */
export interface ApiDocEndpoint {
  key: string;
  group: string;
  title: string;
  method: string;
  path: string;
  /** open / reader / writer / session */
  auth: string;
  scope: string;
  /**
   * scope 是否真的在运行期被校验。
   *
   * 只有 writer 级别校验（ScopeGuard 只拦写操作）。给读接口标"需要 usage:read"
   * 会让接入方去申请一个永远用不到的权限点 —— 所以这一位必须来自后端，
   * 前端**不得**按 auth 自己推断。
   */
  scope_enforced: boolean;
  /** json / csv / binary / empty */
  media: string;
  idempotency: string;
  notes: string;
  params: ApiDocParam[];
  returns: ApiDocReturn[];
  errors: ApiDocError[];
  /** 成功时 data 恒为 null（如 logout、delete 的 204） */
  no_data: boolean;
  request_example: Record<string, unknown>;
  response_example: Record<string, unknown>;
  try: ApiDocTry;
}

/** 三态必填：conditional 的含义写在 required_note 里 */
export type ApiDocRequired = "required" | "optional" | "conditional";

/** 入参所在位置；path/query/body 三种之外还有 header 与 form */
export type ApiDocParamIn = "path" | "query" | "body" | "header" | "form";

/** 入参类型词表（与后端 apicontract.Type 一一对应，不允许出现词表外的值） */
export type ApiDocParamType =
  | "string"
  | "integer"
  | "number"
  | "boolean"
  | "array"
  | "object"
  | "file"
  | "binary"
  | "none"
  | "any";

/** 一条取值约束；kind 是 min/max/pattern/enum 之类的闭合枚举 */
export interface ApiDocConstraint {
  kind: string;
  value: number;
  text: string;
}

export interface ApiDocParam {
  name: string;
  in: ApiDocParamIn;
  type: ApiDocParamType;
  required: ApiDocRequired;
  /** conditional 时的具体条件（其余情况为空） */
  required_note: string;
  desc: string;
  note: string;
  default: string;
  enum: string[];
  example: unknown;
  constraints: ApiDocConstraint[];
}

export interface ApiDocReturn {
  name: string;
  type: string;
  desc: string;
  note: string;
  enum: string[];
}

export interface ApiDocError {
  code: string;
  when: string;
}

/** 试运行三元组：能不能发、为什么不能、要不要二次确认 */
export interface ApiDocTry {
  callable: boolean;
  /** callable=false 时是原因（会原样展示给接入方） */
  hint: string;
  /** safe：只读/无副作用；confirm：真改生产状态，必须二次确认 */
  risk: "safe" | "confirm";
}

export interface ApiDocNotExposed {
  method: string;
  path: string;
  title: string;
  reason: string;
}

export interface ApiDocCatalog {
  groups: string[];
  items: ApiDocEndpoint[];
  not_exposed: ApiDocNotExposed[];
  counts: { total: number; open: number; reader: number; writer: number; session: number };
  /** 托管密钥默认权限集 */
  hosted_scopes: string[];
  /** 可在本页直接试运行的端点数（由后端算，前端不重算） */
  callable: number;
  /** 各视图地址（目录/OpenAPI/Markdown/Swagger UI），由后端下发避免前端猜 */
  links: ApiDocLinks;
}

export interface ApiDocLinks {
  catalog: string;
  openapi: string;
  markdown: string;
  swagger_ui: string;
}

/* ---------------- 版本与一键更新 ---------------- */

/** 清单里的一个版本条目（后端已校验形状，这里只做展示） */
export interface ReleaseLatest {
  version: string;
  tag: string;
  published_at: string;
  notes: string;
  /** manifest 声明的提交号（可选）：给了就会与 tag 解析结果核对 */
  commit: string;
  /** 发布说明链接（仅 https） */
  url: string;
}

/** GET /api/admin/system/release 的 manifest 段 */
export interface ReleaseManifest {
  configured: boolean;
  /** 已打码的地址（去掉 query） */
  url: string;
  /**
   * 非空表示"没检查成功"，必须显示原因而不是"已是最新"。
   * 取值见后端 releasecheck.Kind*：not_configured / blocked / timeout / network /
   * http_status / too_large / bad_json / unsupported_schema / invalid_version / invalid_field
   */
  error_kind: string;
  checked_at?: string;
  /** 这份结论距今多少秒（缓存刻意不过期，但"多久以前"要可见） */
  age_sec?: number;
  http_status?: number;
  latest?: ReleaseLatest;
  update_available: boolean;
  /** 当前版本是否是可比较的版本号（本地 dev 构建为 false） */
  comparable: boolean;
}

/** 一次更新任务（进度 / 结果） */
export interface ReleaseJob {
  id: number;
  target_tag: string;
  status:
    | "queued"
    | "fetching"
    | "building"
    | "backing_up"
    | "switching"
    | "verifying"
    | "success"
    | "failed"
    | "rolling_back"
    | "rolled_back"
    | "manual_required";
  step: string;
  progress: number;
  message: string;
  failure_code?: string;
  rollback_result?: string;
  agent_host?: string;
  attempt: number;
  requested_at: string;
  started_at?: string;
  finished_at?: string;
}

/** 按钮被禁用的原因（前端只按它取文案，不重算规则） */
export type ReleaseBlockedReason =
  | ""
  | "already_running"
  | "not_configured"
  | "manifest_error"
  | "version_unparsable"
  | "no_update"
  | "agent_offline";

export interface ReleaseInfo {
  current: { version: string; node: string; mode: string };
  agent: { online: boolean; host?: string; current_tag?: string; seen_ago_sec?: number };
  manifest: ReleaseManifest;
  update: {
    can_update: boolean;
    blocked_reason: ReleaseBlockedReason;
    active_job: ReleaseJob | null;
    last_job: ReleaseJob | null;
  };
  /** 当前在途任务数（确认弹窗用来提示"会打断多少活"） */
  running_tasks: number;
}

/** GET /api/admin/system/release/update/current 的响应 */
export interface ReleaseCurrent {
  active: ReleaseJob | null;
  last: ReleaseJob | null;
  server_time: string;
}

/* --------------------------- 内置预设（恢复出厂） --------------------------- */

/** 一个对象（渠道/模型/绑定）在本次恢复里的三态计数 */
export interface PresetRestoreCounts {
  /** 命中内置标记、被显式拉回出厂值的行 */
  reused: number;
  /** 出厂声明有、库里没有（或已被删除）而新建的行 */
  created: number;
  /** 跳过：运营自建（未认领）、同名冲突、或写失败（看 warnings） */
  skipped: number;
}

export interface PresetRestoreGroup {
  /** 行标记（`builtin.<slug>`），也是恢复时的 `group` 入参 */
  group: string;
  /** 本组渠道的出厂名（给人看的展示标签，不参与匹配） */
  label: string;
  channels: PresetRestoreCounts;
  models: PresetRestoreCounts;
  bindings: PresetRestoreCounts;
  /** 单行失败/无法解除删除等留痕；有内容必须展示，否则"少恢复了几个"无从解释 */
  warnings?: string[];
}

/** POST /api/admin/presets/restore 的响应 */
export interface PresetRestoreResult {
  /** true = 预览（已回滚），库里没有任何变化 */
  dry_run: boolean;
  groups: PresetRestoreGroup[];
}
