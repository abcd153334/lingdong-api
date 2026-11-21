import { useEffect, useMemo, useRef, useState, type ChangeEvent } from "react";
import { toast } from "sonner";
import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { sendCodeText } from "@/lib/sendCode";
import { ApiError } from "@/lib/apiClient";
import { Input } from "@/components/common/Input";
import { MoneyInput } from "@/components/common/MoneyInput";
import { PageHead } from "@/components/common/PageHead";
import { PageTabs } from "@/components/common/PageTabs";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Tabs } from "@/components/common/Tabs";
import { Dialog } from "@/components/common/Dialog";
import { Select } from "@/components/common/Select";
import { EntitySelect } from "@/components/common/EntitySelect";
import { SwitchRow } from "@/components/common/SwitchRow";
import { SiteLogo } from "@/components/common/SiteLogo";
import { PayChannelsCard } from "@/components/admin/PayChannelsCard";
import { PAY_METHOD_ITEMS, SECRET_KEYS } from "@/lib/payChannel";
import { CHANNEL_META } from "@/lib/contactChannel";
import { getMoneyCtx, groupDiscountText, money2, ratioPercent } from "@/lib/money";
import { FALLBACK_SCHEMAS } from "@/lib/payChannelSchema";
import { usePageTabs, type PageTab } from "@/lib/pageTabs";
import {
  applyTabSlice,
  configTabDirty,
  configTabPayload,
  mergeTabBaseline,
  tabOfMessage,
  unownedConfigKeys,
  type ConfigTab,
} from "@/lib/configTabs";
import { Icon } from "@/components/common/Icon";
import {
  useAdminConfigQuery,
  useAdminModelsQuery,
  useNotifyTestMutation,
  useSaveAdminConfigMutation,
  useStorageTestMutation,
} from "@/lib/api";
import type {
  AdminConfigValues,
  AnnouncementItem,
  AnnouncementType,
  AuthMfaSettings,
  EmailTpl,
  Modality,
} from "@/types";

/* ----------------------------- 静态字典 ----------------------------- */

/**
 * 系统配置 7 个分区（?tab=）。
 *
 * 切分依据是**操作链**而不是后端键名：钱怎么算（计费）与钱怎么收（支付渠道）分开，
 * 因为前者是「改个数字」，后者是「填密钥 / 排序 / 启停」，两者的风险与频率都不同；
 * 混在一页时，改折扣会被「渠道密钥还没填完」拦下（保存是整页提交的）。
 *
 * 类型与「页签 ↔ 配置键」的归属表在 `lib/configTabs.ts`：保存是按页签分片的，
 * 归属只在那里定义一次（本文件只负责渲染）。
 */
export type { ConfigTab } from "@/lib/configTabs";

const CURRENCY_OPTIONS: [string, string][] = [
  ["CNY", "人民币 CNY（¥）"],
  ["USD", "美元 USD（$）"],
];

const CONFIG_TABS: readonly PageTab<ConfigTab>[] = [
  { key: "general", label: "通用" },
  { key: "billing", label: "计费" },
  { key: "pay", label: "支付渠道" },
  { key: "storage", label: "存储" },
  { key: "notify", label: "通知" },
  { key: "models", label: "模型与调用" },
  { key: "security", label: "安全与数据" },
];

/** 邮件模板 4 场景（reset_password 为事务类，不受场景开关约束） */
export type EmailScene = "reset_password" | "task_done" | "low_balance" | "price_change";

const EM_SCENES: { key: EmailScene; label: string; tip: string }[] = [
  { key: "reset_password", label: "密码重置", tip: "忘记密码验证码，事务类始终发送；正文须含 {{code}}" },
  { key: "task_done", label: "任务完成", tip: "异步任务落库后通知" },
  { key: "low_balance", label: "余额不足", tip: "余额低于阈值时通知" },
  { key: "price_change", label: "价目变动", tip: "模型价目调整通知" },
];

/** 场景页签（?scene=）；EM_SCENES 带 tip，不能直接当 TabItem 数组传 */
const EM_SCENE_TABS: readonly PageTab<EmailScene>[] = EM_SCENES.map((s) => ({ key: s.key, label: s.label }));

/** 邮件场景开关（仅 3 个可选场景，密码重置由事务链路直接触发） */
const EM_TOGGLES = [
  { key: "task_done", label: "任务完成通知", tip: "任务产物就绪后发送" },
  { key: "low_balance", label: "余额不足提醒", tip: "余额低于阈值时发送" },
  { key: "price_change", label: "价目变动通知", tip: "模型价目调整后发送" },
];

/** 模板缺省内容（首次进入补齐空模板，保证可保存且变量可读） */
const EM_TPL_DEFAULTS: Record<string, EmailTpl> = {
  reset_password: {
    subject: "【{{site_name}}】密码重置验证码",
    body: "您正在重置登录密码，验证码：{{code}}，10 分钟内有效。如非本人操作，请忽略本邮件。",
  },
  task_done: {
    subject: "【{{site_name}}】任务已完成",
    body: "您的任务已生成完毕，可前往「任务中心」查看产物与下载链接。",
  },
  low_balance: {
    subject: "【{{site_name}}】余额不足提醒",
    body: "您的账户余额已低于提醒阈值，请及时充值以免影响接口调用。",
  },
  price_change: {
    subject: "【{{site_name}}】模型价目变动通知",
    body: "平台模型价目已更新，最新价格请以「模型广场」展示为准。",
  },
};

const EM_ENCRYPTIONS: [string, string][] = [
  ["none", "不加密"],
  ["ssl", "SSL"],
  ["starttls", "STARTTLS"],
];

type ChannelScheme = {
  id: string;
  name: string;
  /** [键, 标签, 是否必填] */
  fields: [string, string, boolean][];
};

/** 云存储渠道 schema 由后端下发（AdminConfigResult.storage_schemes），前端不再维护副本 */

/** 短信渠道（必填项与后端 smsSchemes 一致） */
const SMS_SCHEMES: ChannelScheme[] = [
  {
    id: "aliyun",
    name: "阿里云短信",
    fields: [
      ["access_key_id", "AccessKey ID", true],
      ["access_key_secret", "AccessKey Secret", true],
      ["sign_name", "短信签名", true],
      ["template_code", "模板 CODE", true],
    ],
  },
  {
    id: "tencent",
    name: "腾讯云短信",
    fields: [
      ["secret_id", "SecretId", true],
      ["secret_key", "SecretKey", true],
      ["sdk_app_id", "SDK App ID", true],
      ["sign_name", "短信签名", true],
      ["template_id", "模板 ID", true],
    ],
  },
];

const MODALITY_CARDS: { key: Modality; label: string }[] = [
  { key: "chat", label: "文本对话" },
  { key: "image", label: "图像生成" },
  { key: "video", label: "视频生成" },
  { key: "audio", label: "音频" },
  // 音乐是与音频并列的第 8 类模态：后端 modality_defaults 里一直有这个键，
  // 前端少了这张卡 → 管理员无法为音乐配默认模型（保存时靠整包回写才没丢数据）。
  { key: "music", label: "音乐生成" },
  { key: "embedding", label: "向量嵌入" },
  { key: "rerank", label: "重排序" },
  { key: "moderation", label: "内容审核" },
];

const ANN_TYPES: [AnnouncementType, string][] = [
  ["default", "公告"],
  ["ongoing", "进行中"],
  ["success", "已完成"],
  ["warning", "注意"],
  ["error", "重要"],
];

/** 站点图片的类型白名单：Logo 与标签图标共用（后端两者都是 dataURI，同一组 mime） */
const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp", "image/svg+xml"];
const LOGO_MAX_BYTES = 2 * 1024 * 1024;
/**
 * 图标上限按**编码后**推算，而不是随手写个 100KB：
 * dataURI 约为原文件 1.37 倍，100KB → ≈133KB，落在后端 `sitecfg.MaxFaviconBytes`
 * （140KB）之内。写成 2MB 的话，前端放行、后端 400，运营看到的是"上传成功但保存失败"。
 */
const FAVICON_MAX_BYTES = 100 * 1024;
/** 多地址条数上限，与后端 `sitecfg.MaxBases` 一致 */
const MAX_API_BASES = 8;

/* ----------------------------- 小组件 ----------------------------- */

const num = (value: string): number => (value === "" ? 0 : Number(value));

/** 非空字符串判定（模板字段可能整体缺失：持久化配置里空模板是 `{}` 而非 `{subject:"",body:""}`） */
const hasText = (v: unknown): v is string => typeof v === "string" && v.trim() !== "";

/** 模板缺省补齐：空/字段缺失的模板回落默认文案，保证邮件键始终可保存（后端校验要求 4 场景主题+正文非空） */
function fillTemplates(src: Record<string, EmailTpl> | undefined): Record<string, EmailTpl> {
  const out: Record<string, EmailTpl> = {};
  for (const scene of EM_SCENES) {
    const cur = src?.[scene.key];
    out[scene.key] =
      cur && hasText(cur.subject) && hasText(cur.body) ? cur : { ...EM_TPL_DEFAULTS[scene.key] };
  }
  return out;
}

/* ------------------------------- 页面 ------------------------------- */

/** 上传文件类别：顺序与后端 `uploadcfg.Kinds` 一致，标签只在前端用。 */
const UPLOAD_KIND_LABELS: { key: string; label: string }[] = [
  { key: "image", label: "图片" },
  { key: "audio", label: "音频" },
  { key: "video", label: "视频" },
  { key: "document", label: "文档" },
  { key: "text", label: "文本" },
];

/** 压缩输出格式（与后端 uploadcfg.Format* 一一对应）。 */
const COMPRESS_FORMAT_ITEMS = [
  { key: "original", label: "原格式" },
  { key: "jpeg", label: "JPEG" },
  { key: "webp", label: "WebP" },
];

export function AdminConfigPage() {
  const { data, isLoading, isError, refetch } = useAdminConfigQuery();
  const modelsQuery = useAdminModelsQuery({});
  const save = useSaveAdminConfigMutation();
  const storageTest = useStorageTestMutation();
  const notifyTest = useNotifyTestMutation();

  const { tab, setTab } = usePageTabs(CONFIG_TABS, "general");
  const emailScene = usePageTabs(EM_SCENE_TABS, "reset_password", { param: "scene" });
  const [form, setForm] = useState<AdminConfigValues | null>(null);
  const [initial, setInitial] = useState<AdminConfigValues | null>(null);
  const [emailTarget, setEmailTarget] = useState("");
  const [smsTarget, setSmsTarget] = useState("");
  const [noticeDlg, setNoticeDlg] = useState<{ index: number | null; draft: AnnouncementItem } | null>(null);
  // 「新增后缀」的输入草稿：按类别各存一份，避免在一行里打字把别的行也清空。
  const [draftExt, setDraftExt] = useState<Record<string, string>>({});
  const logoRef = useRef<HTMLInputElement>(null);
  const faviconRef = useRef<HTMLInputElement>(null);

  // 服务端配置仅作首次播种，后续重取（保存后失效）不覆盖用户正在编辑的内容
  useEffect(() => {
    if (data && form === null) {
      const built: AdminConfigValues = structuredClone(data.config);
      built.email.templates = fillTemplates(built.email.templates);
      // 运营模式缺键 = 开启（与后端 conf.Bool 的默认值同语义）：
      // 不能因为键还没写库就让表单显示成「已关闭」——那会让人以为线上真的关了。
      built.portal_enabled = built.portal_enabled ?? true;
      setForm(built);
      setInitial(built);
    }
  }, [data, form]);

  // 归属兜底：新增配置键却没在 `lib/configTabs.ts` 登记时，它既不进提交包、
  // 也不参与脏判定 —— 也就是「怎么改都保存不出去」。dev 下一眼可见，生产不打扰。
  useEffect(() => {
    if (!import.meta.env.DEV || !data) return;
    const orphans = unownedConfigKeys(Object.keys(data.config));
    if (orphans.length > 0) {
      console.warn("[systemconfig] 未归属任何页签的配置键：", orphans);
    }
  }, [data]);

  const patch = (updater: (prev: AdminConfigValues) => AdminConfigValues): void =>
    setForm((prev) => (prev ? updater(prev) : prev));

  /**
   * 切换运营模式。
   *
   * 关闭是「断掉一条对外通道」，所以只在**关闭**时二次确认（开启不需要）。
   * 同时提醒先处理未完成订单：会员一旦登不进来，待支付订单就只能等超时。
   */
  const togglePortal = async (next: boolean) => {
    if (!next) {
      const ok = await confirmDialog({
        title: "关闭门户端？",
        body: "关闭后门户站点、系统内注册与登录入口立即不可用，仅管理员可登录管理后台；API 调用与已有密钥不受影响。建议先处理未完成的订单与退款。",
        okText: "关闭门户",
        okVariant: "danger",
      });
      if (!ok) return;
    }
    patch((p) => ({ ...p, portal_enabled: next }));
  };

  // 增删一个「支持的后缀」。后缀归一成小写去点：`PNG` 与 `.png` 必须落成同一个键，
  // 否则配置里会出现两个只差大小写的后缀，而上传只认其中一个。
  const addUploadExt = (kind: string): void => {
    const ext = (draftExt[kind] ?? "").trim().toLowerCase().replace(/^\./, "");
    if (!ext) return;
    patch((f) => ({ ...f, uploads: { ...f.uploads, types: { ...f.uploads.types, [ext]: kind } } }));
    setDraftExt((m) => ({ ...m, [kind]: "" }));
  };
  const removeUploadExt = (ext: string): void => {
    patch((f) => {
      const next = { ...f.uploads.types };
      delete next[ext];
      return { ...f, uploads: { ...f.uploads, types: next } };
    });
  };

  const modelOptions = useMemo(() => {
    const items = modelsQuery.data?.items ?? [];
    // 带图标：默认模型是"用户没指定时用的那个"，选错一行就要等线上报错才发现，
    // 而这一屏 5 个下拉里名字常常只差一个后缀（如 -mini / -pro）。
    const byModality = new Map<Modality, { id: string; label: string; iconUrl: string }[]>();
    for (const m of items) {
      if (!m.enabled) continue;
      const list = byModality.get(m.modality) ?? [];
      list.push({ id: m.id, label: m.display_name || m.id, iconUrl: m.icon_url });
      byModality.set(m.modality, list);
    }
    return byModality;
  }, [modelsQuery.data]);

  // 哪些页签还有没保存的改动：保存是分页签的，不标出来就会出现
  // 「在计费页改了折扣 → 切到通知页保存 → 离开页面」这种静默丢改动。
  const dirtyTabs = useMemo<ConfigTab[]>(
    () =>
      form && initial
        ? CONFIG_TABS.filter((t) => configTabDirty(t.key, form, initial)).map((t) => t.key)
        : [],
    [form, initial],
  );

  if (isError && !form) {
    return (
      <>
        <PageHead title="系统配置" desc="站点、计费、支付、存储、通知与安全策略；按页签保存，只提交本页的键" />
        <Card>
          <EmptyState
            variant="error"
            title="配置加载失败"
            desc="未能读取 system_config，请检查网络后重试"
            action={
              <button type="button" className="btn" onClick={() => void refetch()}>
                重试
              </button>
            }
          />
        </Card>
      </>
    );
  }

  if (isLoading || !form) {
    return (
      <>
        <PageHead title="系统配置" desc="站点、计费、支付、存储、通知与安全策略；按页签保存，只提交本页的键" />
        <Card>
          <EmptyState variant="loading" title="配置加载中" desc="正在读取 system_config" />
        </Card>
      </>
    );
  }

  const storage = form.storage;
  const sms = form.sms;
  // 云存储渠道 schema 由后端下发（systemconfig.StorageSchemes）：前端不再维护
  // 第二份字段列表 —— 漂移的表现是「后端支持的字段在前端填不了」。
  const storageSchemes: ChannelScheme[] = (data?.storage_schemes ?? []).map((s) => ({
    id: s.id,
    name: s.name,
    fields: s.fields.map(([key, label, req]) => [key, label, req === "1"] as [string, string, boolean]),
  }));
  const storageActive: ChannelScheme = storageSchemes.find((s) => s.id === storage.active) ??
    storageSchemes[0] ?? { id: storage.active, name: storage.active, fields: [] };
  // 进程里**正在用**的写入位置（与表单草稿值 storage.active 不同）：保存后才一致。
  const storageEff = data?.storage_effective;
  const storageEffText = storageEff?.writable
    ? [storageEff.label, storageEff.bucket && `· ${storageEff.bucket}`, storageEff.endpoint && `@ ${storageEff.endpoint}`]
        .filter(Boolean)
        .join(" ")
    : "";
  const smsActive = SMS_SCHEMES.find((s) => s.id === sms.active) ?? SMS_SCHEMES[0];

  /* ------------------- 币种切换：金额口径的实时提示 -------------------
   *
   * 金额是绝对量（微），切币种**不会**重算库里的数字：原本 ¥72 的上限切到美元后
   * 数值仍是 72，但含义变成 $72。这不是 bug，是"金额不带汇率"的必然结果 ——
   * 但运营必须看见它，否则会以为"我明明设过 72 元，怎么变成 72 美元了"。
   */
  const currencyDraftChanged = (form.site.currency || "CNY") !== (initial?.site.currency || "CNY");
  /** 符号跟着币种走，不可自定义（配置里已无 currency_symbol） */
  const symNow = form.site.currency === "USD" ? "$" : "¥";
  const symWas = initial?.site.currency === "USD" ? "$" : "¥";
  const scale = getMoneyCtx().scale;
  /** 折扣输入（倍数，0.9 = 九折）；库里存的是微比例 */
  const discountOf = (micro: number): number => (micro > 0 ? micro / scale : 1);

  /**
   * 邀请返利的**单笔**成本算式（保存前唯一能发现「赠金比充值还高」的地方）。
   *
   * 只用来提示，不参与校验：真正的硬约束在后端（赠金每笔不超过该笔实付）。
   *
   * 必须按「单笔」比：想拿满 N 笔赠金，用户要付 N 笔最低充值（合计 N × min_topup）。
   * 旧写法拿「N × 赠金」去比「单笔 min_topup」，N=3 时只要赠金 ≥ min_topup/3 就告警，
   * 默认值（3×5 元 vs 10 元）也会常亮 —— 告警一旦常亮就等于没有。
   */
  const affPerOrderCost =
    (form.aff.invitee_amount ?? 0) +
    Math.round(((form.aff.inviter_rate ?? 0) * (form.min_topup ?? 0)) / scale);
  const affPayoutRisk =
    affPerOrderCost > 0 && (form.min_topup ?? 0) > 0 && affPerOrderCost >= (form.min_topup ?? 0);

  const setChannelParam = (
    group: "storage" | "sms",
    channel: string,
    key: string,
    value: string,
  ): void => {
    patch((f) => {
      if (group === "storage") {
        const channels = {
          ...f.storage.channels,
          [channel]: { ...(f.storage.channels[channel] ?? {}), [key]: value },
        };
        return { ...f, storage: { ...f.storage, channels } };
      }
      const channels = { ...f.sms.channels, [channel]: { ...(f.sms.channels[channel] ?? {}), [key]: value } };
      return { ...f, sms: { ...f.sms, channels } };
    });
  };

  /**
   * 保存**当前页签**。
   *
   * 只提交本页签拥有的键（见 `lib/configTabs.ts`）：整包提交时，任何一个页签里
   * 半填的草稿都会让整次保存被拒 —— 改一个开关却被另一页的「域名还没填」拦下。
   * 后端 `Update` 的语义是「未出现的键保持原值」，分片提交不会丢掉其它页的配置。
   */
  const submitTab = (): void => {
    if (!form || !initial) return;
    // 支付渠道 preflight：后端也会拦，但这里能把错误定位到具体渠道。
    // 只在支付渠道页签拦：其它页签不提交 pay_channels，后端的这条校验也不会跑。
    if (tab === "pay") {
      for (const ch of form.pay_channels) {
        if (ch.enabled && ch.methods.length === 0) {
          toast.error(`渠道「${ch.name}」已启用，请至少选择一种支付方式`);
          return;
        }
        if (ch.name.includes("-")) {
          toast.error(`渠道名称「${ch.name}」不能包含中划线（-）`);
          return;
        }
      }
    }
    save.mutate(configTabPayload(tab, form, initial), {
      onSuccess: () => {
        // 基线只推进本页签那一片：整份替换会把其它页签里未保存的编辑
        // 一起标记成「已保存」——切回去看着已生效、实际没落库。
        setInitial((prev) => (prev ? mergeTabBaseline(tab, form, prev) : prev));
        toast.success("本页配置已保存并即时生效");
      },
      // 后端只回中文文案（ApiError 无字段路径），按「key 前缀」+「领域文案」两级定位出错的页签，
      // 避免运营拿着 toast 在 7 个分区里翻。命中不唯一就不跳（跳错比不跳更糟）。
      onError: (err) => {
        const target = tabOfMessage(err instanceof Error ? err.message : "");
        if (target && target !== tab) setTab(target);
      },
    });
  };

  /** 本页签是否有未保存改动（只看本页签拥有的键） */
  const dirty = initial ? configTabDirty(tab, form, initial) : false;

  /** 还原本页签：只丢本页签的编辑，其它页签的未保存内容必须留着 */
  const resetTab = (): void => {
    if (!form || !initial) return;
    setForm(applyTabSlice(tab, form, initial));
    toast.info("已还原本页签为上次保存的内容");
  };

  /**
   * 站点图片读取（Logo 与标签图标共用）。
   *
   * 两处各写一份 FileReader 的结局是：类型白名单/体积上限只改了一边，
   * 另一处照样传上去被后端 400。差异只有「目标字段」与「体积上限」两个入参。
   */
  const pickImage = (field: "logo_image" | "favicon_image", file: File | undefined): void => {
    if (!file) return;
    if (!IMAGE_TYPES.includes(file.type)) {
      toast.error("仅支持 PNG / JPG / WebP / SVG");
      return;
    }
    const max = field === "favicon_image" ? FAVICON_MAX_BYTES : LOGO_MAX_BYTES;
    if (file.size > max) {
      toast.error(field === "favicon_image" ? "图标不能超过 100KB" : "图片不能超过 2MB");
      return;
    }
    const reader = new FileReader();
    reader.onload = () =>
      patch((f) => ({ ...f, site: { ...f.site, [field]: String(reader.result ?? "") } }));
    reader.readAsDataURL(file);
  };

  const openNotice = (index: number | null): void => {
    const items = form.announcements.items;
    const draft: AnnouncementItem =
      index === null
        ? { id: items.reduce((max, it) => Math.max(max, it.id), 0) + 1, content: "", publishDate: new Date().toISOString(), type: "default", extra: "" }
        : { ...items[index] };
    setNoticeDlg({ index, draft });
  };

  const saveNotice = (): void => {
    if (!noticeDlg) return;
    const { index, draft } = noticeDlg;
    patch((f) => {
      const items = [...f.announcements.items];
      if (index === null) items.unshift(draft);
      else items[index] = draft;
      return { ...f, announcements: { ...f.announcements, items } };
    });
    setNoticeDlg(null);
  };

  const removeNotice = (index: number): void =>
    patch((f) => ({
      ...f,
      announcements: { ...f.announcements, items: f.announcements.items.filter((_, i) => i !== index) },
    }));

  /** 保存 / 还原放在页头：它们是「整页」的动作，跟着页签走会让运营在长页面里来回找。 */
  const headActions = [
    {
      type: "btn" as const,
      label: "还原本页",
      onClick: resetTab,
      disabled: !dirty || save.isPending,
    },
    {
      type: "btn" as const,
      label: "保存本页",
      variant: "primary" as const,
      onClick: submitTab,
      disabled: !dirty || save.isPending,
    },
  ];

  // 图标预览必须走**同一条兜底链**（图标 → Logo → 站名首字）：只显示 favicon_image
  // 的话，运营会以为"没设置就没有图标"，而实际标签页上早已是 Logo 或首字。
  const faviconSrc = form.site.favicon_image || form.site.logo_image;

  return (
    <>
      <PageHead
        title="系统配置"
        desc="站点、计费、支付、存储、通知与安全策略；按页签保存，只提交本页的键"
        note={dirty ? "本页有未保存的改动" : undefined}
        actions={headActions}
      />

      {/* 7 个分区用 render-prop 条件渲染（不是 key={tab} 的全量重建）：
          全部编辑态都在页面级的 form 快照里，切页签只换面板子树，未保存的编辑不会丢。 */}
      <PageTabs
        tabs={CONFIG_TABS}
        value={tab}
        onChange={setTab}
        idPrefix="admin-config"
        label="系统配置分区"
        markedKeys={dirtyTabs}
      >
        {(panel) => (
          <>
            {panel === "general" && (
              <>
                <Card title="运营模式" sub="决定这套部署对外提供什么：门户网站 + 管理台，还是只提供服务端">
                  <SwitchRow
                    label="启用门户端"
                    checked={form.portal_enabled !== false}
                    onChange={(v) => void togglePortal(v)}
                  />
                  <div className="small muted">
                    关闭后：门户站点不可访问，系统内注册与登录只允许管理员；API 调用、已有密钥、文档与定价接口照常。注册模式仍按「安全与数据」里的设置执行，只是仅对 API 面生效。
                  </div>
                  {form.portal_enabled === false ? (
                    <div className="small muted">
                      保存后本节点立即生效；其它节点最迟 30 秒同步，已打开的页面会在下一次操作时要求重新登录。
                    </div>
                  ) : null}
                </Card>
                <Card
                  title="版本与更新"
                  sub="发布清单地址：管理台据此判断有没有新版本；留空 = 用官方清单，填 off = 关闭检查"
                >
                  <Field
                    label="发布清单地址"
                    tip="一个 https 公网地址，返回 { schema:1, latest:{ version, tag, published_at, notes } }。保存后即时生效；一键更新在侧栏底部的版本号里。"
                  >
                    <Input
                      value={form.release_manifest_url ?? ""}
                      placeholder="留空 = 官方发布清单；off = 关闭自动检查"
                      onChange={(e) => patch((f) => ({ ...f, release_manifest_url: e.target.value }))}
                    />
                  </Field>
                  <div className="small muted">
                    只接受 https 公网地址：清单决定“要不要把线上换成另一个版本”，明文链路可以被中间人改写。
                    地址变更后旧结果立即作废，需重新检查一次。
                  </div>
                </Card>
                <Card title="站点品牌" sub="名称 / 图标 / 对外地址与页脚；保存后全站即时生效">
                  <Field label="站点 Logo 图片（可选）">
                    <div className="logo-up">
                      <div className="logo-preview">
                        <SiteLogo image={form.site.logo_image} name={form.site.site_name} />
                      </div>
                      <div className="logo-up-ops">
                        <button type="button" className="btn btn-sm" onClick={() => logoRef.current?.click()}>
                          上传图片
                        </button>
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={!form.site.logo_image}
                          onClick={() => patch((f) => ({ ...f, site: { ...f.site, logo_image: "" } }))}
                        >
                          移除
                        </button>
                        {/* 块级小字：inline 的话会被按钮挤在同一行，375 下断成两截 */}
                        <div className="small muted">PNG / JPG / WebP / SVG，≤2MB，建议方形透明底。</div>
                      </div>
                    </div>
                    <input
                      ref={logoRef}
                      type="file"
                      accept={IMAGE_TYPES.join(",")}
                      hidden
                      onChange={(e) => {
                        pickImage("logo_image", e.target.files?.[0]);
                        e.target.value = "";
                      }}
                    />
                  </Field>
                  <Field
                    label="站点图标（浏览器标签页）"
                    tip="未上传时依次回落到 Logo 图片、站名首字"
                  >
                    <div className="logo-up">
                      <div className="logo-preview logo-preview--icon">
                        {faviconSrc ? (
                          <img src={faviconSrc} alt="站点图标" className="logo-img" />
                        ) : (
                          <SiteLogo image="" name={form.site.site_name} />
                        )}
                      </div>
                      <div className="logo-up-ops">
                        <button type="button" className="btn btn-sm" onClick={() => faviconRef.current?.click()}>
                          上传图标
                        </button>
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={!form.site.favicon_image}
                          onClick={() => patch((f) => ({ ...f, site: { ...f.site, favicon_image: "" } }))}
                        >
                          移除
                        </button>
                        <div className="small muted">PNG / SVG，≤100KB，建议 64×64</div>
                      </div>
                    </div>
                    <input
                      ref={faviconRef}
                      type="file"
                      accept={IMAGE_TYPES.join(",")}
                      hidden
                      onChange={(e) => {
                        pickImage("favicon_image", e.target.files?.[0]);
                        e.target.value = "";
                      }}
                    />
                  </Field>
                  <div className="form-grid">
                    <Field label="站点名称" req>
                      <Input
                        value={form.site.site_name}
                        onChange={(e) => patch((f) => ({ ...f, site: { ...f.site, site_name: e.target.value } }))}
                      />
                    </Field>
                    <Field label="站点地址（可选）">
                      <Input
                        className="mono"
                        placeholder="https://www.example.com"
                        value={form.site.site_url}
                        onChange={(e) => patch((f) => ({ ...f, site: { ...f.site, site_url: e.target.value } }))}
                      />
                    </Field>
                    <Field label="联系邮箱" req>
                      <Input
                        className="mono"
                        value={form.site.contact_email}
                        onChange={(e) => patch((f) => ({ ...f, site: { ...f.site, contact_email: e.target.value } }))}
                      />
                    </Field>
                    <Field label="ICP 备案号（可选）">
                      <Input
                        className="mono"
                        placeholder="如 京ICP备00000000号-1"
                        value={form.site.icp_beian}
                        onChange={(e) => patch((f) => ({ ...f, site: { ...f.site, icp_beian: e.target.value } }))}
                      />
                    </Field>
                  </div>
                  <Field
                    label="API 调用地址"
                    tip="支付回调、产物直链、接口示例都取第一条；只填域名，/v1 由门户自动补全。全部留空则用站点地址"
                  >
                    <div>
                      {form.site.api_base_urls.map((url, i) => (
                        <div className="gr-row gr-row--url" key={i}>
                          <input
                            className="url-input mono"
                            type="url"
                            placeholder="https://api.example.com"
                            aria-label={i === 0 ? "主 API 调用地址" : `备用 API 调用地址 ${i + 1}`}
                            value={url}
                            onChange={(e) =>
                              patch((f) => {
                                const next = f.site.api_base_urls.slice();
                                next[i] = e.target.value;
                                return { ...f, site: { ...f.site, api_base_urls: next } };
                              })
                            }
                          />
                          {i === 0 ? (
                            <span className="badge badge-info">主</span>
                          ) : (
                            <button
                              type="button"
                              className="btn btn-sm"
                              aria-label="上移"
                              onClick={() =>
                                patch((f) => {
                                  const next = f.site.api_base_urls.slice();
                                  [next[i - 1], next[i]] = [next[i], next[i - 1]];
                                  return { ...f, site: { ...f.site, api_base_urls: next } };
                                })
                              }
                            >
                              ↑
                            </button>
                          )}
                          <button
                            type="button"
                            className="btn btn-sm"
                            aria-label="删除"
                            onClick={() =>
                              patch((f) => ({
                                ...f,
                                site: {
                                  ...f.site,
                                  api_base_urls: f.site.api_base_urls.filter((_, k) => k !== i),
                                },
                              }))
                            }
                          >
                            删除
                          </button>
                        </div>
                      ))}
                      <button
                        type="button"
                        className="btn btn-sm"
                        disabled={form.site.api_base_urls.length >= MAX_API_BASES}
                        onClick={() =>
                          patch((f) => ({
                            ...f,
                            site: { ...f.site, api_base_urls: [...f.site.api_base_urls, ""] },
                          }))
                        }
                      >
                        <Icon name="plus" size={14} />
                        添加地址
                      </button>
                    </div>
                  </Field>
                  <Field label="页脚文案（可选，空则显示默认文案）">
                    <textarea
                      rows={2}
                      placeholder="展示于全站页脚，如：灵动 API · 大模型开放平台"
                      value={form.site.footer_text}
                      onChange={(e) => patch((f) => ({ ...f, site: { ...f.site, footer_text: e.target.value } }))}
                    />
                  </Field>
                </Card>

                <Card title="注册与准入" sub="谁能注册、注册时验证什么、管理员是否强制二次验证">
                  <div className="form-grid form-grid--3">
                    <Field label="注册模式" req>
                      <Select
                        items={[
                          ["open", "开放注册"],
                          ["invite_only", "仅限邀请"],
                          ["closed", "关闭注册"],
                        ]}
                        value={form.auth_register.mode}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_register: { ...f.auth_register, mode: e.target.value as "open" | "invite_only" | "closed" },
                          }))
                        }
                      />
                    </Field>
                    <Field label="联系方式验证" req>
                      <Select
                        items={[
                          ["off", "不要求"],
                          ["required", "必须验证"],
                        ]}
                        value={form.auth_register.contact_verify}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_register: { ...f.auth_register, contact_verify: e.target.value as "required" | "off" },
                          }))
                        }
                      />
                    </Field>
                    <Field label="二次验证策略" req>
                      <Select
                        items={[
                          ["off", "关闭"],
                          ["optional", "用户可选"],
                          ["required_for_admin", "强制管理员绑定"],
                        ]}
                        value={form.auth_mfa.policy}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_mfa: { ...f.auth_mfa, policy: e.target.value as AuthMfaSettings["policy"] },
                          }))
                        }
                      />
                    </Field>
                  </div>
                  <Field
                    label="可用联系方式渠道"
                    req
                    tip="只列出已配好发送服务的渠道；多个可用时优先手机号"
                  >
                    <div className="link-row">
                      {(["email", "sms"] as const).map((ch) => {
                        const on = form.auth_register.contact_channels.includes(ch);
                        return (
                          <label key={ch}>
                            <input
                              type="checkbox"
                              checked={on}
                              onChange={() =>
                                patch((f) => ({
                                  ...f,
                                  auth_register: {
                                    ...f.auth_register,
                                    contact_channels: on
                                      ? f.auth_register.contact_channels.filter((v) => v !== ch)
                                      : [...f.auth_register.contact_channels, ch],
                                  },
                                }))
                              }
                            />
                            {" " + CHANNEL_META[ch].shortLabel}
                          </label>
                        );
                      })}
                    </div>
                  </Field>
                  <div className="hint">
                    要求联系方式验证时，所选渠道必须已在「通知」页启用（短信未启用时勾「手机号」保存会被拒绝）；
                    选「强制管理员绑定」要求操作者本人已绑定 TOTP。
                  </div>
                </Card>

                <Card
                  title="站点公告"
                  sub="门户铃铛弹层与首页横幅展示；同 id 编辑不重复提醒，新增 id 才提醒，最多 100 条"
                >
                  <SwitchRow
                    label="启用站点公告"
                    checked={form.announcements.enabled}
                    onChange={(checked) => patch((f) => ({ ...f, announcements: { ...f.announcements, enabled: checked } }))}
                  />
                  {form.announcements.items.length === 0 ? (
                    <div className="muted small">暂无公告，点击下方按钮新增。</div>
                  ) : (
                    <div className="nt-adm-list">
                      {form.announcements.items.map((item, index) => (
                        <div className="nt-adm-row" key={item.id}>
                          <div className="nt-adm-main">
                            <div className="nt-adm-tx">{item.content}</div>
                            <div className="nt-adm-meta">
                              <span className="nt-tag">{ANN_TYPES.find((t) => t[0] === item.type)?.[1] ?? item.type}</span>
                              <time>{item.publishDate}</time>
                              <span className="muted small">id:{item.id}</span>
                            </div>
                          </div>
                          <div className="nt-adm-ops">
                            <button type="button" className="btn btn-sm" onClick={() => openNotice(index)}>
                              编辑
                            </button>
                            <button
                              type="button"
                              className="btn btn-sm btn-ghost-danger"
                              onClick={() => removeNotice(index)}
                            >
                              删除
                            </button>
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                  <button type="button" className="btn btn-sm" onClick={() => openNotice(null)}>
                    <Icon name="plus" size={14} />
                    新增公告
                  </button>
                </Card>

                <Card title="站点内容" sub="关于页与用户协议正文（空行分段）">
                  <Field label="关于页正文" req>
                    <textarea
                      rows={6}
                      value={form.about_content}
                      onChange={(e) => patch((f) => ({ ...f, about_content: e.target.value }))}
                    />
                  </Field>
                  <Field label="用户协议正文" req>
                    <textarea
                      rows={6}
                      value={form.agreement_content}
                      onChange={(e) => patch((f) => ({ ...f, agreement_content: e.target.value }))}
                    />
                  </Field>
                </Card>
              </>
            )}

            {panel === "billing" && (
              <>
                <Card title="计费参数" sub="分组折扣、汇率与充值限制">
                  <Field
                    label="分组折扣"
                    tip="折扣按微比例存储（1e6 = 不打折）。这里按倍数填：0.9 = 九折、1 = 不打折、1.2 = 加价 20%"
                  >
                    <div>
                      {/* `?? {}` 不是防御性冗余：缺这个键时 `Object.entries(undefined)` 会抛
                          「Cannot convert undefined or null to object」，整个配置页被错误边界
                          换成 "Something went wrong!"（其余三处调用点本来就带 `?? {}`）。 */}
                      {Object.entries(form.group_discount ?? {}).map(([name, micro]) => (
                        <div className="gr-row gr-row--disc" key={name}>
                          <Input
                            className="gn"
                            value={name}
                            readOnly={name === "default"}
                            aria-label="分组名"
                            onChange={(e) => {
                              const value = e.target.value;
                              patch((f) => {
                                const next: Record<string, number> = {};
                                for (const [k, v] of Object.entries(f.group_discount ?? {})) {
                                  next[k === name ? value : k] = v;
                                }
                                return { ...f, group_discount: next };
                              });
                            }}
                          />
                          <input
                            className="gv mono"
                            type="number"
                            step="0.05"
                            min="0.01"
                            max="10"
                            value={discountOf(micro)}
                            aria-label="分组折扣"
                            onChange={(e) =>
                              patch((f) => ({
                                ...f,
                                group_discount: {
                                  ...f.group_discount,
                                  // 折扣以微比例落库：0.9 → 900000。四舍五入保证 0.9 存成 900000
                                  // 而不是 899999（后者会让每次扣费少一分钱里的极小一截）。
                                  [name]: Math.round(num(e.target.value) * scale),
                                },
                              }))
                            }
                          />
                          <span className="small muted">{groupDiscountText(micro)}</span>
                          <button
                            type="button"
                            className="btn btn-sm"
                            disabled={name === "default"}
                            onClick={() =>
                              patch((f) => {
                                const next = { ...f.group_discount };
                                delete next[name];
                                return { ...f, group_discount: next };
                              })
                            }
                          >
                            删除
                          </button>
                        </div>
                      ))}
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() =>
                          patch((f) => ({
                            ...f,
                            group_discount: {
                              ...f.group_discount,
                              [`group${Object.keys(f.group_discount ?? {}).length + 1}`]: scale,
                            },
                          }))
                        }
                      >
                        <Icon name="plus" size={14} />
                        新增分组
                      </button>
                    </div>
                  </Field>
                  {(data?.orphan_groups?.length ?? 0) > 0 && (
                    <div className="alert-banner warn">
                      {data!.orphan_groups!.length} 个分组未配置折扣，将按不打折（1.0）计费：
                      {data!.orphan_groups!.join("、")}。请在下方为这些分组补上折扣。
                    </div>
                  )}
                  <div className="form-grid form-grid--4">
                    <Field label="结算币种" tip={`当前符号 ${symNow}。符号由币种决定，不可自定义`}>
                      <Select
                        aria-label="结算币种"
                        value={form.site.currency ?? "CNY"}
                        items={CURRENCY_OPTIONS}
                        onChange={(e) => {
                          const next = e.target.value;
                          patch((f) => ({ ...f, site: { ...f.site, currency: next } }));
                        }}
                      />
                    </Field>
                    <Field label="汇率（1 美元兑人民币）" tip="仅用于「≈ 另一种币种」的参考展示，永不参与扣费">
                      <input
                        type="number"
                        step="0.01"
                        min="0.01"
                        value={form.cny_per_usd}
                        onChange={(e) => patch((f) => ({ ...f, cny_per_usd: num(e.target.value) }))}
                      />
                    </Field>
                    <Field label={`最低充值金额（${symNow}）`} req htmlFor="cfg-mintopup">
                      <MoneyInput
                        id="cfg-mintopup"
                        value={form.min_topup}
                        onChange={(v) => {
                          if (v !== null) patch((f) => ({ ...f, min_topup: v }));
                        }}
                      />
                    </Field>
                    <Field
                      label={`单用户余额上限（${symNow}，0 = 不限）`}
                      htmlFor="cfg-ballimit"
                      tip="充值后余额不得超过它；设为 0 表示不限制"
                    >
                      <MoneyInput
                        id="cfg-ballimit"
                        value={form.balance_limit}
                        onChange={(v) => {
                          if (v !== null) patch((f) => ({ ...f, balance_limit: v }));
                        }}
                      />
                    </Field>
                  </div>
                  {currencyDraftChanged && (
                    <div className="alert-banner warn">
                      切换币种不会重算已存的金额：最低充值仍为 {money2(form.min_topup)}、余额上限{" "}
                      {form.balance_limit > 0 ? money2(form.balance_limit) : "不限"}，
                      但它们的含义从 {symWas} 变成了 {symNow}。请确认这两个数值是否需要一并调整。
                    </div>
                  )}
                  <div className="small muted count-line">
                    充值到账 = 实付金额 + 套餐赠送额；没有倍率、没有刻度，用户看到多少就到账多少。单笔最低{" "}
                    {money2(form.min_topup)}。
                  </div>
                  <SwitchRow
                    label="允许自定义金额充值"
                    tip="关闭后只能选预设套餐。充值到账 = 实付金额 + 套餐赠送额，没有倍率、没有刻度"
                    checked={form.custom_topup_enabled}
                    onChange={(checked) => patch((f) => ({ ...f, custom_topup_enabled: checked }))}
                  />
                </Card>

                <Card
                  title="邀请返利"
                  sub="按好友前 N 笔充值的实付金额分成；奖励转入余额时可转出，冷却期与最低转出门槛都在这一页"
                >
                  <SwitchRow
                    label="启用邀请返利"
                    tip="关闭后停止新的邀请绑定与返利发放；已获得的返利仍可转入余额"
                    checked={form.aff.enabled}
                    onChange={(checked) => patch((f) => ({ ...f, aff: { ...f.aff, enabled: checked } }))}
                  />
                  <div className="form-grid form-grid--3">
                    <Field label="邀请人返利比例（%）" req>
                      <Input
                        type="number"
                        min={0}
                        max={100}
                        step={0.5}
                        value={form.aff.inviter_rate == null ? "" : form.aff.inviter_rate / 10_000}
                        onChange={(e) => {
                          const pct = Number(e.target.value);
                          // 前端就夹紧到 0~100：input 的 min/max 只影响原生校验，
                          // 输入 150 仍会进表单，等到保存才被后端拒绝。
                          const clamped = Number.isFinite(pct) ? Math.min(100, Math.max(0, pct)) : 0;
                          patch((f) => ({
                            ...f,
                            aff: { ...f.aff, inviter_rate: Math.round(clamped * 10_000) },
                          }));
                        }}
                      />
                    </Field>
                    <Field label={`被邀请人每笔奖励（${symNow}）`} req>
                      <MoneyInput
                        value={form.aff.invitee_amount}
                        onChange={(v) => patch((f) => ({ ...f, aff: { ...f.aff, invitee_amount: v } }))}
                      />
                    </Field>
                    <Field label="参与返利的充值笔数" req>
                      <Input
                        type="number"
                        min={1}
                        max={100}
                        step={1}
                        value={form.aff.max_rewards == null ? "" : form.aff.max_rewards}
                        onChange={(e) => {
                          const n = Number(e.target.value);
                          // 与冷却期一致：夹到 1~100，避免清空输入框后提交 0 被后端拒绝
                          const clamped = Number.isFinite(n) ? Math.min(100, Math.max(1, n)) : 1;
                          patch((f) => ({
                            ...f,
                            aff: { ...f.aff, max_rewards: Math.round(clamped) },
                          }));
                        }}
                      />
                    </Field>
                    <Field label="返利冷却期（小时）" req>
                      <Input
                        type="number"
                        min={24}
                        max={720}
                        step={1}
                        value={form.aff.hold_hours == null ? "" : form.aff.hold_hours}
                        onChange={(e) => {
                          const n = Number(e.target.value);
                          // 下限 24：0 等于「签发即到期」，用户能立刻转入余额再申请退款，
                          // 作废逻辑再也追不回来（后端同样拒绝 < 24）。
                          const clamped = Number.isFinite(n) ? Math.min(720, Math.max(24, n)) : 24;
                          patch((f) => ({
                            ...f,
                            aff: { ...f.aff, hold_hours: Math.round(clamped) },
                          }));
                        }}
                      />
                    </Field>
                    <Field label={`奖励最低转出金额（${symNow}）`} req tip="转入可用余额的门槛">
                      <MoneyInput
                        value={form.aff.min_transfer}
                        onChange={(v) => patch((f) => ({ ...f, aff: { ...f.aff, min_transfer: v } }))}
                      />
                    </Field>
                    <Field label="每日邀请上限（人）">
                      <Input
                        type="number"
                        min={1}
                        max={1000}
                        step={1}
                        // 空 = 用后端默认 20；0 不再表示「不限」（那种语义不可观测）。
                        value={form.aff.daily_invite_cap == null ? "" : form.aff.daily_invite_cap}
                        onChange={(e) => {
                          const raw = e.target.value.trim();
                          if (raw === "") {
                            patch((f) => ({ ...f, aff: { ...f.aff, daily_invite_cap: null } }));
                            return;
                          }
                          const n = Number(raw);
                          const clamped = Number.isFinite(n) ? Math.min(1000, Math.max(1, n)) : 20;
                          patch((f) => ({
                            ...f,
                            aff: { ...f.aff, daily_invite_cap: Math.round(clamped) },
                          }));
                        }}
                      />
                    </Field>
                  </div>
                  {/*
                    成本算式：运营唯一能在保存前发现「赠金比充值还高」的地方。
                    刻意不用 tip/hint —— 这两类文案在 Config.tsx 的预算已顶格
                    （check-ui-contracts 的 M19/M21 只许降不许升）。
                  */}
                  <div className={`small mt16 ${affPayoutRisk ? "text-danger" : "muted"}`}>
                    单笔成本 {money2(affPerOrderCost)}（赠金 {money2(form.aff.invitee_amount ?? 0)} ＋ 返利{" "}
                    {ratioPercent(form.aff.inviter_rate ?? 0)} × 实付）；最低充值{" "}
                    {money2(form.min_topup ?? 0)}，最多算 {form.aff.max_rewards ?? 0} 笔。
                    {affPayoutRisk
                      ? ` 单笔成本已不低于最低充值，可被刷号套利，请下调赠金/比例或抬高最低充值。`
                      : ""}
                  </div>
                </Card>
              </>
            )}

            {panel === "pay" && (
              <PayChannelsCard
                channels={form.pay_channels}
                // 已落库的基线：用于「已标记删除 N 条，保存后生效」的对比。
                // 没它的话，删除只改表单态、卡片立刻消失，运营会以为已经删掉了。
                savedChannels={initial?.pay_channels ?? []}
                // 后端未下发 schema（滚动发布连到旧后端）时回落本地兜底；
                // 兜底声明必须与 backend/internal/service/payment/schema.go 同步。
                schemas={data?.pay_channel_schemas ?? FALLBACK_SCHEMAS}
                methodOptions={data?.pay_method_options ?? PAY_METHOD_ITEMS.map((m) => ({ value: m.key, label: m.label }))}
                notifyBase={data?.notify_base ?? ""}
                siteCurrency={form.site.currency || "CNY"}
                mask={data?.mask ?? "******"}
                onChange={(next) => patch((f) => ({ ...f, pay_channels: next }))}
              />
            )}

            {panel === "storage" && (
              <>
                <Card
                  title="云存储"
                  sub="上传与产物的写入位置；只有选中后端的参数生效，其余后端的参数会保留"
                >
                  {storageEff && (
                    <Field
                      label="当前写入"
                      hint={storageEff.writable ? undefined : "未配置存储位置：上传与产物转存会返回 503，不会回落到本地磁盘"}
                      hintWarn={!storageEff.writable}
                    >
                      <Input readOnly className="mono" value={storageEff.writable ? storageEffText : "未配置"} />
                    </Field>
                  )}
                  <Field label="存储渠道">
                    <SegmentedChips
                      items={storageSchemes.map((s) => ({ key: s.id, label: s.name }))}
                      value={storage.active}
                      onChange={(key) =>
                        patch((f) => ({ ...f, storage: { ...f.storage, active: key } }))
                      }
                    />
                  </Field>
                  {storageActive.fields.length > 0 ? (
                    <div className="form-grid">
                      {storageActive.fields.map(([key, label, required]) => (
                        <Field key={key} label={label} req={required}>
                          <input
                            className="mono"
                            type={SECRET_KEYS.has(key) ? "password" : "text"}
                            autoComplete="off"
                            value={storage.channels[storageActive.id]?.[key] ?? ""}
                            onChange={(e) => setChannelParam("storage", storageActive.id, key, e.target.value)}
                          />
                        </Field>
                      ))}
                    </div>
                  ) : (
                    <div className="hint">本地磁盘无需参数：上传与产物落在服务器本地目录，不随实例迁移。</div>
                  )}
                  {/* 探针按钮不放进 Field：`<label for>` 会接管 button 的可访问名
                      （名字变成「连通性」，与可见文案不符）。它是卡片级动作，独立成行。 */}
                  <Field
                    label="产物保留（天）"
                    req
                    tip="任务产物在存储层的保留上限，超期由清理任务删除对象"
                  >
                    <input
                      type="number"
                      min="1"
                      value={form.file_retention_days}
                      onChange={(e) =>
                        patch((f) => ({ ...f, file_retention_days: num(e.target.value) }))
                      }
                    />
                  </Field>
                  <div className="hint">测试直接用当前表单配置，无需先保存。</div>
                  <button
                    type="button"
                    className="btn btn-sm"
                    disabled={storageTest.isPending}
                    onClick={() =>
                      storageTest.mutate(
                        { active: storage.active, channels: storage.channels },
                        {
                          onSuccess: (r) =>
                            r.ok
                              ? toast.success(`${r.backend}：${r.message}`)
                              : toast.error(`${r.backend}：${r.message}`),
                          onError: (error) =>
                            toast.error(error instanceof ApiError ? error.message : "测试失败，请稍后重试"),
                        },
                      )
                    }
                  >
                    <Icon name="refresh" size={14} />
                    测试连通性
                  </button>
                </Card>

                <Card title="文件上传" sub="开发者上传文件的限额与总闸（24h 临时托管，与产物保留无关）；单文件上限按后缀所属类别生效，删掉后缀即禁止该类型上传">
                  <SwitchRow
                    label="开放文件上传接口"
                    tip="关闭后禁止新上传，仅可查看与删除；存量文件仍可访问到自然到期"
                    checked={form.uploads.enabled}
                    onChange={(checked) => patch((f) => ({ ...f, uploads: { ...f.uploads, enabled: checked } }))}
                  />
                  <div className="form-grid form-grid--4">
                    <Field label="每 KEY 日限（次）" req>
                      <input
                        type="number"
                        min="1"
                        max="100000"
                        value={form.uploads.daily_limit_per_key}
                        onChange={(e) =>
                          patch((f) => ({ ...f, uploads: { ...f.uploads, daily_limit_per_key: num(e.target.value) } }))
                        }
                      />
                    </Field>
                    <Field label="有效期（小时，1–72）" req>
                      <input
                        type="number"
                        min="1"
                        max="72"
                        value={form.uploads.retention_hours}
                        onChange={(e) =>
                          patch((f) => ({ ...f, uploads: { ...f.uploads, retention_hours: num(e.target.value) } }))
                        }
                      />
                    </Field>
                    <Field label="每用户在存上限（MB）" req>
                      <input
                        type="number"
                        min="1"
                        max="1048576"
                        value={form.uploads.user_total_mb}
                        onChange={(e) => patch((f) => ({ ...f, uploads: { ...f.uploads, user_total_mb: num(e.target.value) } }))}
                      />
                    </Field>
                    <Field label="并发上传数（超出 429）" req>
                      <input
                        type="number"
                        min="1"
                        value={form.uploads.concurrency}
                        onChange={(e) => patch((f) => ({ ...f, uploads: { ...f.uploads, concurrency: num(e.target.value) } }))}
                      />
                    </Field>
                  </div>

                  <div className="form-grid form-grid--3">
                    {UPLOAD_KIND_LABELS.map(({ key, label }) => (
                      <Field key={key} label={`${label}上限（MB）`} req>
                        <input
                          type="number"
                          min="1"
                          max="10240"
                          value={form.uploads.limits[key] ?? 0}
                          onChange={(e) =>
                            patch((f) => ({
                              ...f,
                              uploads: { ...f.uploads, limits: { ...f.uploads.limits, [key]: num(e.target.value) } },
                            }))
                          }
                        />
                      </Field>
                    ))}
                  </div>

                  {UPLOAD_KIND_LABELS.map(({ key, label }) => {
                    const exts = Object.keys(form.uploads.types ?? {})
                      .filter((e) => form.uploads.types[e] === key)
                      .sort();
                    return (
                      <Field key={key} label={`${label}后缀`}>
                        <div className="gr-row gr-row--up">
                          <input
                            type="text"
                            className="up-ext"
                            placeholder="新增后缀"
                            aria-label={`新增${label}后缀`}
                            value={draftExt[key] ?? ""}
                            onChange={(e) => setDraftExt((m) => ({ ...m, [key]: e.target.value }))}
                            onKeyDown={(e) => {
                              if (e.key === "Enter") {
                                e.preventDefault();
                                addUploadExt(key);
                              }
                            }}
                          />
                          <button type="button" className="btn btn-sm" aria-label={`添加${label}后缀`} onClick={() => addUploadExt(key)}>
                            <Icon name="plus" size={14} />
                          </button>
                        </div>
                        <div className="chips">
                          {exts.map((ext) => (
                            <span className="tag-chip" key={ext}>
                              {ext}
                              <b role="button" tabIndex={0} aria-label={`删除后缀 ${ext}`} onClick={() => removeUploadExt(ext)}>
                                ×
                              </b>
                            </span>
                          ))}
                          {exts.length === 0 && <span className="small muted">暂无</span>}
                        </div>
                      </Field>
                    );
                  })}

                  <SwitchRow
                    label="上传图片压缩（仅本接口，模型参考图与图标不压缩）"
                    checked={form.uploads.compress.enabled}
                    onChange={(checked) =>
                      patch((f) => ({ ...f, uploads: { ...f.uploads, compress: { ...f.uploads.compress, enabled: checked } } }))
                    }
                  />
                  {form.uploads.compress.enabled && (
                    <div className="form-grid form-grid--3">
                      <Field label="压缩质量（1–100）">
                        <input
                          type="number"
                          min="1"
                          max="100"
                          value={form.uploads.compress.quality}
                          onChange={(e) =>
                            patch((f) => ({
                              ...f,
                              uploads: { ...f.uploads, compress: { ...f.uploads.compress, quality: num(e.target.value) } },
                            }))
                          }
                        />
                      </Field>
                      <Field label="输出格式" tip="WebP 降幅最大且保留透明；原格式只把能压小的换掉">
                        <SegmentedChips
                          items={COMPRESS_FORMAT_ITEMS}
                          value={form.uploads.compress.format}
                          onChange={(key) =>
                            patch((f) => ({
                              ...f,
                              uploads: {
                                ...f.uploads,
                                compress: { ...f.uploads.compress, format: key as "original" | "jpeg" | "webp" },
                              },
                            }))
                          }
                        />
                      </Field>
                    </div>
                  )}
                </Card>
              </>
            )}

            {panel === "notify" && (
              <>
                <div className="cfg-grid">
                  <Card title="邮件通知" sub="SMTP 通道与发送场景开关">
                    <SwitchRow
                      label="启用邮件通知"
                      tip="关闭后不发信，「忘记密码」同步关闭"
                      checked={form.email.enabled}
                      onChange={(checked) => patch((f) => ({ ...f, email: { ...f.email, enabled: checked } }))}
                    />
                    <div className="form-grid">
                      <Field label="SMTP 服务器" req span>
                        <Input
                          className="mono"
                          value={form.email.smtp_host}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, smtp_host: e.target.value } }))}
                        />
                      </Field>
                      <Field label="SMTP 端口" req>
                        <input
                          type="number"
                          min="1"
                          max="65535"
                          value={form.email.smtp_port}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, smtp_port: num(e.target.value) } }))}
                        />
                      </Field>
                      <Field label="加密方式">
                        <Select
                          items={EM_ENCRYPTIONS}
                          value={form.email.encryption}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, encryption: e.target.value } }))}
                        />
                      </Field>
                      <Field label="认证用户名" req>
                        <Input
                          className="mono"
                          autoComplete="off"
                          value={form.email.username}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, username: e.target.value } }))}
                        />
                      </Field>
                      <Field label="认证密码" req>
                        <input
                          className="mono"
                          type="password"
                          autoComplete="off"
                          value={form.email.password}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, password: e.target.value } }))}
                        />
                      </Field>
                      <Field label="发件人地址" req>
                        <Input
                          className="mono"
                          value={form.email.from_addr}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, from_addr: e.target.value } }))}
                        />
                      </Field>
                      <Field label="发件人显示名">
                        <Input
                          value={form.email.sender_name}
                          onChange={(e) => patch((f) => ({ ...f, email: { ...f.email, sender_name: e.target.value } }))}
                        />
                      </Field>
                    </div>
                    {EM_TOGGLES.map((s) => (
                      <SwitchRow
                        key={s.key}
                        label={s.label}
                        tip={s.tip}
                        checked={form.email.scenes[s.key] ?? false}
                        onChange={(checked) =>
                          patch((f) => ({
                            ...f,
                            email: { ...f.email, scenes: { ...f.email.scenes, [s.key]: checked } },
                          }))
                        }
                      />
                    ))}
                    <Field label="测试收件邮箱" tip="先保存再测试">
                      <div className="code-row">
                        <Input
                          className="mono"
                          placeholder="test@example.com"
                          value={emailTarget}
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setEmailTarget(e.target.value)}
                        />
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={notifyTest.isPending}
                          onClick={() =>
                            notifyTest.mutate(
                              { channel: "email", target: emailTarget.trim() },
                              { onSuccess: () => toast.success(`测试邮件已发送至 ${emailTarget.trim()}`) },
                            )
                          }
                        >
                          {sendCodeText(0, notifyTest.isPending && notifyTest.variables?.channel === "email", "测试发送")}
                        </button>
                      </div>
                    </Field>
                  </Card>

                  <Card title="短信通知" sub="仅用于系统告警与绑定校验码">
                    <SwitchRow
                      label="启用短信通知"
                      checked={sms.enabled}
                      onChange={(checked) => patch((f) => ({ ...f, sms: { ...f.sms, enabled: checked } }))}
                    />
                    {!sms.enabled && (
                      <div className="hint">
                        当前未启用：注册与找回不会发送短信，勾「手机号」作为验证渠道会被拒绝。
                      </div>
                    )}
                    <Field label="短信渠道">
                      <SegmentedChips
                        items={SMS_SCHEMES.map((s) => ({ key: s.id, label: s.name }))}
                        value={sms.active}
                        onChange={(key) => patch((f) => ({ ...f, sms: { ...f.sms, active: key } }))}
                      />
                    </Field>
                    <div className="form-grid">
                      {smsActive.fields.map(([key, label, required]) => (
                        <Field key={key} label={label} req={required}>
                          <input
                            className="mono"
                            type={SECRET_KEYS.has(key) ? "password" : "text"}
                            autoComplete="off"
                            value={sms.channels[smsActive.id]?.[key] ?? ""}
                            onChange={(e) => setChannelParam("sms", smsActive.id, key, e.target.value)}
                          />
                        </Field>
                      ))}
                    </div>
                    <Field label="每日短信限额（条）" tip="0 表示不限量">
                      <input
                        type="number"
                        min="0"
                        value={sms.daily_limit}
                        onChange={(e) => patch((f) => ({ ...f, sms: { ...f.sms, daily_limit: num(e.target.value) } }))}
                      />
                    </Field>
                    <Field label="测试接收手机号" tip="先保存再测试">
                      <div className="code-row">
                        <Input
                          className="mono"
                          placeholder="13800000000"
                          value={smsTarget}
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSmsTarget(e.target.value)}
                        />
                        <button
                          type="button"
                          className="btn btn-sm"
                          disabled={notifyTest.isPending}
                          onClick={() =>
                            notifyTest.mutate(
                              { channel: "sms", target: smsTarget.trim() },
                              { onSuccess: () => toast.success(`测试短信已发送至 ${smsTarget.trim()}`) },
                            )
                          }
                        >
                          {sendCodeText(0, notifyTest.isPending && notifyTest.variables?.channel === "sms", "测试发送")}
                        </button>
                      </div>
                    </Field>
                  </Card>
                </div>

                <Card title="邮件模板" sub="变量在发送时自动填充；4 个场景必须齐备，缺失会被拦截">
                  <Tabs
                    variant="pill"
                    items={EM_SCENE_TABS}
                    value={emailScene.tab}
                    onChange={(k) => emailScene.setTab(k as EmailScene)}
                    idPrefix="admin-config-em-scene"
                    label="邮件模板场景"
                  />
                  {(() => {
                    const scene = EM_SCENES.find((s) => s.key === emailScene.tab) ?? EM_SCENES[0];
                    const tpl = form.email.templates[emailScene.tab] ?? EM_TPL_DEFAULTS[emailScene.tab];
                    return (
                      <>
                        <Field label="邮件主题" req tip={`场景：${scene.label}；${scene.tip}`}>
                          <Input
                            value={tpl.subject}
                            onChange={(e) =>
                              patch((f) => ({
                                ...f,
                                email: {
                                  ...f.email,
                                  templates: {
                                    ...f.email.templates,
                                    [emailScene.tab]: { ...tpl, subject: e.target.value },
                                  },
                                },
                              }))
                            }
                          />
                        </Field>
                        <Field label="邮件正文" req tip="{{site_name}} / {{code}} 发送时替换">
                          <textarea
                            rows={6}
                            value={tpl.body}
                            onChange={(e) =>
                              patch((f) => ({
                                ...f,
                                email: {
                                  ...f.email,
                                  templates: {
                                    ...f.email.templates,
                                    [emailScene.tab]: { ...tpl, body: e.target.value },
                                  },
                                },
                              }))
                            }
                          />
                        </Field>
                        <button
                          type="button"
                          className="btn btn-sm"
                          onClick={() =>
                            patch((f) => ({
                              ...f,
                              email: {
                                ...f.email,
                                templates: { ...f.email.templates, [emailScene.tab]: { ...EM_TPL_DEFAULTS[emailScene.tab] } },
                              },
                            }))
                          }
                        >
                          恢复默认模板
                        </button>
                      </>
                    );
                  })()}
                </Card>
              </>
            )}

            {panel === "models" && (
              <>
                <Card
                  title="各类型默认模型"
                  sub="用户未指定模型时的兜底；受限类型要求密钥白名单包含默认模型"
                >
                  <div className="default-grid">
                    {MODALITY_CARDS.map((m) => {
                      const options = modelOptions.get(m.key) ?? [];
                      const current = form.modality_defaults[m.key] ?? "";
                      const missing = current !== "" && !options.some((o) => o.id === current);
                      return (
                        <div className="dcard" key={m.key}>
                          <div className="dc-label">{m.label}</div>
                          <EntitySelect
                            // 允许留空 = 自动选择（后端派生时回落到该模态首个在售模型）。
                            // 少了这一项，运营想把某类交回自动就无路可走，只能填一个具体模型。
                            allLabel="自动选择"
                            ariaLabel={`${m.label}默认模型`}
                            options={[
                              // 当前值已停用时也要能显示出来：直接消失会让人以为"配置丢了"，
                              // 而保存时那个值其实还在。
                              ...(missing
                                ? [{ value: current, label: `${current}（当前，未启用）` }]
                                : []),
                              ...options.map((o) => ({ value: o.id, label: o.label, iconUrl: o.iconUrl })),
                            ]}
                            value={current}
                            onChange={(v) =>
                              patch((f) => ({
                                ...f,
                                modality_defaults: { ...f.modality_defaults, [m.key]: v },
                              }))
                            }
                          />
                        </div>
                      );
                    })}
                  </div>
                  <div className="hint">
                    「自动选择」= 跟随该类型下第一个在售模型；留空不会被当作配置错误。
                  </div>
                </Card>

                <Card title="路由与限流" sub="失败重试与默认限流兜底">
                  <div className="form-grid form-grid--4">
                    <Field label="失败自动重试次数" tip="仅网络或渠道错误会重试，业务报错不重试">
                      <input
                        type="number"
                        min="0"
                        max="10"
                        value={form.retry_times}
                        onChange={(e) => patch((f) => ({ ...f, retry_times: num(e.target.value) }))}
                      />
                    </Field>
                    <Field label="默认限流：每分钟请求数" req>
                      <input
                        type="number"
                        min="1"
                        value={form.rate_limit_defaults.rpm}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            rate_limit_defaults: { ...f.rate_limit_defaults, rpm: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field label="默认限流：并发数" req>
                      <input
                        type="number"
                        min="1"
                        value={form.rate_limit_defaults.concurrency}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            rate_limit_defaults: { ...f.rate_limit_defaults, concurrency: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field label="任务查询截止（分钟）">
                      <input
                        type="number"
                        min="5"
                        value={form.poll_deadline_min}
                        onChange={(e) => patch((f) => ({ ...f, poll_deadline_min: num(e.target.value) }))}
                      />
                    </Field>
                  </div>
                </Card>

                <Card
                  title="渠道监控"
                  sub="开启自动禁用的渠道的全局熔断参数；渠道级开关在渠道管理页"
                >
                  <div className="form-grid form-grid--3">
                    <Field label="失败率阈值" req tip="0–1 之间的小数，达到即触发自动禁用">
                      <input
                        type="number"
                        min="0.01"
                        max="1"
                        step="0.01"
                        value={form.monitor.fail_threshold}
                        onChange={(e) => patch((f) => ({ ...f, monitor: { ...f.monitor, fail_threshold: num(e.target.value) } }))}
                      />
                    </Field>
                    <Field label="统计窗口（分钟）" req>
                      <input
                        type="number"
                        min="1"
                        value={form.monitor.fail_window}
                        onChange={(e) => patch((f) => ({ ...f, monitor: { ...f.monitor, fail_window: num(e.target.value) } }))}
                      />
                    </Field>
                    <Field label="探活间隔（分钟）" req>
                      <input
                        type="number"
                        min="1"
                        value={form.monitor.probe_interval_min}
                        onChange={(e) =>
                          patch((f) => ({ ...f, monitor: { ...f.monitor, probe_interval_min: num(e.target.value) } }))
                        }
                      />
                    </Field>
                  </div>
                  <SwitchRow
                    label="失败率超限自动禁用渠道"
                    checked={form.monitor.auto_disable}
                    onChange={(checked) => patch((f) => ({ ...f, monitor: { ...f.monitor, auto_disable: checked } }))}
                  />
                  <SwitchRow
                    label="探活通过自动恢复渠道"
                    checked={form.monitor.auto_enable}
                    onChange={(checked) => patch((f) => ({ ...f, monitor: { ...f.monitor, auto_enable: checked } }))}
                  />
                </Card>
              </>
            )}

            {panel === "security" && (
              <>
                <Card
                  title="登录防护"
                  sub="图形验证码（5 位数字）；达阈值后要求验证码 + 随机退避，只有单 IP 维度会直接 429"
                >
                  <div className="form-grid form-grid--3">
                    <Field label="登录场景" req>
                      <Select
                        items={[
                          ["always", "每次都要"],
                          ["after_fails", "失败后要"],
                          ["off", "从不"],
                        ]}
                        value={form.auth_captcha.scenes.login}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_captcha: {
                              ...f.auth_captcha,
                              scenes: { login: e.target.value as "always" | "after_fails" | "off" },
                            },
                          }))
                        }
                      />
                    </Field>
                    <Field label="转验证码阈值（次）" req>
                      <input
                        type="number"
                        min="3"
                        max="20"
                        value={form.auth_login.fail_threshold}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_login: { ...f.auth_login, fail_threshold: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field label="服务端退避上限（毫秒）" req>
                      <input
                        type="number"
                        min="0"
                        max="3000"
                        value={form.auth_login.delay_max_ms}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_login: { ...f.auth_login, delay_max_ms: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field label="单 IP 失败上限（次）" req>
                      <input
                        type="number"
                        min="5"
                        max="200"
                        value={form.auth_login.ip_fail_limit}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_login: { ...f.auth_login, ip_fail_limit: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field label="单 IP 统计窗口（秒）" req>
                      <input
                        type="number"
                        min="60"
                        max="86400"
                        value={form.auth_login.ip_fail_window_sec}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_login: { ...f.auth_login, ip_fail_window_sec: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field
                      label="失败计数阶梯（秒）"
                      req
                      tip="逗号分隔、必须递增；第 n 次失败套用第 n 档，超出取末档。例如 60,300,900"
                    >
                      <input
                        type="text"
                        className="mono"
                        value={form.auth_login.backoff_seconds.join(",")}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_login: {
                              ...f.auth_login,
                              backoff_seconds: e.target.value
                                .split(",")
                                .map((v) => num(v.trim()))
                                .filter((v) => v > 0),
                            },
                          }))
                        }
                      />
                    </Field>
                  </div>
                  <div className="hint">
                    注册与找回密码固定每次都要验证码；刻意没有「锁定 X 分钟」——账号维度的硬锁等于给攻击者一个锁死任意账号的开关。
                  </div>
                </Card>

                <Card title="会话" sub="登录态生命周期与单用户设备数">
                  <div className="form-grid form-grid--4">
                    <Field label="会话绝对有效期（天）" req>
                      <input
                        type="number"
                        min="1"
                        value={Math.round(form.auth_session.absolute_ttl_sec / 86400)}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_session: { ...f.auth_session, absolute_ttl_sec: num(e.target.value) * 86400 },
                          }))
                        }
                      />
                    </Field>
                    <Field label="空闲超时（小时）" req>
                      <input
                        type="number"
                        min="1"
                        value={Math.round(form.auth_session.idle_ttl_sec / 3600)}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_session: { ...f.auth_session, idle_ttl_sec: num(e.target.value) * 3600 },
                          }))
                        }
                      />
                    </Field>
                    <Field label="单用户活跃会话上限" req>
                      <input
                        type="number"
                        min="1"
                        max="50"
                        value={form.auth_session.max_active}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_session: { ...f.auth_session, max_active: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                    <Field label="活跃时间写入间隔（秒）" req tip="0 = 每个请求都写（不推荐）">
                      <input
                        type="number"
                        min="0"
                        max="3600"
                        value={form.auth_session.touch_interval_sec}
                        onChange={(e) =>
                          patch((f) => ({
                            ...f,
                            auth_session: { ...f.auth_session, touch_interval_sec: num(e.target.value) },
                          }))
                        }
                      />
                    </Field>
                  </div>
                </Card>

                <Card
                  className="card--danger"
                  title="数据保留"
                  sub="日志类数据的留存周期；缩短会立刻缩小可查询范围且不可恢复"
                >
                  <div className="form-grid form-grid--4">
                    <Field label="调用日志保留（天）" req>
                      <input
                        type="number"
                        min="30"
                        value={form.logs_retention_days}
                        onChange={(e) => patch((f) => ({ ...f, logs_retention_days: num(e.target.value) }))}
                      />
                    </Field>
                    <Field label="归档保留（天）" req>
                      <input
                        type="number"
                        min="60"
                        value={form.archive_retention_days}
                        onChange={(e) => patch((f) => ({ ...f, archive_retention_days: num(e.target.value) }))}
                      />
                    </Field>
                    <Field label="审计日志保留（天）" req>
                      <input
                        type="number"
                        min="30"
                        value={form.audit_retention_days}
                        onChange={(e) => patch((f) => ({ ...f, audit_retention_days: num(e.target.value) }))}
                      />
                    </Field>
                    <Field label="认证事件保留（天）" req>
                      <input
                        type="number"
                        min="1"
                        value={form.auth_event_retention_days}
                        onChange={(e) =>
                          patch((f) => ({ ...f, auth_event_retention_days: num(e.target.value) }))
                        }
                      />
                    </Field>
                  </div>
                  <div className="hint warn">
                    缩短保留期前请先导出审计记录；清理任务执行后无法恢复。
                  </div>
                </Card>
              </>
            )}
          </>
        )}
      </PageTabs>

      <Dialog
        open={noticeDlg !== null}
        onClose={() => setNoticeDlg(null)}
        title={noticeDlg?.index === null ? "新增公告" : "编辑公告"}
        footer={
          <>
            <button type="button" className="btn" onClick={() => setNoticeDlg(null)}>
              取消
            </button>
            <button type="button" className="btn btn-primary" onClick={saveNotice}>
              保存
            </button>
          </>
        }
      >
        {noticeDlg ? (
          <>
            <Field label="公告内容" req tip="≤500 字，空行分段">
              <textarea
                rows={4}
                maxLength={500}
                value={noticeDlg.draft.content}
                onChange={(e) => setNoticeDlg({ ...noticeDlg, draft: { ...noticeDlg.draft, content: e.target.value } })}
              />
            </Field>
            <div className="form-grid">
              <Field label="公告类型" req>
                <Select
                  items={ANN_TYPES}
                  value={noticeDlg.draft.type}
                  onChange={(e) =>
                    setNoticeDlg({
                      ...noticeDlg,
                      draft: { ...noticeDlg.draft, type: e.target.value as AnnouncementType },
                    })
                  }
                />
              </Field>
              <Field label="发布时间" req tip="RFC3339（如 2026-10-15T10:00:00+08:00）">
                <Input
                  className="mono"
                  value={noticeDlg.draft.publishDate}
                  onChange={(e) =>
                    setNoticeDlg({ ...noticeDlg, draft: { ...noticeDlg.draft, publishDate: e.target.value } })
                  }
                />
              </Field>
            </div>
            <Field label="附加信息（可选）" tip="≤100 字">
              <Input
                maxLength={100}
                value={noticeDlg.draft.extra}
                onChange={(e) => setNoticeDlg({ ...noticeDlg, draft: { ...noticeDlg.draft, extra: e.target.value } })}
              />
            </Field>
          </>
        ) : null}
      </Dialog>
    </>
  );
}
