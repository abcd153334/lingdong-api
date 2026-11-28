import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, ApiError, DEFAULT_PAGE_SIZE, downloadCSV, upload, type PageQuery } from "@/lib/apiClient";
import { apiUrl } from "@/lib/apiBase";
import { setBillingConstants } from "@/lib/format";
import { invalidatePortalMode } from "@/lib/portalMode";
import { clearAccountQueries } from "@/lib/queryClient";
import { isActiveTask } from "@/lib/task";
import { useAuthStore } from "@/stores/auth";
import type {
  AdapterInfo,
  AdapterSpecImpact,
  AdapterSpecItem,
  AdapterSpecPayload,
  AdminAppOption,
  AdminAuditItem,
  AdminConfigResult,
  AdminChannelsResult,
  AdminLogItem,
  AdminModelItem,
  AdminModelPayload,
  AdminModelsResult,
  AdminUserRow,
  AdminVendorPayload,
  AdminIconsResult,
  UploadedIconItem,
  AffInfo,
  AffRecord,
  AdminCapacityResult,
  AnnouncementsResult,
  ApiKeyItem,
  AuditMetaResult,
  AuthEventItem,
  AuthEventMetaResult,
  AdminCallDetail,
  AdminAppPluginDetail,
  AdminAppPluginImportResult,
  AdminAppPluginInstall,
  AdminAppPluginItem,
  AdminAppPluginRecord,
  BindingSaveResult,
  BindingsResult,
  BoardResult,
  ReleaseInfo,
  ReleaseCurrent,
  ReleaseJob,
  ChannelItem,
  ChannelKeysPayload,
  ChannelKeysResult,
  ChannelPayload,
  ChannelTestResult,
  PresetRestoreResult,
  CreateKeyResult,
  KeyPayload,
  KeySecretResult,
  LogAnalyticsResult,
  LoginResponse,
  LoginResult,
  MFABackupCodesResult,
  MFAStatus,
  MFASetupResult,
  MeInfo,
  AdminOrdersResult,
  AdminOrderItem,
  AdminPackageItem,
  AdminPackagesParams,
  AdminPackagesResult,
  PackagePayload,
  PackageWriteResult,
  OrderItem,
  OrderRefundOutcome,
  PortalConfig,
  PortalDefaults,
  PortalOverview,
  PricingSnapshot,
  BillingMode,
  RedeemDeleteResult,
  RedeemGenerateResult,
  RedeemItem,
  RegisterResult,
  ResetTicketResult,
  StorageTestResult,
  AdminTaskItem,
  TaskItem,
  TaskStatus,
  QRResult,
  SessionItem,
  TopupResult,
  TransactionItem,
  UpstreamSearchResult,
  UploadConfig,
  UploadItem,
  PortalCallDetail,
  UsageItem,
  VerifyResult,
  ContactChannel,
  PortalUsageResult,
  AdminUserKeyItem,
  UserRole,
  VendorItem,
  AdminVendorsResult,
  VerifyCodeResult,
  WalletPackagesResult,
  AdminAppItem,
  AdminAppKeyItem,
  AdminAppStats,
  AdminAppBrand,
  AppBrandPayload,
  AdminAppNotice,
  AppNoticePayload,
  AppNoticeUpdatePayload,
  AdminAppRelease,
  AdminAppReleasePage,
  AppReleasePayload,
  AppPayload,
  ApiDocCatalog,
} from "@/types";

/** 全部查询键集中此处，避免散落在页面 */
export const queryKeys = {
  me: ["portal", "me"] as const,
  /** 登录设备列表（撤销某台后要失效） */
  sessions: ["portal", "auth", "sessions"] as const,
  adminUsers: (params: AdminUsersParams) => ["admin", "users", params] as const,
  /* S17：用户详情抽屉三块只读数据 */
  adminUserTransactions: (id: number, params: AdminUserDetailParams) =>
    ["admin", "users", id, "transactions", params] as const,
  adminUserKeys: (id: number) => ["admin", "users", id, "keys"] as const,
  adminUsersAll: ["admin", "users"] as const,
  adminApps: (params: AdminAppsParams) => ["admin", "apps", "list", params] as const,
  // 选项与卡片列表分开缓存：形状不同（选项不含聚合），混用会让下拉触发一次全量聚合。
  adminAppOptions: (params: AdminAppOptionsParams) =>
    ["admin", "apps", "options", params] as const,
  /**
   * 列表前缀（不含任何详情）。
   *
   * 「list」这一段是必须的：`["admin","apps"]` 是列表与详情的公共前缀，
   * 拿它当失效目标等于"改一条公告顺手重算整页卡片的聚合、再重算一次 7 日趋势"。
   * 分段之后，列表与详情各失效各的，由调用方按真实依赖决定。
   */
  adminAppListAll: ["admin", "apps", "list"] as const,
  /** 全量前缀：只给"新建应用"用（此时不存在任何详情缓存）。 */
  adminAppsAll: ["admin", "apps"] as const,
  adminAppBrand: (id: number) => ["admin", "apps", id, "brand"] as const,
  adminAppStats: (id: number) => ["admin", "apps", id, "stats"] as const,
  adminAppNotices: (id: number, params: AdminAppNoticesParams) =>
    ["admin", "apps", id, "notices", params] as const,
  adminAppNoticesAll: (id: number) => ["admin", "apps", id, "notices"] as const,
  adminAppReleases: (id: number, params: AdminAppReleasesParams) =>
    ["admin", "apps", id, "releases", params] as const,
  adminAppReleasesAll: (id: number) => ["admin", "apps", id, "releases"] as const,
  adminAppKeys: (id: number, params: AdminAppKeysParams) =>
    ["admin", "apps", id, "keys", params] as const,
  adminAppKeysAll: (id: number) => ["admin", "apps", id, "keys"] as const,
  /* 应用插件（声明式）：目录 / 安装 / 记录三段键。 */
  adminAppPlugins: (params: PageQuery) => ["admin", "app-plugins", "list", params] as const,
  adminAppPluginsAll: ["admin", "app-plugins", "list"] as const,
  adminAppPlugin: (key: string) => ["admin", "app-plugins", key] as const,
  adminAppPluginSample: ["admin", "app-plugins", "sample"] as const,
  adminAppPluginInstalls: (id: number) => ["admin", "apps", id, "plugins"] as const,
  adminAppPluginDetail: (id: number, key: string) => ["admin", "apps", id, "plugins", key] as const,
  adminAppPluginRecords: (id: number, key: string, params: PageQuery) =>
    ["admin", "apps", id, "plugins", key, "records", params] as const,
  adminAppPluginRecordsAll: (id: number, key: string) =>
    ["admin", "apps", id, "plugins", key, "records"] as const,
  apiDocs: ["admin", "api-docs"] as const,
  /* S3 */
  pricing: ["pricing"] as const,
  keys: ["portal", "keys"] as const,
  adminModels: (params: AdminModelsParams) => ["admin", "models", params] as const,
  adminModelsAll: ["admin", "models"] as const,
  adminVendors: ["admin", "vendors"] as const,
  /** 图标图库（内置 + 最近上传）；只在选择器里用 */
  adminIcons: ["admin", "icons"] as const,
  /** 上游参考价：键里带关键词与供应商偏好，同词重开命中缓存、换词立即是新请求 */
  upstreamSearch: (p: { keyword: string; vendor: string }) =>
    ["admin", "upstream", "search", p] as const,
  /* S4 */
  adminChannels: (params: AdminChannelsParams) => ["admin", "channels", params] as const,
  adminChannelKeys: (id: number) => ["admin", "channels", id, "keys"] as const,
  adminChannelsAll: ["admin", "channels"] as const,
  adminAdapters: ["admin", "adapters"] as const,
  adminAdapterSpecs: ["admin", "adapter-specs"] as const,
  adminAdapterSpec: (id: number) => ["admin", "adapter-specs", id] as const,
  adminAdapterSpecImpact: (id: number) => ["admin", "adapter-specs", id, "impact"] as const,
  /* S12 */
  /* S5 */
  portalUsage: (params: PortalUsageParams) => ["portal", "usage", params] as const,
  portalUsageAll: ["portal", "usage"] as const,
  /** 调用记录详情：前缀与列表同源，列表刷新（新记录）不会误伤已打开的详情 */
  portalUsageDetail: (id: number) => ["portal", "usage", "detail", id] as const,
  /* S7 */
  portalTasks: (params: PortalTasksParams) => ["portal", "tasks", params] as const,
  portalTasksAll: ["portal", "tasks"] as const,
  adminTasks: (params: AdminTasksParams) => ["admin", "tasks", params] as const,
  adminTasksAll: ["admin", "tasks"] as const,
  /** 资金对账报表（补收欠款后必须失效重取，否则数字停在旧值） */
  adminReconcile: ["admin", "reconcile"] as const,
  /** 任务详情：列表不下发产物清单，抽屉按需取（前缀同 tasksAll，取消/重试后自动刷新） */
  portalTaskDetail: (id: string) => ["portal", "tasks", "detail", id] as const,
  adminTaskDetail: (id: string) => ["admin", "tasks", "detail", id] as const,
  /* S8 */
  portalFiles: (params: PortalFilesParams) => ["portal", "files", params] as const,
  /** 前缀失效键：同时覆盖列表（各筛选组合）与 config */
  portalFilesAll: ["portal", "files"] as const,
  portalFilesConfig: ["portal", "files", "config"] as const,
  /* S9 */
  portalPackages: ["portal", "packages"] as const,
  portalOrders: (params: PortalOrdersParams) => ["portal", "orders", params] as const,
  portalOrdersAll: ["portal", "orders"] as const,
  portalOrderQR: (id: number) => ["portal", "orders", id, "qrcode"] as const,
  adminOrders: (params: AdminOrdersParams) => ["admin", "orders", params] as const,
  adminOrdersAll: ["admin", "orders"] as const,
  portalTransactions: (params: PortalTransactionsParams) => ["portal", "transactions", params] as const,
  portalTransactionsAll: ["portal", "transactions"] as const,
  portalAff: ["portal", "aff"] as const,
  portalAffRecords: (params: PortalAffRecordsParams) => ["portal", "aff", "records", params] as const,
  /* S10 */
  portalConfig: ["portal", "config"] as const,
  portalAnnouncements: ["portal", "announcements"] as const,
  adminConfig: ["admin", "config"] as const,
  /* S15 */
  portalOverview: (days: number) => ["portal", "overview", days] as const,
  portalDefaults: ["portal", "defaults"] as const,
  /* S11 */
  adminBoard: ["admin", "data", "board"] as const,
  /* B7：容量体检（运维看板） */
  adminCapacity: ["admin", "data", "capacity"] as const,
  adminLogs: (params: AdminLogsParams) => ["admin", "logs", params] as const,
  adminLogAnalytics: (params: AdminLogsParams) => ["admin", "logs", "analytics", params] as const,
  adminLogDetail: (id: number, scope: string) => ["admin", "logs", "detail", scope, id] as const,
  adminAudits: (params: AdminAuditsParams) => ["admin", "audits", params] as const,
  adminAuditMeta: ["admin", "audits", "meta"] as const,
  /* 版本与一键更新 */
  adminRelease: ["admin", "system", "release"] as const,
  adminReleaseCurrent: ["admin", "system", "release", "current"] as const,
  /* 认证事件审计 */
  adminAuthEvents: (params: AdminAuthEventsParams) => ["admin", "auth-events", params] as const,
  adminAuthEventMeta: ["admin", "auth-events", "meta"] as const,
  /* 二次验证 */
  mfaStatus: ["portal", "mfa", "status"] as const,
  /* S16 */
  adminRedeems: (params: AdminRedeemsParams) => ["admin", "redeems", params] as const,
  adminRedeemsAll: ["admin", "redeems"] as const,
  /* 充值套餐 */
  adminPackages: (params: AdminPackagesParams) => ["admin", "packages", params] as const,
  adminPackagesAll: ["admin", "packages"] as const,
};

const networkError = (error: unknown): ApiError =>
  error instanceof ApiError ? error : new ApiError(-1, "网络异常，请检查连接后重试");

type Outcome<T> = { ok: true; data: T } | { ok: false; error: ApiError };

/* ----------------------------- 门户 / 鉴权 ----------------------------- */
/*
 * 认证端点的分组约定（与后端 router 一致）：
 * - `/portal/auth/*`（公开）：注册、登录、找回密码、验证码 —— 凭证来自 Cookie 之外的输入；
 * - `/portal/auth/*`（会话）：登出、改密、换绑、设备列表 —— 必须带会话 + CSRF 头。
 * 两者路径同前缀，靠「要不要登录」区分，前端只按语义调用。
 */

export function useMeQuery() {
  return useQuery({
    queryKey: queryKeys.me,
    queryFn: () => apiClient.get<MeInfo>("/portal/me"),
  });
}

/**
 * 头像变更后同步会话 store。
 *
 * 只 `invalidateQueries(me)` 不够：顶栏 / 用户菜单读的是 `useAuthStore.user` 的快照，
 * 而 `loadSession` 命中 `loaded` 就直接返回缓存 —— 结果是「头像已换，右上角还是旧的」，
 * 直到整页刷新。
 */
function syncAvatar(avatarUrl: string): void {
  const { user, setUser } = useAuthStore.getState();
  if (user) setUser({ ...user, avatar_url: avatarUrl });
}

/** 上传头像（multipart 字段名 file）；后端裁 128×128 转 webp 后以 data URL 回传。 */
export function useAvatarUploadMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
      return upload<{ avatar_url: string }>("/portal/me/avatar", form);
    },
    onSuccess: (res) => {
      syncAvatar(res.avatar_url);
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/** 清空头像（回到默认图）。后端恒回 200 + 空串，不返回 204。 */
export function useAvatarDeleteMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => apiClient.delete<{ avatar_url: string }>("/portal/me/avatar"),
    onSuccess: (res) => {
      syncAvatar(res.avatar_url);
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

export interface LoginBody {
  /** 账号：用户名 / 邮箱 / 手机号三选一（服务端按形态判定） */
  account: string;
  password: string;
  /** 图形验证码；只有策略要求时才需要（after_fails 达阈值 / always）。 */
  captcha_id?: string;
  captcha_answer?: string;
}

/** 登录失败交由 err-line 呈现（原型不弹 toast），故吞掉错误返回判别式结果 */
export function useLoginMutation() {
  const setUser = useAuthStore((s) => s.setUser);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: LoginBody): Promise<Outcome<LoginResponse>> => {
      try {
        return { ok: true, data: await apiClient.post<LoginResponse>("/portal/auth/login", body) };
      } catch (error) {
        return { ok: false, error: networkError(error) };
      }
    },
    onSuccess: (outcome) => {
      if (!outcome.ok) return;
      // 需要二次验证：**没有 Cookie、没有用户**，用户态必须保持未登录，
      // 否则前端会先跳进 /home，再被守卫弹回来（闪一下才算失败）。
      if ("mfa_required" in outcome.data) return;
      // 换号登录：先清掉上一个账号的账户级缓存，再写用户态。
      // 反过来的话，中间那一瞬（以及随后 30s staleTime 内）页面读到的仍是旧账号数据。
      clearAccountQueries();
      // 服务端已把会话票写进 httpOnly Cookie；这里只接管用户态。
      setUser(outcome.data.user);
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/** 登录第二步：用工牌换会话（动态码或备份码） */
export function useLoginMFAMutation() {
  const setUser = useAuthStore((s) => s.setUser);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: { challenge: string; code: string }): Promise<Outcome<LoginResult>> => {
      try {
        return { ok: true, data: await apiClient.post<LoginResult>("/portal/auth/login/mfa", body) };
      } catch (error) {
        return { ok: false, error: networkError(error) };
      }
    },
    onSuccess: (outcome) => {
      if (!outcome.ok) return;
      // 同 useLoginMutation：MFA 是同一轮登录的第二步，换号时也要先清旧缓存。
      clearAccountQueries();
      setUser(outcome.data.user);
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/* ------------------------------ 二次验证（TOTP） ------------------------------ */

/** 当前用户的二次验证状态（关闭时 backup_codes_left 恒 0） */
export function useMFAStatusQuery(enabled = true) {
  return useQuery({
    queryKey: queryKeys.mfaStatus,
    queryFn: () => apiClient.get<MFAStatus>("/portal/auth/mfa/status"),
    enabled,
  });
}

/** 生成（或覆盖未完成的）TOTP 密钥，返回 secret 与 otpauth 地址 */
export function useMFASetupMutation() {
  return useMutation({
    mutationFn: (password: string) =>
      apiClient.post<MFASetupResult>("/portal/auth/mfa/setup", { password }),
  });
}

/** 用一次正确动态码确认绑定；返回**仅此一次**可见的备份码 */
export function useMFAEnableMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: { code: string; password: string }) =>
      apiClient.post<MFABackupCodesResult>("/portal/auth/mfa/enable", input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.mfaStatus });
    },
  });
}

/** 关闭二次验证：服务端会撤销全部会话（含当前），调用方必须跳登录页 */
export function useMFADisableMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (code: string) => apiClient.post<null>("/portal/auth/mfa/disable", { code }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.mfaStatus });
    },
  });
}

/** 重生成备份码（旧码立即失效） */
export function useMFARegenerateMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (code: string) =>
      apiClient.post<MFABackupCodesResult>("/portal/auth/mfa/backup-codes/regenerate", { code }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.mfaStatus });
    },
  });
}

export interface RegisterBody {
  username: string;
  /** 联系方式渠道（undefined = 未填联系方式） */
  channel?: ContactChannel;
  target?: string;
  /** 联系方式验证码 */
  code?: string;
  password: string;
  confirm_password: string;
  aff_code?: string;
  agreed: boolean;
  /** 图形验证码；注册恒要求（见 useCaptcha） */
  captcha_id?: string;
  captcha_answer?: string;
}

/** 注册失败同样走 err-line */
export function useRegisterMutation() {
  return useMutation({
    mutationFn: async (body: RegisterBody): Promise<Outcome<RegisterResult>> => {
      try {
        return { ok: true, data: await apiClient.post<RegisterResult>("/portal/auth/register", body) };
      } catch (error) {
        return { ok: false, error: networkError(error) };
      }
    },
  });
}

export interface ContactCodeBody {
  channel: ContactChannel;
  target: string;
  /** 图形验证码；找回密码的发码端点恒校验，注册发码不需要 */
  captcha_id?: string;
  captcha_answer?: string;
}

/** 注册发码（公开）：必须先过人机验证，否则这是一条免费短信/邮件通道 */
export function useRegisterContactCodeMutation() {
  return useMutation({
    mutationFn: (body: ContactCodeBody) =>
      apiClient.post<VerifyCodeResult>("/portal/auth/register/contact-code", body),
  });
}

/** 找回密码发码（公开）：账号不存在也返回 200（枚举防护），前端不做存在性判断 */
export function useResetContactCodeMutation() {
  return useMutation({
    mutationFn: (body: ContactCodeBody) =>
      apiClient.post<VerifyCodeResult>("/portal/auth/password/reset/contact-code", body),
  });
}

export interface ChangePasswordBody {
  old_password: string;
  new_password: string;
}

/**
 * 改密：成功后**该用户全部会话失效（含当前）**，服务端已清 Cookie。
 * 因此调用方必须跳登录页，而不是继续留在原页面（下一个请求必然 401）。
 */
export function useChangePasswordMutation() {
  return useMutation({
    mutationFn: (body: ChangePasswordBody) => apiClient.post<null>("/portal/auth/password/change", body),
  });
}

export interface ResetVerifyBody {
  channel: ContactChannel;
  target: string;
  code: string;
}

/** 找回第二步：校验验证码换一次性凭据（只校验不消费验证码） */
export function useResetVerifyMutation() {
  return useMutation({
    mutationFn: (body: ResetVerifyBody) =>
      apiClient.post<ResetTicketResult>("/portal/auth/password/reset/verify", body),
  });
}

export interface ResetPasswordBody {
  reset_ticket: string;
  new_password: string;
}

/** 找回第三步：用一次性凭据重置密码（凭据只能兑换一次） */
export function useResetPasswordMutation() {
  return useMutation({
    mutationFn: (body: ResetPasswordBody) => apiClient.post<null>("/portal/auth/password/reset", body),
  });
}

export interface BindBody {
  channel: ContactChannel;
  target: string;
  code: string;
}

/** 已登录换绑发码（走会话 + CSRF） */
export function useContactCodeMutation() {
  return useMutation({
    mutationFn: (body: { channel: ContactChannel; target: string }) =>
      apiClient.post<VerifyCodeResult>("/portal/auth/contact/code", body),
  });
}

export function useBindMutation() {
  const queryClient = useQueryClient();
  const setUser = useAuthStore((s) => s.setUser);
  return useMutation({
    mutationFn: (body: BindBody) => apiClient.post<BindingsResult>("/portal/auth/contact/bind", body),
    onSuccess: (data) => {
      const current = useAuthStore.getState().user;
      if (current) setUser({ ...current, email: data.email, phone: data.phone });
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/**
 * 设备列表（会话管理）。
 *
 * 只读取自己的会话：作用域由服务端从会话里取 uid 决定，前端不传用户 id
 * （传了也不能信，否则就是一个越权读别人设备的入口）。
 */
export function useSessionsQuery(enabled = true) {
  return useQuery({
    queryKey: queryKeys.sessions,
    queryFn: () => apiClient.get<{ sessions: SessionItem[] }>("/portal/auth/sessions"),
    enabled,
  });
}

export function useRevokeSessionMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.post<null>(`/portal/auth/sessions/${id}/revoke`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
    },
  });
}

export function useRevokeAllSessionsMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    // 撤销全部（含当前）：服务端清 Cookie，本地必须同步登出。
    mutationFn: () => apiClient.post<null>("/portal/auth/sessions/revoke-all"),
    onSuccess: () => {
      useAuthStore.getState().clear();
      queryClient.clear();
    },
  });
}

/** 登出：服务端撤销当前会话并清 Cookie；失败也照常清理本地态（避免无后端时误报 toast） */
export function useLogoutMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      try {
        await apiClient.post<null>("/portal/auth/logout");
      } catch {
        /* 会话可能已失效：本地照常收尾 */
      }
    },
    onSettled: () => {
      useAuthStore.getState().clear();
      queryClient.clear();
    },
  });
}

/* ------------------------------- 管理台 ------------------------------- */

export interface AdminUsersParams {
  page?: number;
  page_size?: number;
  sort?: string;
  keyword?: string;
  group?: string;
  status?: number;
  /** 按所属应用筛选：0 = 平台直连（订单/日志/任务）；用户页不接受 0 */
  app_id?: number;
  // 用户页的 app_id 语义不同：筛的是「在该应用下持有托管密钥」的用户，
  // 且 app_id=0 会被后端拒绝（用户池全局共享，没有「平台直连的用户」）。
}

export function useAdminUsersQuery(params: AdminUsersParams) {
  return useQuery({
    queryKey: queryKeys.adminUsers(params),
    queryFn: ({ signal }) =>
      apiClient.getPage<AdminUserRow>("/admin/users", params as PageQuery, { signal }),
    placeholderData: (previous) => previous,
  });
}

export interface AdjustQuotaBody {
  /** 调整金额（微，可正可负）—— 与后端 `adminAdjustReq.Amount` 同一个量纲 */
  amount: number;
  reason: string;
}

export function useAdminAdjustQuotaMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: AdjustQuotaBody }) =>
      apiClient.post<null>(`/admin/users/${id}/quota`, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminUsersAll });
    },
  });
}

export interface AdminUpdateUserBody {
  group?: string;
  status?: number;
  role?: UserRole;
  /** 用户级余额上限（微；0 = 清空覆盖、跟随全局配置） */
  balance_limit?: number;
}

export function useAdminUpdateUserMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: AdminUpdateUserBody }) =>
      apiClient.patch<null>(`/admin/users/${id}`, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminUsersAll });
    },
  });
}

/* --------------------- S3 定价目录 / API KEY / 模型目录 --------------------- */

/**
 * 拉取定价目录并注入计费口径（币种/符号/qpc）。
 *
 * 单独导出而不是只在 hook 里内联：App 启动时需要**无条件**拉一次
 * （见 main.tsx），否则没进过模型广场/钱包页的用户会用 money.ts 里的
 * 默认口径渲染金额 —— 单币种时代那只是「$ 与 ¥ 的小差别」，
 * 多币种下就是差 7.2 倍的错数。
 */
export async function fetchPricingSnapshot(): Promise<PricingSnapshot> {
  const data = await apiClient.get<PricingSnapshot>("/pricing");
  // 一次注入完整口径：币种/符号/qpc/次币种 qpc 必须来自**同一个响应**，
  // 分两次注入会出现「符号已换、基准还是旧的」的瞬间。
  setBillingConstants({
    scale: data.scale,
    cnyPerUsd: data.cny_per_usd,
    currency: data.currency,
    otherCurrency: data.other_currency,
  });
  return data;
}

/** 定价目录快照（公开）；顺带把计费口径注入前端（单一真相源） */
export function usePricingQuery(options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: queryKeys.pricing,
    queryFn: fetchPricingSnapshot,
    staleTime: 60_000,
    enabled: options?.enabled ?? true,
  });
}

/* ---------- 门户 API KEY ---------- */

export function useKeysQuery(options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: queryKeys.keys,
    queryFn: () => apiClient.get<{ items: ApiKeyItem[] }>("/portal/keys"),
    // 公开页（如 /docs 运行台）未登录时禁用，避免 401 触发全局会话失效处理
    enabled: options?.enabled ?? true,
  });
}

export function useCreateKeyMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: KeyPayload) => apiClient.post<CreateKeyResult>("/portal/keys", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.keys });
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

export function useUpdateKeyMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: KeyPayload }) =>
      apiClient.put<null>(`/portal/keys/${id}`, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.keys });
    },
  });
}

export function useUpdateKeyStatusMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, status }: { id: number; status: 1 | 2 }) =>
      apiClient.patch<null>(`/portal/keys/${id}/status`, { status }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.keys });
    },
  });
}

export function useResetKeyMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.post<KeySecretResult>(`/portal/keys/${id}/reset`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.keys });
    },
  });
}

export function useDeleteKeyMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.delete<null>(`/portal/keys/${id}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.keys });
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/** 揭示明文（GET + 服务端写审计，故按 mutation 处理） */
export function useRevealKeyMutation() {
  return useMutation({
    mutationFn: (id: number) => apiClient.get<KeySecretResult>(`/portal/keys/${id}/plaintext`),
  });
}

/* ---------- 托管密钥（门户侧轮换入口） ---------- */

/** POST /api/portal/apps/{slug}/hosted-key 的响应（签发与轮换同形）。 */
export interface HostedKeyResult {
  app: { id: number; slug: string; name: string };
  key: {
    id: number;
    prefix: string;
    scopes: string[];
    created: boolean;
    rotated: boolean;
    issued_at: string;
    status: number;
    expired_at: string | null;
    amount_limit: number;
    used: number;
    rpm_limit: number;
    /**
     * 上一把明文的最后可用时间（轮换重叠窗口的终点）。
     * null = 没有上一代可用（首次签发，或平台把窗口配成 0 = 立即失效）。
     * 界面上必须提示这个时间：用户据此排期替换，而不是等应用突然 401。
     */
    previous_expires_at: string | null;
  };
  /** 空串表示没有新明文（命中已有密钥且未轮换），不得覆盖本地凭证。 */
  plaintext: string;
}

/**
 * 轮换托管密钥：换新明文 + 续期有效期；技能上只在可自愈的禁用下解禁。
 *
 * 不需要先拿到应用清单：`app_slug` 已随 `/portal/keys` 的每一行直出。
 * 曾经的实现要先查 `/portal/apps` 把 app_id 映射成 slug 才能轮换，于是
 * 清单请求失败、或托管钥落在列表第二页时，轮换入口会**静默消失**。
 */
export function useRotateHostedKeyMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (slug: string) =>
      apiClient.post<HostedKeyResult>(`/portal/apps/${slug}/hosted-key`, { rotate: true }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.keys });
    },
  });
}

/**
 * 首次接入：为某个应用换取托管密钥（`rotate=false`）。
 *
 * 与轮换是**同一个端点**：是否「首次」由服务端按库里有没有这把 `(app,user)` 判断，
 * 不由请求体声明（否则一个 `{rotate:true}` 就能绕过自助签发开关）。
 *
 * 已持有时服务端返回 `plaintext=""` + `created=false` —— 调用方**不能**把它当成
 * 成功并展示空密钥，那会让人以为"拿到了密钥"然后复制到配置里。
 */
export function useIssueHostedKeyMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (slug: string) =>
      apiClient.post<HostedKeyResult>(`/portal/apps/${slug}/hosted-key`, { rotate: false }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.keys });
    },
  });
}

/* ---------- 管理台模型 / 供应商 / 上游 ---------- */

/** 管理台列表通用查询参数（B3：分页 + 排序同时下推） */
export interface AdminListParams {
  page?: number;
  page_size?: number;
  /** 排序字段（服务端白名单，非法值返回 400） */
  sort?: string;
  order?: "asc" | "desc";
}

export interface AdminModelsParams extends AdminListParams {
  keyword?: string;
  vendor?: string;
  /** 只看这个渠道上的模型（存在一条绑定即算，**不看绑定启停**） */
  channel_id?: number;
  /** 模型自身的启停 */
  enabled?: boolean;
}

export function useAdminModelsQuery(params: AdminModelsParams, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminModels(params),
    // enabled 只有"选模型"这类按需场景才传 false：列表页首屏必须发一次。
    enabled,
    queryFn: ({ signal }) =>
      apiClient.get<AdminModelsResult>("/admin/models", {
        signal,
        query: {
          keyword: params.keyword,
          vendor: params.vendor,
          channel_id: params.channel_id,
          enabled: params.enabled,
          page: params.page,
          page_size: params.page_size,
          sort: params.sort,
          order: params.order,
        },
      }),
    placeholderData: (previous) => previous,
  });
}

function useInvalidateCatalog() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminModelsAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.pricing });
  };
}

/**
 * 会改写「渠道行的绑定摘要」的模型写操作：模型列表 + 渠道列表 + 定价一起失效。
 *
 * 渠道行的 `bound_models` 是左栏浮层「N 个模型」与 `BindDialog`「已绑定」标记的
 * **唯一**来源：
 *  - 在渠道内新建模型带 `initial_binding` → 只失效模型列表就会"右栏已经列出新行，
 *    左栏还写着旧计数，绑定选择器里还把刚建的模型标成可绑定"；
 *  - 编辑模型会改图标与展示名（`bound_models[].icon_url` 派生自模型）→ 左栏图示变旧。
 *
 * 启停与删除**不进**这个集合：前者不改任何渠道字段；后者被后端拦在"仍有绑定"之外
 *（删除成功 = 本来就没有绑定）。让它们每次都拉一遍全量渠道纯属浪费。
 */
function useInvalidateCatalogAndChannels() {
  const queryClient = useQueryClient();
  const invalidateCatalog = useInvalidateCatalog();
  return () => {
    invalidateCatalog();
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminChannelsAll });
  };
}

export function useCreateAdminModelMutation() {
  const invalidate = useInvalidateCatalogAndChannels();
  return useMutation({
    mutationFn: (body: AdminModelPayload) =>
      apiClient.post<{ item: AdminModelItem; warnings?: string[] }>("/admin/models", body),
    onSuccess: invalidate,
  });
}

export function useUpdateAdminModelMutation() {
  const invalidate = useInvalidateCatalogAndChannels();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: AdminModelPayload }) =>
      apiClient.put<{ item: AdminModelItem; warnings?: string[] }>(`/admin/models/${id}`, body),
    onSuccess: invalidate,
  });
}

export function useToggleAdminModelMutation() {
  const invalidate = useInvalidateCatalog();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      apiClient.patch<{ enabled: boolean }>(`/admin/models/${id}`, { enabled }),
    onSuccess: invalidate,
  });
}

export function useDeleteAdminModelMutation() {
  const invalidate = useInvalidateCatalog();
  return useMutation({
    mutationFn: (id: string) => apiClient.delete<null>(`/admin/models/${id}`),
    onSuccess: invalidate,
  });
}

/**
 * 绑定变更后的失效集合：模型目录（绑定数/可用性）+ 渠道列表（左栏"N 个模型"
 * 与删除拦截都读 `bound_models`）+ 门户定价（渠道数）。
 *
 * 少失效渠道列表的表现是：解绑后左栏仍写着「3 个模型」，去删渠道又被后端 409。
 *
 * 与模型写操作共用同一条集合（见 `useInvalidateCatalogAndChannels`）：绑定数同时长在
 * 两边的行上，任何一边单独失效都会出现两张表各说一套。
 */
function useInvalidateBindings() {
  return useInvalidateCatalogAndChannels();
}

/**
 * 新增/更新**一条**绑定（模型 ↔ 渠道）。
 *
 * 刻意没有"整组替换"的封装：整组要求调用方先读全部渠道、改一处、再整组回传，
 * 两名管理员同时改同一模型的不同渠道会互相覆盖。后端同样只保留单条写入口。
 */
export function useUpsertModelBindingMutation() {
  const invalidate = useInvalidateBindings();
  return useMutation({
    mutationFn: ({
      modelId,
      channelId,
      body,
    }: {
      modelId: string;
      channelId: number;
      body: { upstream_model: string; weight: number; priority: number; enabled: boolean };
    }) =>
      apiClient.put<BindingSaveResult>(`/admin/models/${modelId}/bindings/${channelId}`, body),
    onSuccess: invalidate,
  });
}

export function useDeleteModelBindingMutation() {
  const invalidate = useInvalidateBindings();
  return useMutation({
    mutationFn: ({ modelId, channelId }: { modelId: string; channelId: number }) =>
      apiClient.delete<{ bindings: number }>(`/admin/models/${modelId}/bindings/${channelId}`),
    onSuccess: invalidate,
  });
}

export function useAdminVendorsQuery() {
  return useQuery({
    queryKey: queryKeys.adminVendors,
    queryFn: () => apiClient.get<AdminVendorsResult>("/admin/vendors"),
  });
}

export function useCreateAdminVendorMutation() {
  const queryClient = useQueryClient();
  const invalidate = useInvalidateCatalog();
  return useMutation({
    mutationFn: (body: AdminVendorPayload) =>
      apiClient.post<{ item: VendorItem }>("/admin/vendors", body),
    onSuccess: () => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminVendors });
    },
  });
}

export function useUpdateAdminVendorMutation() {
  const queryClient = useQueryClient();
  const invalidate = useInvalidateCatalog();
  return useMutation({
    // 回包是**更新后**的行（后端重新读了一次）：保存后用它刷新草稿，
    // 不必等整表 refetch，也就不会出现「保存成功但界面还是旧图标」。
    mutationFn: ({ id, body }: { id: string; body: AdminVendorPayload }) =>
      apiClient.put<{ item: VendorItem }>(`/admin/vendors/${id}`, body),
    onSuccess: () => {
      // 三处都要失效：供应商列表（自己）、模型表格的供应商列（名字/色/图标变了）、
      // 门户价目目录（pricing 快照）。少一处就是「改完这里对、那里还是旧的」。
      invalidate();
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminVendors });
    },
  });
}

export function useDeleteAdminVendorMutation() {
  const queryClient = useQueryClient();
  const invalidate = useInvalidateCatalog();
  return useMutation({
    mutationFn: (id: string) => apiClient.delete<null>(`/admin/vendors/${id}`),
    onSuccess: () => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminVendors });
    },
  });
}

/**
 * 图标图库（内置 + 最近上传）。
 *
 * `staleTime: Infinity`：内置图库编译在二进制里，跟随发版变化；上传列表只在
 * 本会话里由 `useUploadIconMutation` 主动失效。选择器每次打开都重拉一遍
 * 72 条元数据没有意义，而多拉一次就多一次首屏延迟。
 */
export function useAdminIconsQuery() {
  return useQuery({
    queryKey: queryKeys.adminIcons,
    queryFn: () => apiClient.get<AdminIconsResult>("/admin/icons"),
    staleTime: Infinity,
  });
}

/** 上传图标：multipart（不能走 JSON 通道，见 apiClient.upload）。 */
export function useUploadIconMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
      return upload<{
        id: string;
        icon: string;
        /** 可渲染的绝对地址（服务端拼好；前端不参与路径规则） */
        url: string;
        mime: string;
        size: number;
        created_at: number;
      }>(
        "/admin/icons",
        form,
      );
    },
    onSuccess: (res) => {
      // 先把自己**前插**进缓存，再失效。图库接口有 200 条上限且按上传时间倒序，
      // 只 invalidate 再重拉的话，刚传的这张可能排在 200 名之外 ——
      // 表现是「上传成功但图库里找不到」，用户会以为没传上去。
      queryClient.setQueryData<AdminIconsResult>(queryKeys.adminIcons, (prev) => {
        if (!prev) return prev; // 还没拉过：交给下面的 invalidate 去取全量
        const rest = prev.uploaded.filter((u) => u.id !== res.id);
        return { ...prev, uploaded: [uploadedItemOf(res), ...rest] };
      });
      // 只失效图库：上传只是"多了一张可选图"，不会改动任何实体。
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminIcons });
    },
  });
}

/** 上传回包 → 图库列表项（两处形状必须一致，只在这里转一次）。 */
function uploadedItemOf(res: {
  id: string;
  icon: string;
  url: string;
  size: number;
  created_at: number;
}): UploadedIconItem {
  return { id: res.id, icon: res.icon, url: res.url, size: res.size, created_at: res.created_at };
}

export interface UpstreamPriceParams {
  keyword: string;
  /** 当前模型所属供应商：只影响同名去重时的排序偏好，不参与过滤 */
  vendor?: string;
}

/**
 * 上游参考价查询（models.dev 公开数据，服务端 10 分钟快照）。
 *
 * `meta.silent`：失败在面板内联展示，不弹全局 toast —— 它是个辅助查询，
 * 拉不到不该在整页顶上飘一条红条。
 * `keyword.length >= 2`：单字符既没有检索价值，又会白白触发一次 7MB 快照拉取。
 * 不传 limit：条数上限只在后端定义一处。
 */
export function useUpstreamPriceQuery(p: UpstreamPriceParams, enabled = true) {
  const keyword = p.keyword.trim();
  const vendor = p.vendor ?? "";
  return useQuery({
    queryKey: queryKeys.upstreamSearch({ keyword, vendor }),
    queryFn: ({ signal }) =>
      apiClient.get<UpstreamSearchResult>("/admin/upstream/search", {
        signal,
        query: { keyword, vendor },
      }),
    enabled: enabled && keyword.length >= 2,
    staleTime: 5 * 60_000,
    meta: { silent: true },
  });
}

/* ------------------- S4 渠道与适配器 ------------------- */

export interface AdminChannelsParams extends AdminListParams {
  keyword?: string;
  status?: number;
}

export function useAdminChannelsQuery(params: AdminChannelsParams) {
  return useQuery({
    queryKey: queryKeys.adminChannels(params),
    queryFn: ({ signal }) =>
      apiClient.get<AdminChannelsResult>("/admin/channels", {
        signal,
        query: {
          keyword: params.keyword,
          status: params.status || undefined,
          page: params.page,
          page_size: params.page_size,
          sort: params.sort,
          order: params.order,
        },
      }),
    placeholderData: (previous) => previous,
  });
}

/** 规格列表（与 /admin/adapters 同源，但带 DSL 原文之外的完整字段）。 */
export function useAdminAdapterSpecsQuery() {
  return useQuery({
    queryKey: queryKeys.adminAdapterSpecs,
    queryFn: () => apiClient.get<{ items: AdapterSpecItem[] }>("/admin/adapter-specs"),
  });
}

/** 单个规格详情（含 DSL 原文，编辑抽屉用）。 */
export function useAdminAdapterSpecQuery(id: number | null) {
  return useQuery({
    queryKey: queryKeys.adminAdapterSpec(id ?? 0),
    queryFn: () => apiClient.get<{ item: AdapterSpecItem }>(`/admin/adapter-specs/${id}`),
    enabled: !!id,
  });
}

/** 影响面：删除/改端点前必须先看它在被谁用。 */
export function useAdapterSpecImpactQuery(id: number | null) {
  return useQuery({
    queryKey: queryKeys.adminAdapterSpecImpact(id ?? 0),
    queryFn: () => apiClient.get<{ impact: AdapterSpecImpact }>(`/admin/adapter-specs/${id}/impact`),
    enabled: !!id,
  });
}

/** 规格写操作的失效集合：适配器清单、渠道下拉、文档字段表都跟着变。 */
function useInvalidateSpecs() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminAdapters });
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminAdapterSpecs });
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminChannelsAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.pricing });
  };
}

/**
 * 保存规格（PUT）。必须带 version：不带就是"后写覆盖先写"，
 * 两个管理员同时编辑时，先保存的那份会静默消失。
 */
export function useUpdateAdapterSpecMutation() {
  const invalidate = useInvalidateSpecs();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: AdapterSpecPayload }) =>
      apiClient.put<{ item: AdapterSpecItem }>(`/admin/adapter-specs/${id}`, body),
    onSuccess: invalidate,
  });
}

export function useSetAdapterSpecStatusMutation() {
  const invalidate = useInvalidateSpecs();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, status }: { id: number; status: number }) =>
      apiClient.patch<null>(`/admin/adapter-specs/${id}/status`, { status }),
    onSuccess: (_d, v) => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminAdapterSpec(v.id) });
    },
  });
}

export function useDeleteAdapterSpecMutation() {
  const invalidate = useInvalidateSpecs();
  return useMutation({
    mutationFn: (id: number) => apiClient.delete<null>(`/admin/adapter-specs/${id}`),
    onSuccess: invalidate,
  });
}

export function useAdminAdaptersQuery() {
  return useQuery({
    queryKey: queryKeys.adminAdapters,
    queryFn: () => apiClient.get<{ items: AdapterInfo[] }>("/admin/adapters"),
  });
}

/**
 * 渠道变更后的失效集合（P2-14）。
 *
 * 渠道是「模型 ↔ 上游」的枢纽，改一个渠道会同时影响：
 *   - 渠道列表本身；
 *   - 模型列表的**绑定数**与绑定抽屉里的渠道状态；
 *   - 定价快照（模型上下架由绑定是否存在决定，改渠道可能让模型进出目录）；
 *   - 适配器页的渠道计数。
 * 原实现只失效渠道列表，于是「新建渠道 → 模型广场仍显示 0 个渠道可用」。
 */
function useInvalidateChannels() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminChannelsAll });
    // adminModelsAll 是前缀键，已覆盖 ["admin","models",<id>,"bindings"]，无需另列
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminModelsAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.pricing });
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminAdapters });
  };
}

export function useCreateAdminChannelMutation() {
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: (body: ChannelPayload) => apiClient.post<{ item: ChannelItem }>("/admin/channels", body),
    onSuccess: invalidate,
  });
}

export function useUpdateAdminChannelMutation() {
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: ChannelPayload }) =>
      apiClient.put<{ item: ChannelItem }>(`/admin/channels/${id}`, body),
    onSuccess: invalidate,
  });
}

export function useDeleteAdminChannelMutation() {
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: (id: number) => apiClient.delete<null>(`/admin/channels/${id}`),
    onSuccess: invalidate,
  });
}

/** 手动启停（status 1 启用 / 2 手动禁用；reason 仅入审计） */
export function useSetChannelStatusMutation() {
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: ({ id, status, reason }: { id: number; status: number; reason?: string }) =>
      apiClient.put<{ item: ChannelItem }>("/admin/channel/status", { id, status, reason }),
    onSuccess: invalidate,
  });
}

/** 连通测试（返回结果对象；连通失败不抛错，由调用方读 result.ok） */
export function useTestChannelMutation() {
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: (id: number) => apiClient.post<{ result: ChannelTestResult }>("/admin/channel/test", { id }),
    onSuccess: invalidate,
  });
}

/**
 * 渠道密钥池明细（**只含指纹，不含明文**）。
 *
 * `enabled` 只在面板打开时拉：它带 Redis 冷却/最近错误的实时态，
 * 塞进列表接口会让每次渠道列表刷新都多打 N 次 Redis。
 */
export function useAdminChannelKeysQuery(id: number, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminChannelKeys(id),
    queryFn: () => apiClient.get<ChannelKeysResult>(`/admin/channels/${id}/keys`),
    enabled: enabled && id > 0,
  });
}

/**
 * 密钥池变更（add / replace / replace_all / remove / meta / reset_cooldown
 * 一次提交，原子生效）。这是**已有渠道**改密钥池的唯一入口 ——
 * `PUT /admin/channels/:id` 不再受理 keys。
 */
export function usePatchAdminChannelKeysMutation() {
  const queryClient = useQueryClient();
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: ChannelKeysPayload }) =>
      apiClient.patch<ChannelKeysResult>(`/admin/channels/${id}/keys`, body),
    onSuccess: (data, vars) => {
      queryClient.setQueryData(queryKeys.adminChannelKeys(vars.id), data);
      invalidate();
    },
  });
}

/* --------------------------- 内置预设（恢复出厂） --------------------------- */

/**
 * 恢复出厂（`dry_run` 只预览，事务回滚，库里不变）。
 *
 * 这是唯一会**覆盖**运营改名/改价/改绑定的动作，所以：
 *  - 后端要求 `confirm:"RESTORE"`（不是"点了个红按钮"就算确认，脚本误调也会被拦）；
 *  - 前端一律先 dry_run 拿到计数再让人确认，绝不直接写；
 *  - 只有真跑才失效缓存 —— dry_run 回的是预览数据，刷新列表纯属浪费且会让
 *    "预览"看起来像"已经改了"。
 */
export function useRestorePresetsMutation() {
  const queryClient = useQueryClient();
  const invalidate = useInvalidateChannels();
  return useMutation({
    mutationFn: (body: { group?: string; dry_run?: boolean; keep_enabled?: boolean; confirm?: string }) =>
      apiClient.post<PresetRestoreResult>("/admin/presets/restore", body),
    onSuccess: (data) => {
      if (!data.dry_run) invalidate();
    },
    onError: () => {
      // 失败时也要刷一次：事务可能已部分提交（单组内多条 UPDATE 在同一事务，
      // 整体回滚，但重试前让界面回到服务端事实比留着旧数字更安全）。
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminChannelsAll });
    },
  });
}

/* ------------------------------- S5 调用记录 ------------------------------- */

export interface PortalUsageParams {
  page?: number;
  page_size?: number;
  model?: string;
  modality?: string;
  status?: string;
  key_id?: number;
  /** RFC3339 闭区间上下界（由日期控件按本地日界转换） */
  start?: string;
  end?: string;
}

/** 门户调用记录（服务端分页，T21）；筛选参数变化即重新取数。 */
export function usePortalUsageQuery(params: PortalUsageParams) {
  return useQuery({
    queryKey: queryKeys.portalUsage(params),
    queryFn: ({ signal }) =>
      apiClient.get<PortalUsageResult>("/portal/usage", {
        signal,
        query: { page: 1, page_size: DEFAULT_PAGE_SIZE, ...params },
      }),
    placeholderData: (previous) => previous,
    // 只有列表里**存在进行中的记录**时才轮询：否则每次翻页都多打一个无意义请求。
    // refetchIntervalInBackground 保持默认 false —— 后台标签页不刷，省掉一整夜的无效流量。
    refetchInterval: (query) =>
      query.state.data?.items?.some((i) => i.status === "pending") ? 5000 : false,
  });
}

/** 门户调用记录详情（含原始报文与扣费明细）。 */
export function usePortalUsageDetailQuery(id: number | null) {
  return useQuery({
    queryKey: queryKeys.portalUsageDetail(id ?? 0),
    queryFn: ({ signal }) =>
      apiClient.get<PortalCallDetail>(`/portal/usage/${id}`, { signal }),
    enabled: id !== null && id > 0,
    // 进行中的记录详情也要跟着更新（结算完成后扣费与耗时才有值）。
    refetchInterval: (query) => (query.state.data?.status === "pending" ? 5000 : false),
  });
}

/** 导出当前筛选的本人调用记录 CSV（作用域恒为当前登录用户）。 */
export function exportPortalUsage(params: PortalUsageParams) {
  return downloadCSV("/portal/usage/export", { ...params }, "usage.csv");
}

/* ------------------------------- S7 异步任务 ------------------------------- */

export interface PortalTasksParams {
  page?: number;
  page_size?: number;
  status?: string;
  model?: string;
}

export interface AdminTasksParams extends PortalTasksParams {
  user_id?: number;
  /** 按所属应用筛选：0 = 平台直连（订单/日志/任务）；用户页不接受 0 */
  app_id?: number;
}

/** 门户异步任务（服务端分页，S7）；筛选参数变化即重新取数。 */
export function usePortalTasksQuery(params: PortalTasksParams) {
  return useQuery({
    queryKey: queryKeys.portalTasks(params),
    queryFn: () => apiClient.getPage<TaskItem>("/portal/tasks", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 管理台异步任务（服务端分页，S7）。 */
export function useAdminTasksQuery(params: AdminTasksParams) {
  return useQuery({
    queryKey: queryKeys.adminTasks(params),
    queryFn: ({ signal }) => apiClient.getPage<AdminTaskItem>("/admin/tasks", { ...params }, { signal }),
    placeholderData: (previous) => previous,
  });
}

/**
 * 单个任务详情（含产物清单 contents）。
 *
 * 列表故意不下发 contents（每条任务都要查一次产物表 = N+1），所以抽屉打开时才按需取；
 * 音乐任务一次出 2 首，没有这一步就只能下载到第 0 首。
 */
export function usePortalTaskQuery(id: string | null) {
  return useQuery({
    queryKey: queryKeys.portalTaskDetail(id ?? ""),
    queryFn: ({ signal }) => apiClient.get<TaskItem>(`/portal/tasks/${id}`, { signal }),
    enabled: id != null,
    // 进行中的任务详情要跟着变：抽屉开着时进度与产物都在推进。
    // 只在 queued/running 轮询，终态立刻停（与调用详情对 pending 的口径一致）。
    refetchInterval: (query) => (isActiveTask(query.state.data?.status ?? "") ? 5000 : false),
  });
}

/** 管理台单个任务详情（含产物清单，供运营排障逐个预览/下载）。 */
export function useAdminTaskQuery(id: string | null) {
  return useQuery({
    queryKey: queryKeys.adminTaskDetail(id ?? ""),
    queryFn: ({ signal }) => apiClient.get<AdminTaskItem>(`/admin/tasks/${id}`, { signal }),
    enabled: id != null,
    refetchInterval: (query) => (isActiveTask(query.state.data?.status ?? "") ? 5000 : false),
  });
}

/** 取消门户异步任务；成功后退款影响余额，故同时失效 me。 */
export function useCancelPortalTaskMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) =>
      apiClient.post<TaskItem>(`/portal/tasks/${id}/cancel`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalTasksAll });
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/** 取消管理台异步任务（后端写审计）。 */
export function useCancelAdminTaskMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiClient.post<AdminTaskItem>(`/admin/tasks/${id}/cancel`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminTasksAll });
    },
  });
}

/** 重试失败/超时任务（后端重置状态并重新冻结额度，由 Worker 重新接手）。 */
export function useRetryAdminTaskMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiClient.post<AdminTaskItem>(`/admin/tasks/${id}/retry`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminTasksAll });
    },
  });
}

/* ------------------------------- S8 文件上传 ------------------------------- */

export interface PortalFilesParams {
  page?: number;
  page_size?: number;
  /** KEY 可选筛选（跨当前用户全部 KEY） */
  key_id?: number;
  sort?: string;
}

/** 门户文件列表（服务端分页，S8；仅未过期件，跨当前用户全部 KEY）。 */
export function usePortalFilesQuery(params: PortalFilesParams) {
  return useQuery({
    queryKey: queryKeys.portalFiles(params),
    queryFn: () => apiClient.getPage<UploadItem>("/portal/files", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 门户文件配置（总闸 / 四重限额 / 当日用量快照）。 */
export function usePortalFilesConfigQuery() {
  return useQuery({
    queryKey: queryKeys.portalFilesConfig,
    queryFn: () => apiClient.get<UploadConfig>("/portal/files/config"),
  });
}

/**
 * 门户直传文件（会话身份，无需 API KEY）。
 *
 * 走 multipart 通道（apiClient.upload）：不能复用 request —— 它恒设
 * `Content-Type: application/json` 并 JSON.stringify，服务端 `c.FormFile` 会直接失败。
 *
 * 成功后失效 `portalFilesAll` 前缀：它同时覆盖列表与 config 快照，
 * 配额条（今日次数 / 在存容量）因此立刻反映这次上传。
 */
export function usePortalFileUploadMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
      return upload<UploadItem>("/portal/files", form);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalFilesAll });
    },
  });
}

/** 删除门户文件；成功后失效列表与 config（在存容量随之下调）。 */
export function useDeletePortalFileMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiClient.delete<{ id: string }>(`/portal/files/${id}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalFilesAll });
    },
  });
}

/* ------------------------------- S9 钱包 · 支付 · 兑换 · 邀请 ------------------------------- */

export interface PortalOrdersParams {
  page?: number;
  page_size?: number;
  status?: string;
}

export interface AdminOrdersParams extends PortalOrdersParams {
  user_id?: number;
  /** 按所属应用筛选：0 = 平台直连（订单/日志/任务）；用户页不接受 0 */
  app_id?: number;
  /** 稳定渠道 ID（推荐） */
  pay_channel_id?: string;
  /** 渠道展示快照（历史兼容，精确匹配） */
  pay_channel?: string;
}

export interface PortalAffRecordsParams {
  page?: number;
  page_size?: number;
}

/** 套餐 + 钱包配置 + 收银台支付方式（一次性取回，钱包页不做二次请求）；顺带注入计费换算常量。 */
export function usePortalPackagesQuery() {
  return useQuery({
    queryKey: queryKeys.portalPackages,
    queryFn: async () => {
      const data = await apiClient.get<WalletPackagesResult>("/portal/packages");
      setBillingConstants({
        scale: data.scale,
        cnyPerUsd: data.cny_per_usd,
        currency: data.currency,
      });
      return data;
    },
  });
}

/** 门户订单列表（服务端分页 + 状态筛选；收银台轮询复用本钩子并按需开启 refetchInterval）。 */
export function usePortalOrdersQuery(
  params: PortalOrdersParams,
  options?: { enabled?: boolean; refetchInterval?: number },
) {
  return useQuery({
    queryKey: queryKeys.portalOrders(params),
    queryFn: () => apiClient.getPage<OrderItem>("/portal/orders", { ...params }),
    placeholderData: (previous) => previous,
    enabled: options?.enabled ?? true,
    refetchInterval: options?.refetchInterval,
  });
}

export interface PortalTransactionsParams {
  page?: number;
  page_size?: number;
  /** 六型枚举（recharge/redeem/freeze/release/settle/refund/adjust/invite） */
  type?: string;
}

/** 门户资金流水（服务端分页 + 类型筛选；数据源与用户中心共用）。 */
export function usePortalTransactionsQuery(params: PortalTransactionsParams) {
  return useQuery({
    queryKey: queryKeys.portalTransactions(params),
    queryFn: () => apiClient.getPage<TransactionItem>("/portal/transactions", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 管理台订单列表（服务端分页 + 用户/状态筛选）。 */
export function useAdminOrdersQuery(params: AdminOrdersParams) {
  return useQuery({
    queryKey: queryKeys.adminOrders(params),
    queryFn: async ({ signal }) => {
      // 后端在分页四字段之外还下发 pay_channels（含停用渠道）：
      // 管理台筛选必须用它，否则渠道一停用就再也筛不出历史订单。
      const page = await apiClient.getPage<AdminOrderItem>("/admin/orders", { ...params }, { signal });
      return page as AdminOrdersResult;
    },
    placeholderData: (previous) => previous,
  });
}

/**
 * 下单入参：package_id 与 amount 二选一；method 来自 pay_options（承接渠道由服务端按方式路由）。
 *
 * `amount` 是**十进制金额字符串**（"10.5"），不是微、也不是 JSON number：
 * 后端 `money.Parse` 只收字符串，同名字段若传 number 会直接 400；传微则会放大 1e6 倍。
 * 生成它请用 `microToInput()`，不要 `String(微)`。
 */
export interface TopupBody {
  package_id?: number;
  amount?: string;
  method?: string;
}

/** 下单：返回签名收银台地址；成功后失效订单列表。 */
export function useTopupMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: TopupBody) => apiClient.post<TopupResult>("/portal/topup", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
    },
  });
}

/** 过期订单续付（沿用原金额与到账额度，生成新支付单号）。 */
export function useContinueOrderMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.post<TopupResult>(`/portal/orders/${id}/continue`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
    },
  });
}

/**
 * 对**同一订单**重新获取支付信息。
 *
 * 前端「支付通道暂不可用 → 重试」必须走这里，**不能用重新下单代替**：
 * 重新下单会换单号，而上游可能已有第一笔订单，用户付了新的又付旧的 → 双倍扣款。
 */
export function useRetryOrderPayMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.post<TopupResult>(`/portal/orders/${id}/pay`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
    },
  });
}

/**
 * 「我已支付，立即核对」：主动查单一次并返回最新状态。
 *
 * 查单失败不报错（query_ok=false）：查不到不等于没付，报错会让用户以为支付失效而重复付款。
 */
export function useOrderVerifyMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.post<VerifyResult>(`/portal/orders/${id}/verify`),
    onSuccess: (data) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalOrdersAll });
      if (data.order.status === "paid") {
        void queryClient.invalidateQueries({ queryKey: queryKeys.me });
        void queryClient.invalidateQueries({ queryKey: queryKeys.portalTransactionsAll });
      }
    },
  });
}

/** 二维码数据：image=已代理的 data URL、content=内容（前端降级）、none=无二维码。 */
export function useOrderQRCodeQuery(orderID: number | null, enabled = true) {
  return useQuery({
    queryKey: queryKeys.portalOrderQR(orderID ?? 0),
    queryFn: ({ signal }) =>
      apiClient.get<QRResult>(`/portal/orders/${orderID}/qrcode`, { signal }),
    enabled: enabled && !!orderID,
    // 二维码 5 分钟有效，30s 内不必重取；失败直接展示降级文案，不做自动重试。
    staleTime: 30_000,
    retry: false,
  });
}

/** 兑换码核销；成功后余额变化，失效 me 与订单列表（兑换会写 redeem 流水）。 */
export function useRedeemMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (code: string) => apiClient.post<{ amount: number }>("/portal/redeem", { code }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/** 邀请返利面板（邀请码 / 受邀人数 / 返利额度）。 */
export function usePortalAffQuery() {
  return useQuery({
    queryKey: queryKeys.portalAff,
    queryFn: () => apiClient.get<AffInfo>("/portal/aff"),
  });
}

/** 直邀记录（服务端分页）。 */
export function usePortalAffRecordsQuery(params: PortalAffRecordsParams) {
  return useQuery({
    queryKey: queryKeys.portalAffRecords(params),
    queryFn: () => apiClient.getPage<AffRecord>("/portal/aff/records", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 返利额度转入余额；成功后失效 aff 面板与 me。 */
export function useAffTransferMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => apiClient.post<{ amount: number }>("/portal/aff/transfer"),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalAff });
      void queryClient.invalidateQueries({ queryKey: queryKeys.me });
    },
  });
}

/**
 * 管理端退款（原因必填；后端写 order.refund 审计）。
 *
 * mode：gateway = 原路退回（虎皮椒有退款接口）；balance = 仅回退平台余额；缺省由后端按渠道自动判定。
 * 返回退款三态（success/pending/failed），调用方必须分别提示——pending 不是失败。
 */
export function useAdminOrderRefundMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: { order_id: number; reason: string; mode?: "gateway" | "balance" }) =>
      apiClient.post<OrderRefundOutcome>("/admin/order/refund", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminOrdersAll });
    },
  });
}

/**
 * 管理端确认退款结果（仅对 refunding 订单生效；后端写 order.refund_confirm 审计）。
 * result=success 视为上游已退款；result=failed 会回滚已扣额度并让订单回到 paid。
 */
export function useAdminOrderRefundConfirmMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    // evidence = 上游核实凭证：result=failed 会无条件回滚额度，后端强制要求（refund_evidence_required）。
    mutationFn: (body: {
      order_id: number;
      result: "success" | "failed";
      reason: string;
      evidence?: string;
    }) => apiClient.post<null>("/admin/order/refund/confirm", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminOrdersAll });
    },
  });
}

/** 管理端人工入账（need_manual 丢单兜底；原因必填；后端写 order.manual_credit 审计）。 */
export function useAdminOrderManualCreditMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: { order_id: number; reason: string }) =>
      apiClient.post<null>("/admin/order/manual-credit", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminOrdersAll });
    },
  });
}

/** 导出当前筛选的订单 CSV。 */
export function exportAdminOrders(params: AdminOrdersParams) {
  return downloadCSV("/admin/orders/export", { ...params }, "orders.csv");
}

/* ---------------------- S10 系统配置中心与通知 ---------------------- */

/** 站点公开配置（公开端点，全站共享一份；失败静默回落本地常量，不弹 toast）。 */
/**
 * 取一张图形验证码（一次拿到 id 与图，天然配对）。
 *
 * 不走 react-query：每次刷新都必须是一次新请求（答案一次性、图也不能复用），
 * 缓存住会让第二个人拿到同一张图。所以是普通函数，由 `useCaptcha` 调用。
 */
export function fetchCaptcha(scene: string): Promise<{ id: string; image: string }> {
  return apiClient.get<{ id: string; image: string }>(
    `/portal/auth/captcha?scene=${encodeURIComponent(scene)}`,
  );
}

export function usePortalConfigQuery() {
  return useQuery({
    queryKey: queryKeys.portalConfig,
    queryFn: () => apiClient.get<PortalConfig>("/portal/config"),
    // 30 秒：管理员改了人机验证策略，别的浏览器最迟半分钟就能看到。
    // 不设 0：useSite() 每页都挂这个 query，每次路由切换都发请求不值得。
    staleTime: 30_000,
    // 切回标签页立刻重取：否则"改了配置这边没反应"要等整整一个 staleTime。
    refetchOnWindowFocus: true,
    meta: { silent: true },
  });
}

/** 站点公告时间线（登录态；未登录不请求）。 */
export function useAnnouncementsQuery(enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.portalAnnouncements,
    queryFn: () => apiClient.get<AnnouncementsResult>("/portal/announcements"),
    enabled,
    meta: { silent: true },
  });
}

/**
 * 把当前全部公告标记已读；用响应**整份替换**缓存，无需重拉列表。
 *
 * 刻意不带请求体：要标记哪些公告由服务端从配置派生（客户端算 keys = 第二真相源）。
 * 也不做乐观置位：失败就保持未读，下次打开重试 —— 「假装已读」比多弹一次糟得多。
 */
export function useMarkAnnouncementsReadMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => apiClient.post<AnnouncementsResult>("/portal/announcements/read"),
    onSuccess: (data) => {
      queryClient.setQueryData<AnnouncementsResult>(queryKeys.portalAnnouncements, data);
    },
    meta: { silent: true },
  });
}

/** 管理台配置全量（含脱敏回显）；保存后必须失效，避免下次进入拿到旧值。 */
export function useAdminConfigQuery() {
  return useQuery({
    queryKey: queryKeys.adminConfig,
    queryFn: () => apiClient.get<AdminConfigResult>("/admin/config"),
  });
}

/** 保存配置：扁平「键 → 值」全量包；成功失效配置/公开配置/定价（计费参数可能变动）。 */
export function useSaveAdminConfigMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: Record<string, unknown>) =>
      apiClient.put<{ keys: string[] }>("/admin/config", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminConfig });
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalConfig });
      void queryClient.invalidateQueries({ queryKey: queryKeys.pricing });
      // 支付渠道就在这份配置里：不失效收银台选项的话，改完渠道方式
      // 切到门户看到的还是旧选项（同一 SPA 内 query 不会自己重取）。
      void queryClient.invalidateQueries({ queryKey: queryKeys.portalPackages });
      // 运营模式可能刚被切换：守卫读的是模块缓存，必须一起失效，
      // 否则保存完还要等 30 秒才按新模式走。
      invalidatePortalMode();
    },
  });
}

/**
 * 云存储连通性探针（能测**草稿**配置；连通失败也返回 200，由 ok 字段判定）。
 *
 * 传 {active, channels} 表示测尚未保存的那一份：保存即热生效，
 * 凭证填错会让在跑的产物转存全部失败，所以「先验后存」是必需的。
 * 不传则测当前已生效配置。
 */
export function useStorageTestMutation() {
  return useMutation({
    mutationFn: (draft?: { active: string; channels: Record<string, Record<string, string>> }) =>
      apiClient.post<StorageTestResult>("/admin/storage/test", draft ?? {}),
  });
}

/** 通知通道测试发送（target 必填；通道未启用返回 notify_channel_disabled）。 */
export function useNotifyTestMutation() {
  return useMutation({
    mutationFn: (body: { channel: "email" | "sms"; target: string }) =>
      apiClient.post<{ channel: string; target: string }>("/admin/notify/test", body),
  });
}

/* ---------------------- S15 门户主页聚合 ---------------------- */

/** 门户主页聚合（今日/本月用量 + 趋势 + 在途任务数；余额/冻结由 useMeQuery 提供）。 */
export function usePortalOverviewQuery(days: number) {
  return useQuery({
    queryKey: queryKeys.portalOverview(days),
    queryFn: () => apiClient.get<PortalOverview>("/portal/overview", { query: { days } }),
    // staleTime: 0 = **每次进入页面必定重取**。
    //
    // 全局默认是 30s，而这一页的「今日」是实时口径：留着 30s 会让用户切走再切回来
    // 时看到 30 秒前的数字，与卡片上「实时」的语义矛盾。不轮询已由决策确定，
    // 所以这里不做 refetchInterval —— 页面开着不动就不刷新，卡片上的
    // generated_at（「截至 14:32」）如实反映这一点。
    staleTime: 0,
    placeholderData: (previous) => previous,
  });
}

/** 各模态默认模型 + 是否被任一密钥授权（默认模型的唯一真相源）。 */
export function usePortalDefaultsQuery() {
  return useQuery({
    queryKey: queryKeys.portalDefaults,
    queryFn: () => apiClient.get<PortalDefaults>("/portal/defaults"),
    staleTime: 60_000,
  });
}

/* ---------------------- S11 管理台数据面与可观测 ---------------------- */

/** 调用日志筛选与分页参数（scope=hot 明细 / archive 归档）。 */
export interface AdminLogsParams extends PageQuery {
  scope?: "hot" | "archive";
  user_id?: number;
  /** 按所属应用筛选：0 = 平台直连（订单/日志/任务）；用户页不接受 0 */
  app_id?: number;
  model?: string;
  channel_id?: number;
  modality?: string;
  kind?: string;
  status?: string;
  /** 记录来源：sync / async / precheck */
  call_kind?: string;
  /** 精确检索：排障时手上最先有的是这两个 id */
  request_id?: string;
  upstream_request_id?: string;
  /** 上游密钥指纹（keyID）：多 key 渠道排「是不是就这一把在挂」 */
  channel_key_id?: string;
  usage_id?: number;
  /** 时间预设：1h / 3h / 24h（默认）/ 7d / 30d / all */
  range?: string;
  from?: string;
  to?: string;
}

/** 审计筛选与分页参数。 */
export interface AdminAuditsParams extends PageQuery {
  action?: string;
  admin_id?: number;
  keyword?: string;
  range?: string;
  from?: string;
  to?: string;
}

/** 经营看板（今日指标 + 近 7 日趋势 + d7/d30 双档聚合，单请求取全量）。 */
export function useAdminBoardQuery() {
  return useQuery({
    queryKey: queryKeys.adminBoard,
    queryFn: () => apiClient.get<BoardResult>("/admin/data/board"),
    staleTime: 30_000,
  });
}

/**
 * 容量体检（B7）：产物总量 + 磁盘水位 + 阈值与开关状态。
 *
 * 快照由后端 worker 每小时刷新，前端 60s 缓存即可 —— 磁盘水位不会秒级变化，
 * 频繁轮询只会让「管理台开着」变成持续的 DB 压力。
 */
export function useAdminCapacityQuery() {
  return useQuery({
    queryKey: queryKeys.adminCapacity,
    queryFn: ({ signal }) => apiClient.get<AdminCapacityResult>("/admin/data/capacity", { signal }),
    staleTime: 60_000,
  });
}

/** 调用日志明细/归档（服务端分页，多条件筛选）。 */
export function useAdminLogsQuery(params: AdminLogsParams) {
  return useQuery({
    queryKey: queryKeys.adminLogs(params),
    queryFn: ({ signal }) =>
      apiClient.getPage<AdminLogItem>("/admin/logs", { ...params }, { signal }),
    placeholderData: (previous) => previous,
    // 同门户：只有存在进行中的记录才 5 秒刷一次。
    refetchInterval: (query) =>
      query.state.data?.items?.some((i) => i.status === "pending") ? 5000 : false,
  });
}

/** 管理台调用记录详情（含原始报文、上游错误原文与逐项扣费）。 */
export function useAdminLogDetailQuery(id: number | null, scope: "hot" | "archive") {
  return useQuery({
    queryKey: queryKeys.adminLogDetail(id ?? 0, scope),
    queryFn: ({ signal }) => apiClient.get<AdminCallDetail>(`/admin/logs/${id}`, { signal, query: { scope } }),
    enabled: id !== null && id > 0,
    refetchInterval: (query) => (query.state.data?.status === "pending" ? 5000 : false),
  });
}

/** 调用日志分析（类型分布 + 错误排行，与筛选联动、不受分页影响）。 */
export function useAdminLogAnalyticsQuery(params: AdminLogsParams) {
  return useQuery({
    queryKey: queryKeys.adminLogAnalytics(params),
    queryFn: () =>
      apiClient.get<LogAnalyticsResult>("/admin/logs/analytics", {
        query: { ...params, page: undefined, page_size: undefined },
      }),
  });
}

/** 审计日志明细（动作/操作人/关键字/时间组合过滤）。 */
export function useAdminAuditsQuery(params: AdminAuditsParams) {
  return useQuery({
    queryKey: queryKeys.adminAudits(params),
    queryFn: () => apiClient.getPage<AdminAuditItem>("/admin/audits", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 审计元信息：库内实际动作集合（数据驱动筛选）+ 保留天数。 */
export function useAdminAuditMetaQuery() {
  return useQuery({
    queryKey: queryKeys.adminAuditMeta,
    queryFn: () => apiClient.get<AuditMetaResult>("/admin/audits/meta"),
    staleTime: 5 * 60_000,
  });
}

/** 导出当前筛选的调用日志 CSV（列契约 E5，Bearer 鉴权下载）。 */
export function exportAdminLogs(params: AdminLogsParams) {
  return downloadCSV("/admin/logs/export", { ...params }, "logs.csv");
}

/** 导出当前筛选的审计日志 CSV。 */
export function exportAdminAudits(params: AdminAuditsParams) {
  return downloadCSV("/admin/audits/export", { ...params }, "audit.csv");
}

/* ---------------------- 认证事件审计 ---------------------- */

/** 认证事件筛选与分页参数（uid 传数字，其余为字符串枚举）。 */
export interface AdminAuthEventsParams extends PageQuery {
  event?: string;
  result?: string;
  uid?: number;
  ip?: string;
  account?: string;
  range?: string;
  from?: string;
  to?: string;
}

/** 认证事件列表（登录/注册/改密/二次验证流水）。 */
export function useAdminAuthEventsQuery(params: AdminAuthEventsParams) {
  return useQuery({
    queryKey: queryKeys.adminAuthEvents(params),
    queryFn: () => apiClient.getPage<AuthEventItem>("/admin/auth-events", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 认证事件元信息：可选事件（编译期白名单，含中文名）+ 保留天数。 */
export function useAdminAuthEventMetaQuery() {
  return useQuery({
    queryKey: queryKeys.adminAuthEventMeta,
    queryFn: () => apiClient.get<AuthEventMetaResult>("/admin/auth-events/meta"),
    staleTime: 5 * 60_000,
  });
}

/** 导出当前筛选的认证事件 CSV。 */
export function exportAdminAuthEvents(params: AdminAuthEventsParams) {
  return downloadCSV("/admin/auth-events/export", { ...params }, "auth-events.csv");
}

/* ---------------------- S16 后台兑换码管理 ---------------------- */

/** 兑换码列表筛选与分页参数（status 取后端派生枚举）。 */
export interface AdminRedeemsParams extends PageQuery {
  batch?: string;
  status?: string;
}

export interface RedeemGenerateBody {
  batch: string;
  /** 面额（微）。0 或负数由后端拒绝：一张不给钱的码不该由生成接口产生。 */
  amount: number;
  count: number;
  /** YYYY-MM-DD；空串 = 永不过期 */
  expires_at?: string;
}

/** 兑换码列表（batch/status 可选筛选，服务端分页）。 */
export function useAdminRedeemsQuery(params: AdminRedeemsParams) {
  return useQuery({
    queryKey: queryKeys.adminRedeems(params),
    queryFn: () => apiClient.getPage<RedeemItem>("/admin/redeems", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 批量生成兑换码：成功后失效列表（新码即刻可见）。 */
export function useAdminRedeemGenerateMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: RedeemGenerateBody) =>
      apiClient.post<RedeemGenerateResult>("/admin/redeems", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminRedeemsAll });
    },
  });
}

/** 批量作废（仅未使用的码生效，已用码跳过）：幂等，返回 deleted/skipped 计数。 */
export function useAdminRedeemDeleteMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (ids: number[]) =>
      apiClient.post<RedeemDeleteResult>("/admin/redeems/delete", { ids }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminRedeemsAll });
    },
  });
}

/** 导出当前筛选的兑换码 CSV。 */
export function exportAdminRedeems(params: AdminRedeemsParams) {
  return downloadCSV("/admin/redeems/export", { ...params }, "redeem-codes.csv");
}

/* ---------------------- 充值套餐（后台配置） ---------------------- */

/**
 * 套餐列表。
 *
 * 返回里带 balance_limit / scale / min_price_cent / max_price_cent / max_bonus：
 * 表单的区间上下限、额度预览、超上限红标全部由服务端下发，前端不硬编码任何业务常量。
 * （售价是**分**——支付渠道的最小单位；到账额是**微**。两者互转用 lib/money 的
 * microFromMinor，别在页面里自己乘 10000。）
 */
export function useAdminPackagesQuery(params: AdminPackagesParams) {
  return useQuery({
    queryKey: queryKeys.adminPackages(params),
    queryFn: () => apiClient.get<AdminPackagesResult>("/admin/packages", { query: { ...params } }),
    placeholderData: (previous) => previous,
  });
}

/**
 * 写操作的统一失效策略：**必须同时失效门户套餐键**。
 *
 * 只失效 adminPackagesAll 时，同一 SPA 内切到门户页不会重新请求，
 * 用户会看到改价前的旧价格 —— 这正是「改了价、门户没变」的直接原因。
 */
function useInvalidatePackages() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.adminPackagesAll });
    void queryClient.invalidateQueries({ queryKey: queryKeys.portalPackages });
  };
}

/** 新建套餐（enabled 仅创建时生效）。 */
export function useAdminPackageCreateMutation() {
  const invalidate = useInvalidatePackages();
  return useMutation({
    mutationFn: (body: PackagePayload) =>
      apiClient.post<PackageWriteResult>("/admin/packages", body),
    onSuccess: () => invalidate(),
  });
}

/** 编辑套餐（PUT 不接收 enabled，启停走独立接口）。 */
export function useAdminPackageUpdateMutation() {
  const invalidate = useInvalidatePackages();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: PackagePayload }) =>
      apiClient.put<PackageWriteResult>(`/admin/packages/${id}`, body),
    onSuccess: () => invalidate(),
  });
}

/** 启停套餐：与编辑分开，审计动作也不同（package.status）。 */
export function useAdminPackageStatusMutation() {
  const invalidate = useInvalidatePackages();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      apiClient.patch<{ item: AdminPackageItem }>(`/admin/packages/${id}/status`, { enabled }),
    onSuccess: () => invalidate(),
  });
}

/** 删除套餐：有订单引用时后端回 409 package_in_use（前端按钮同时也置灰）。 */
export function useAdminPackageDeleteMutation() {
  const invalidate = useInvalidatePackages();
  return useMutation({
    mutationFn: (id: number) => apiClient.delete<null>(`/admin/packages/${id}`),
    onSuccess: () => invalidate(),
  });
}

/* ---------------------- S17 用户详情抽屉（只读三块） ---------------------- */

/** 用户详情抽屉的资金流水筛选（type 可选） */
export interface AdminUserDetailParams extends PageQuery {
  type?: string;
}

/** 指定用户的资金流水（只读；与门户 /portal/transactions 共用同一 service 口径）。 */
export function useAdminUserTransactionsQuery(id: number, params: AdminUserDetailParams) {
  return useQuery({
    queryKey: queryKeys.adminUserTransactions(id, params),
    queryFn: () =>
      apiClient.getPage<TransactionItem>(`/admin/users/${id}/transactions`, { ...params }),
    enabled: id > 0,
    placeholderData: (previous) => previous,
  });
}

/** 指定用户的密钥列表（只读；**不含明文**——管理台刻意不提供密钥回显能力）。 */
export function useAdminUserKeysQuery(id: number) {
  return useQuery({
    queryKey: queryKeys.adminUserKeys(id),
    queryFn: () => apiClient.get<{ items: AdminUserKeyItem[] }>(`/admin/users/${id}/keys`),
    enabled: id > 0,
  });
}

/* ---------------------- 资金对账（只读报表 + 补收返利欠款） ---------------------- */

/**
 * 对账报表。所有数字都由后端 SQL 算好，前端**不做任何算术**：
 * 这里的每一项都是「两份账本对不上」的计数，自己再算一遍只会引入第二套口径。
 */
export interface ReconcileChainRow {
  user_id: number;
  txn_id: number;
  prev_balance_after: number;
  amount: number;
  balance_after: number;
  /** balance_after − (prev_balance_after + amount)，单位微；正常恒为 0。 */
  delta: number;
}

/**
 * 一条「任务与账务口径不一致」的样本。
 *
 * 金额字段（settled / charge / task_frozen / usage_frozen）**全是微**。
 * `task_frozen` 与 `usage_frozen` 必须分开列：tasks 侧是给运营看的副本，
 * usages 侧才是扣款的依据，只有并排看才知道该改哪一边。
 */
export interface ReconcileTaskRow {
  task_id: number;
  status: string;
  settled: number;
  charge: number;
  task_frozen: number;
  usage_frozen: number;
  /**
   * settled_shortfall / charged_but_failed / settled_mismatch /
   * pending_after_terminal / frozen_mismatch
   */
  issue: string;
}

export interface ReconcileReport {
  generated_at: string;
  /**
   * 本次**未完成**的检查段（users / usages / ledger / orders / refunds /
   * affiliate / tasks / indexes）。
   *
   * 必须单独渲染：失败段的计数全是 0，与「查过、没差异」在页面上完全同形。
   * 不显示这一栏，一次没跑成的对账会被当成「账目完美」。
   */
  errors: string[];
  users: {
    checked: number;
    chain_mismatch: number;
    balance_mismatch: number;
    frozen_mismatch: number;
    negative_frozen: number;
    samples: unknown[];
    chain_samples: ReconcileChainRow[];
  };
  usages: {
    pending_total: number;
    pending_old: number;
    pending_orphan: number;
    estimated_24h: number;
    reclaimed_24h: number;
    zero_usage_24h: number;
  };
  ledger: {
    total_rows: number;
    earliest: string;
    duplicate_usage_rows: number;
    charge_rows: number;
    charge_diff: number;
    samples: unknown[];
  };
  orders: {
    paid_orders: number;
    recharge_rows: number;
    missing_ledger: number;
    duplicate_ledger: number;
    pending_but_credited: number;
    amount_mismatch: number;
    samples: unknown[];
  };
  refunds: {
    refunding: number;
    stuck_refunding: number;
    missing_refund_ledger: number;
    gateway_without_no: number;
    upstream_only: number;
  };
  /**
   * 邀请返利凭据一致性。
   *
   * 返利额度由 affiliate_rewards 派生，**没有欠款可补收**：
   * leak 是「钱已退、返利仍有效」的真差异；missing 是「已入账却没发返利」；
   * ledger_diff 是转出行与 invite 流水之差（应恒 0）；
   * cooling 是冷却中未转出的观察值（不是错误）。
   */
  affiliate: {
    leak_rows: number;
    leak_total: number;
    missing_rows: number;
    /** 已标记转出的返利总额（微） */
    transferred_sum: number;
    /** 转出总额 − invite 流水总额；正=标记了没入账，负=入账了没标记 */
    ledger_diff: number;
    cooling_rows: number;
    cooling_total: number;
    samples: ReconcileAffiliateRow[];
  };
  /** 异步任务与 usages 的口径一致性（任务链路的钱全在 usages 里） */
  tasks: {
    settled_mismatch: number;
    pending_after_terminal: number;
    frozen_mismatch: number;
    /** task.frozen − usage.frozen 的合计，单位微；正数 = 任务侧记的冻结额偏大 */
    frozen_diff: number;
    /**
     * 任务已成功交付、但账已**释放**（没收钱）。
     *
     * 成因：迟到成功补收失败 / 计费记录已归档 / 提交拿到单号后状态推进失败。
     * 钱压根没动过，所以余额链、流水都不会不平 —— 只有这一项能发现。
     */
    settled_shortfall: number;
    /** 收了钱但任务失败/超时/取消（settled>0 且 charge>0），需要人工退款 */
    charged_but_failed: number;
    samples: ReconcileTaskRow[];
  };
  indexes: { missing: string[] };
}

/** 一条返利凭据差异（金额单位是**微**） */
export interface ReconcileAffiliateRow {
  order_id: number;
  invitee_id: number;
  /** 受益人；missing_reward 样本为 0（补发时还没有这一行） */
  beneficiary_id: number;
  /** inviter | invitee；missing_reward 样本为空串 */
  role: string;
  amount: number;
  available_at: string;
  /** refund_leak（钱退了返利还在）| missing_reward（已入账却无返利行） */
  issue: string;
}

/** 补发结果（只补凭据，不动余额） */
export interface AffBackfillResult {
  scanned: number;
  rewarded: number;
  skipped: number;
}

/** 资金对账报表（只读，GET /admin/data/reconcile）。 */
export function useAdminReconcileQuery() {
  return useQuery({
    queryKey: queryKeys.adminReconcile,
    queryFn: ({ signal }) => apiClient.get<ReconcileReport>("/admin/data/reconcile", { signal }),
  });
}

/**
 * 补发「已入账但没有任何返利行」的订单（POST /admin/data/reconcile/affiliate/backfill）。
 *
 * 只补凭据、**不动余额**：用户仍要自己点「转入余额」。返回 dry_run 时是试算。
 * 成功后必须让对账报表失效 —— 否则刚补完的 missing_rows 还挂在旧数字上。
 */
export function useAffiliateBackfillMutation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: { days?: number; limit?: number; dry_run?: boolean }) =>
      apiClient.post<AffBackfillResult>("/admin/data/reconcile/affiliate/backfill", body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.adminReconcile });
    },
  });
}

/* ============================================================
   应用管理（管理台）
   ============================================================ */

export interface AdminAppsParams extends PageQuery {
  keyword?: string;
  status?: number;
  /** 1 = 只看维护中；0 = 只看正常 */
  maintenance?: number;
  sort?: "id" | "created_at" | "name";
}

export interface AdminAppKeysParams extends PageQuery {}

export interface AdminAppNoticesParams extends PageQuery {}

export interface AdminAppReleasesParams extends PageQuery {
  status?: number;
}

/** 应用列表（含密钥数/用户数/消费/充值聚合）。 */
export function useAdminAppsQuery(params: AdminAppsParams) {
  return useQuery({
    queryKey: queryKeys.adminApps(params),
    queryFn: () => apiClient.getPage<AdminAppItem>("/admin/apps", { ...params }),
    placeholderData: (previous) => previous,
  });
}

/** 应用下拉选项的参数：只在结果被截断时才带 keyword 回查。 */
export interface AdminAppOptionsParams {
  keyword?: string;
}

/**
 * 应用下拉选项（轻量：只回 id/slug/name/display_name/status）。
 *
 * 三个刻意的选择：
 *  1. `staleTime 5 分钟`：应用增删是低频动作，而管理台四个页面都要用它，
 *     不设就会每次进页面重拉一遍；
 *  2. `meta.silent`：这是"全站共享但可缺失"的基础数据，拉不到不该在每个页面
 *     弹一次错误提示（全局 QueryCache 会对所有查询 toast）；
 *  3. 返回值**归一成 items: []**：接口挂掉时页面直接 map 不会炸，
 *     筛选器退化成"只有全部应用"，而不是整页白屏。
 */
export function useAdminAppOptionsQuery(params: AdminAppOptionsParams = {}) {
  const query = useQuery({
    queryKey: queryKeys.adminAppOptions(params),
    queryFn: () =>
      apiClient.get<{ items: AdminAppOption[]; truncated: boolean }>("/admin/apps/options", {
        query: { ...params },
      }),
    staleTime: 5 * 60_000,
    meta: { silent: true },
  });
  return {
    ...query,
    items: query.data?.items ?? [],
    truncated: query.data?.truncated ?? false,
  };
}

/** 新建应用。此时不存在任何详情缓存，全量失效一次最省事。 */
export function useCreateAppMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (payload: AppPayload) => apiClient.post<{ id: number; slug: string }>("/admin/apps", payload),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppsAll });
    },
  });
}

/**
 * 改名称/描述（slug 不可改：接入方按它调用，改名等于全部失效）。
 *
 * 列表要刷（卡片标题取 `display_name || name`），品牌也要刷 ——
 * 展示名留空时回落后台名称，所以改名会改到公开面的展示名。
 */
export function useUpdateAppMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, payload }: { id: number; payload: AppPayload }) =>
      apiClient.put<void>(`/admin/apps/${id}`, payload),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppBrand(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

/** 启用/停用（停用只拦新签发；公开面照常返回，原因见后端 app_public.go）。 */
export function useSetAppStatusMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, status }: { id: number; status: number }) =>
      apiClient.patch<void>(`/admin/apps/${id}/status`, { status }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

/** 应用下的托管密钥（后台只读：无明文、无密文）。 */
export function useAdminAppKeysQuery(id: number, params: AdminAppKeysParams, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppKeys(id, params),
    queryFn: () => apiClient.getPage<AdminAppKeyItem>(`/admin/apps/${id}/keys`, { ...params }),
    enabled,
    placeholderData: (previous) => previous,
  });
}

/**
 * KEY 兜底启停（含托管密钥解锁）。
 * status=2 的托管密钥唯一来源是管理员动作，这里是它的唯一恢复通道。
 *
 * **appId 必须一起传**：只有 key id 定位不到"哪个应用的密钥列表要刷"，
 * 而密钥抽屉正开着的时候不刷就是"点了没反应"。
 */
export function useAdminKeyStatusMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, status }: { id: number; status: number; appId?: number }) =>
      apiClient.patch<void>(`/admin/keys/${id}/status`, { status }),
    onSuccess: (_data, { appId }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
      if (appId) void qc.invalidateQueries({ queryKey: queryKeys.adminAppKeysAll(appId) });
    },
  });
}

/* ---- 应用级后台：品牌 / 统计 / 公告 / 版本 / 维护 ---- */

/**
 * 品牌（9 字段 + 图标令牌 + 图标地址）。
 *
 * 与统计分开：品牌页签只需要这 10 个字段，不该为了回填表单先等一次 7 日聚合。
 */
export function useAdminAppBrandQuery(id: number, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppBrand(id),
    queryFn: () => apiClient.get<AdminAppBrand>(`/admin/apps/${id}/brand`),
    enabled: enabled && id > 0,
  });
}

/**
 * 统计（7 日趋势 + 在线指标）。
 *
 * 概览与「在线」页签共用这一条：两个仪表盘各算一份的下场是数字对不上，
 * 而谁都不知道哪个对。四格汇总（消费/充值/密钥/用户）**不在这里** ——
 * 它们随列表卡片一起下发，弹窗直接用列表行，于是卡片与弹窗天然同源。
 */
export function useAdminAppStatsQuery(id: number, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppStats(id),
    queryFn: () => apiClient.get<AdminAppStats>(`/admin/apps/${id}/stats`),
    enabled: enabled && id > 0,
  });
}

/** 保存品牌（全量 9 字段）。回显归一化后的值，供表单立刻更新预览。 */
export function useSaveAppBrandMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, payload }: { id: number; payload: AppBrandPayload }) =>
      apiClient.put<AdminAppBrand>(`/admin/apps/${id}/brand`, payload),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppBrand(id) });
      // 卡片显示 display_name/tagline/logo_url，必须跟着刷；
      // **不刷 stats** —— 少算一次 7 日聚合，这正是拆分要解决的浪费。
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

/**
 * 维护模式开关。
 *
 * 语义与「停用」严格不同：停用只拦新签发，维护只拦存量流量（/v1 503）。
 * 两者不联动 —— 一次点击不该同时完成两件事。
 */
export function useSetAppMaintenanceMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled, message }: { id: number; enabled: boolean; message: string }) =>
      apiClient.patch<{ maintenance_mode: boolean; maintenance_message: string }>(
        `/admin/apps/${id}/maintenance`,
        { enabled, message },
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

/** 应用公告（管理台视角，含已停用）。 */
export function useAdminAppNoticesQuery(id: number, params: AdminAppNoticesParams, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppNotices(id, params),
    queryFn: () => apiClient.getPage<AdminAppNotice>(`/admin/apps/${id}/notices`, { ...params }),
    enabled: enabled && id > 0,
    placeholderData: (previous) => previous,
  });
}

export function useCreateAppNoticeMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, payload }: { id: number; payload: AppNoticePayload }) =>
      apiClient.post<AdminAppNotice>(`/admin/apps/${id}/notices`, payload),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppNoticesAll(id) });
      // 卡片上有"生效公告数"。
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

export function useUpdateAppNoticeMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, nid, payload }: { id: number; nid: number; payload: AppNoticeUpdatePayload }) =>
      apiClient.put<void>(`/admin/apps/${id}/notices/${nid}`, payload),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppNoticesAll(id) });
      // 改时间窗会改"此刻生效"的条数，卡片要跟着变。
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

export function useSetAppNoticeStatusMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, nid, enabled }: { id: number; nid: number; enabled: boolean }) =>
      apiClient.patch<void>(`/admin/apps/${id}/notices/${nid}/status`, { enabled }),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppNoticesAll(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

export function useDeleteAppNoticeMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, nid }: { id: number; nid: number }) =>
      apiClient.delete<void>(`/admin/apps/${id}/notices/${nid}`),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppNoticesAll(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

/** 应用版本（含草稿与已撤回）。 */
export function useAdminAppReleasesQuery(id: number, params: AdminAppReleasesParams, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppReleases(id, params),
    // getPage 只保证分页四字段，current_release_id 是本端点额外的字段，
    // 用显式的页面类型收口（UI 需要它标出"当前下发"的版本）。
    queryFn: async () =>
      (await apiClient.getPage<AdminAppRelease>(`/admin/apps/${id}/releases`, {
        ...params,
      })) as AdminAppReleasePage,
    enabled: enabled && id > 0,
    placeholderData: (previous) => previous,
  });
}

/**
 * 新建版本：后端一律落草稿，发布是独立动作。
 *
 * 不刷列表：草稿不会出现在卡片的"最近发布"上（那里按 published_at 取已发布行）。
 */
export function useCreateAppReleaseMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, payload }: { id: number; payload: AppReleasePayload }) =>
      apiClient.post<AdminAppRelease>(`/admin/apps/${id}/releases`, payload),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppReleasesAll(id) });
    },
  });
}

/** 编辑版本：已发布行只能改说明/下载地址（版本号只读），不影响卡片。 */
export function useUpdateAppReleaseMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, rid, payload }: { id: number; rid: number; payload: AppReleasePayload }) =>
      apiClient.put<void>(`/admin/apps/${id}/releases/${rid}`, payload),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppReleasesAll(id) });
    },
  });
}

/** status：2 发布 / 3 撤回。会改卡片的"最近发布"，所以列表也要刷。 */
export function useSetAppReleaseStatusMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, rid, status }: { id: number; rid: number; status: 2 | 3 }) =>
      apiClient.patch<void>(`/admin/apps/${id}/releases/${rid}/status`, { status }),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppReleasesAll(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppListAll });
    },
  });
}

/** 删除版本：仅草稿可删，不影响卡片的"最近发布"。 */
export function useDeleteAppReleaseMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, rid }: { id: number; rid: number }) =>
      apiClient.delete<void>(`/admin/apps/${id}/releases/${rid}`),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppReleasesAll(id) });
    },
  });
}

/* ============================================================
   开放接口手册（管理台只读）
   ============================================================ */

/** 接口契约目录：分组 + 每条端点 + 不开放清单。 */
export function useApiDocCatalogQuery() {
  return useQuery({
    queryKey: queryKeys.apiDocs,
    queryFn: () => apiClient.get<ApiDocCatalog>("/admin/api-docs"),
    // 契约定稿后基本不变：5 分钟内重复进页面不重复拉取。
    staleTime: 5 * 60 * 1000,
  });
}

/**
 * 导出接口文件。
 *
 * 复用 downloadCSV：它做的事就是「带凭证 fetch → Blob → 触发保存」，
 * 与格式无关（文件名取自 Content-Disposition）。刻意**不**用 window.open：
 * 管理台响应是 private 缓存的鉴权资源，直链导航在 401 时会渲染成一段 JSON 错误页。
 */
export function exportApiDocFile(kind: "openapi" | "markdown"): Promise<void> {
  const name = kind === "openapi" ? "lingdong-openapi.json" : "lingdong-api.md";
  return downloadCSV(`/admin/api-docs/${kind === "openapi" ? "openapi.json" : "markdown"}`, undefined, name);
}

/**
 * 把后端下发的文档视图地址变成浏览器可打开的地址。
 *
 * 为什么不能直接用 `links.swagger_ui`：它是**站内绝对路径**（`/api/admin/...`），
 * 而 API 基址本身已经含 `/api`（跨域部署时还是另一个域名）。直接导航会在
 * 「API 不在同域」的部署下打到前端站点的 404，而同域部署时又恰好能开 ——
 * 这种"只在一半环境里坏"的写法最难发现。
 *
 * 走 `apiUrl`（唯一的基址来源）保证与 apiClient 打同一个后端；Swagger UI 必须
 * 由浏览器直接导航（它要读管理台 Cookie），所以这里只算地址、不 fetch。
 */
export function apiDocsViewURL(link: string): string {
  return apiUrl(link.replace(/^\/api(?=\/)/, ""));
}

/* ============================================================
   应用插件（声明式）
   ============================================================ */

/** 插件目录（不含 manifest 全文）。 */
export function useAdminAppPluginsQuery(params: PageQuery = {}, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppPlugins(params),
    queryFn: () => apiClient.getPage<AdminAppPluginItem>("/admin/app-plugins", params),
    enabled,
  });
}

/** 出厂示例模板（不落库，见后端 appplugins.SampleManifest 注释）。 */
export function useAdminAppPluginSampleQuery(enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.adminAppPluginSample,
    queryFn: () => apiClient.get<{ manifest: unknown }>("/admin/app-plugins/sample"),
    enabled,
    staleTime: Infinity,
  });
}

/** 目录详情：编译后的页面描述 + manifest 原文 + 安装数。 */
export function useAdminAppPluginQuery(key: string, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppPlugin(key),
    queryFn: () =>
      apiClient.get<{
        plugin: AdminAppPluginDetail["plugin"];
        manifest: unknown;
        status: number;
        source: string;
        lock_version: number;
        installs: number;
        updated_at: string;
      }>(`/admin/app-plugins/${key}`),
    enabled: enabled && !!key,
  });
}

/**
 * 导入/升级目录。`dryRun` 走 `?dry_run=1`：只体检不落库。
 *
 * 注意两个分支**都**用同一个 mutation：预检与导入的返回形状相同（applied 区分），
 * 拆两个 mutation 会让"预检通过 → 导入"之间多一次缓存失效，反而容易看到旧状态。
 */
export function useImportAppPluginMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      key,
      manifest,
      expectedVersion,
      force,
      dryRun,
    }: {
      key: string;
      manifest: unknown;
      expectedVersion: number;
      force?: boolean;
      dryRun?: boolean;
    }) =>
      apiClient.put<AdminAppPluginImportResult>(
        `/admin/app-plugins/${key}${dryRun ? "?dry_run=1" : ""}`,
        { manifest, expected_version: expectedVersion, force: !!force },
      ),
    onSuccess: (res) => {
      // 预检不落库，不需要失效任何东西；导入才要刷目录与详情。
      if (!res.applied) return;
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginsAll });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPlugin(res.key) });
    },
  });
}

/** 上架/下架目录。下架只拦新装与新写，已安装照常读。 */
export function useSetAppPluginCatalogStatusMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ key, status }: { key: string; status: number }) =>
      apiClient.patch<void>(`/admin/app-plugins/${key}/status`, { status }),
    onSuccess: (_d, { key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginsAll });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPlugin(key) });
    },
  });
}

/** 删除目录行（仍有应用在用时后端 409，文案带引用数）。 */
export function useDeleteAppPluginMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (key: string) => apiClient.delete<void>(`/admin/app-plugins/${key}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginsAll });
    },
  });
}

/** 某应用已安装的插件（含已卸载：它的入口要用"重新启用 / 清除数据"）。 */
export function useAdminAppPluginInstallsQuery(id: number, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppPluginInstalls(id),
    queryFn: () => apiClient.get<{ items: AdminAppPluginInstall[] }>(`/admin/apps/${id}/plugins`),
    enabled: enabled && id > 0,
  });
}

/** 泛化路由页面的唯一数据源（安装态 + 描述符 + 漂移提示）。 */
export function useAdminAppPluginDetailQuery(id: number, key: string, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppPluginDetail(id, key),
    queryFn: () => apiClient.get<AdminAppPluginDetail>(`/admin/apps/${id}/plugins/${key}`),
    enabled: enabled && id > 0 && !!key,
    // 卸载/启用后安装态会变，但描述符不变：保留旧数据能让页面不闪。
    placeholderData: (previous) => previous,
  });
}

export function useInstallAppPluginMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, pluginKey, config }: { id: number; pluginKey: string; config: Record<string, unknown> }) =>
      apiClient.post<void>(`/admin/apps/${id}/plugins`, { plugin_key: pluginKey, config }),
    onSuccess: (_d, { id }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginInstalls(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginsAll });
    },
  });
}

/** 改配置 / 启停 / 卸载：同一个 PATCH，按 body 里带了什么区分。 */
export function usePatchAppPluginMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      key,
      config,
      status,
      publicEnabled,
      expectedVersion,
    }: {
      id: number;
      key: string;
      config?: Record<string, unknown>;
      status?: number;
      publicEnabled?: boolean;
      expectedVersion: number;
    }) =>
      apiClient.patch<void>(`/admin/apps/${id}/plugins/${key}`, {
        config,
        status,
        public_enabled: publicEnabled,
        expected_version: expectedVersion,
      }),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginInstalls(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(id, key) });
    },
  });
}

/** 卸载：状态置 3，数据保留。 */
export function useUninstallAppPluginMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, key }: { id: number; key: string }) =>
      apiClient.delete<void>(`/admin/apps/${id}/plugins/${key}`),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginInstalls(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(id, key) });
    },
  });
}

/** 清除数据：物理删除该应用下这个插件的全部记录（要求已卸载）。 */
export function usePurgeAppPluginMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, key }: { id: number; key: string }) =>
      apiClient.delete<{ deleted: number }>(`/admin/apps/${id}/plugins/${key}/data`),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginInstalls(id) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(id, key) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginRecordsAll(id, key) });
    },
  });
}

/** 记录分页。`enabled` 由调用方按"已卸载"关掉，避免明知 404 还发请求。 */
export function useAdminAppPluginRecordsQuery(id: number, key: string, params: PageQuery, enabled = true) {
  return useQuery({
    queryKey: queryKeys.adminAppPluginRecords(id, key, params),
    queryFn: () => apiClient.getPage<AdminAppPluginRecord>(`/admin/apps/${id}/plugins/${key}/records`, params),
    enabled: enabled && id > 0 && !!key && !!params.entity,
    placeholderData: (previous) => previous,
  });
}

export function useCreateAppPluginRecordMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      key,
      entity,
      data,
    }: {
      id: number;
      key: string;
      entity: string;
      data: Record<string, unknown>;
    }) => apiClient.post<AdminAppPluginRecord>(`/admin/apps/${id}/plugins/${key}/records`, { entity, data }),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginRecordsAll(id, key) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(id, key) });
    },
  });
}

export function useUpdateAppPluginRecordMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      key,
      entity,
      recordKey,
      data,
      version,
    }: {
      id: number;
      key: string;
      entity: string;
      recordKey: string;
      data: Record<string, unknown>;
      version: number;
    }) =>
      apiClient.put<AdminAppPluginRecord>(`/admin/apps/${id}/plugins/${key}/records/${recordKey}`, {
        entity,
        data,
        version,
      }),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginRecordsAll(id, key) });
    },
  });
}

export function useDeleteAppPluginRecordMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, key, entity, recordKey }: { id: number; key: string; entity: string; recordKey: string }) =>
      apiClient.delete<void>(`/admin/apps/${id}/plugins/${key}/records/${recordKey}?entity=${encodeURIComponent(entity)}`),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginRecordsAll(id, key) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(id, key) });
    },
  });
}

/**
 * 批量删除（单事务）。
 *
 * 上限 100 条由后端把关：前端不复制这个数字，只在超限时把后端消息原样弹出来 ——
 * 两处各写一个上限，改一处就会漂。
 */
export function useBatchDeleteAppPluginRecordsMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      key,
      entity,
      recordKeys,
    }: {
      id: number;
      key: string;
      entity: string;
      recordKeys: string[];
    }) =>
      apiClient.post<{ deleted: number }>(`/admin/apps/${id}/plugins/${key}/records/batch-delete`, {
        entity,
        record_keys: recordKeys,
      }),
    onSuccess: (_d, { id, key }) => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginRecordsAll(id, key) });
      void qc.invalidateQueries({ queryKey: queryKeys.adminAppPluginDetail(id, key) });
    },
  });
}

/**
 * 导出当前筛选的记录 CSV。
 *
 * 排序不跟着屏幕走（后端恒按创建顺序）：keyset 依赖单调列，
 * 按数据列排序只能整表排或内存排，5 万行的上限就是为此设的。
 */
export function exportAppPluginRecords(
  id: number,
  key: string,
  params: PageQuery & { entity: string },
): Promise<void> {
  return downloadCSV(`/admin/apps/${id}/plugins/${key}/records/export`, { ...params }, `${key}-${params.entity}.csv`);
}

/**
 * 下载「应用插件开发手册」（markdown，由后端从代码生成）。
 *
 * 复用 downloadCSV：它名字里有 CSV，但做的是"凭会话取回 Blob 并按
 * Content-Disposition 存盘"，与文件类型无关（重命名会牵动 8 个调用点）。
 */
export function downloadPluginDevDoc(): Promise<void> {
  return downloadCSV("/admin/app-plugins/dev-doc.md", undefined, "应用插件开发手册.md");
}

/* ---------------- 版本与一键更新 ---------------- */

/**
 * 版本状态（当前版本 / 清单 / 更新程序 / 进行中的任务）。
 *
 * `staleTime` 5 分钟 + `retry: 0` + 静默：它挂在侧栏上，每个后台页面都会渲染；
 * 让它成为"每页都报错一次的请求"会把真正的错误淹掉。
 * 这个接口**只读缓存**，不会出站抓清单（抓取在 `useReleaseCheckMutation`）。
 */
export function useAdminReleaseQuery() {
  return useQuery({
    queryKey: queryKeys.adminRelease,
    queryFn: () => apiClient.get<ReleaseInfo>("/admin/system/release"),
    staleTime: 5 * 60_000,
    retry: 0,
    meta: { silent: true },
  });
}

/** 强制检查一次（服务端有 15s 冷却；冷却内的连点不会重复出站）。 */
export function useReleaseCheckMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiClient.post<ReleaseInfo>("/admin/system/release/check"),
    onSuccess: (data) => {
      qc.setQueryData(queryKeys.adminRelease, data);
    },
  });
}

/** 提交一次一键更新。`force` 用于"同版本刚失败过"或"降级"的显式确认。 */
export function useReleaseUpdateMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { target_tag: string; request_id: string; force?: boolean }) =>
      apiClient.post<ReleaseJob>("/admin/system/release/update", body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminReleaseCurrent });
      void qc.invalidateQueries({ queryKey: queryKeys.adminRelease });
    },
  });
}

/** 取消一次尚未开始的更新（仅 queued 可取消）。 */
export function useReleaseCancelMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (jobId: number) =>
      apiClient.post<{ canceled: boolean }>("/admin/system/release/update/cancel", { job_id: jobId }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.adminReleaseCurrent });
      void qc.invalidateQueries({ queryKey: queryKeys.adminRelease });
    },
  });
}

/**
 * 更新进度轮询。
 *
 * 只在有进行中的任务时 2s 轮询，终态立即停止。`retry: 0` 是刻意的：
 * 切换容器的那几十秒后端必然连不上，重试只会把日志刷满；
 * 界面据 `isError && hasData` 显示"服务重启中"。
 */
export function useReleaseCurrentQuery(enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.adminReleaseCurrent,
    queryFn: () => apiClient.get<ReleaseCurrent>("/admin/system/release/update/current"),
    enabled,
    retry: 0,
    refetchOnMount: "always",
    refetchInterval: (query) => {
      const d = query.state.data as ReleaseCurrent | undefined;
      return d?.active ? 2000 : false;
    },
    meta: { silent: true },
  });
}
