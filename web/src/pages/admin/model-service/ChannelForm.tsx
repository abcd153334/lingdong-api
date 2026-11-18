import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";

import { ChannelKeysSection } from "@/components/admin/ChannelKeysSection";
import { Drawer } from "@/components/common/Drawer";
import { Field } from "@/components/common/Field";
import { IconNameField } from "@/components/common/IconNameField";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import {
  useCreateAdminChannelMutation,
  useUpdateAdminChannelMutation,
} from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import {
  ADAPTER_PRESETS,
  EMPTY_ADAPTER_FORM,
  buildConfig,
  configToForm,
  type AdapterConfigForm,
} from "@/lib/adapterConfig";
import {
  AUTODL_MODE_LABELS,
  AUTODL_MODE_ORDER,
  AUTODL_MODE_TIPS,
  AUTODL_RESOLUTION_ITEMS,
  AUTODL_TIERS,
  AUTODL_WORKFLOW_DEFAULTS,
  autodlOptionsToForm,
  buildAutoDLOptions,
  emptyAutoDLForm,
  type AutoDLMode,
  type AutoDLOptionsForm,
  type AutoDLTier,
} from "@/lib/autodlOptions";
import { firstBlockingIssue, parseKeyLines, type KeyLineIssue } from "@/lib/channelKeys";
import {
  DUOMI_AUTH_ITEMS,
  DUOMI_DEFAULT_AUTH_PREFIX,
  DUOMI_DEFAULT_PATHS,
  DUOMI_MAX_OPTIONS_BYTES,
  DUOMI_MAX_SUNO_MV_LEN,
  DUOMI_METADATA_ITEMS,
  DUOMI_PATH_ITEMS,
  DUOMI_TRIBOOL_ITEMS,
  buildDuomiOptions,
  duomiKlingPrefixHint,
  duomiOptionsToForm,
  duomiOverridesHint,
  emptyDuomiForm,
  type DuomiAuthMode,
  type DuomiMetadataKey,
  type DuomiOptionsForm,
  type DuomiPathKey,
  type TriBool,
} from "@/lib/duomiOptions";
import { formatDateTime } from "@/lib/format";
import {
  METASO_CONTEXT_IR_ITEMS,
  METASO_DEFAULT_QUERY_PATH,
  METASO_MAX_QUERY_PATH_LEN,
  METASO_RESOLUTION_ITEMS,
  METASO_TIERS,
  buildMetasoOptions,
  emptyMetasoForm,
  metasoOptionsToForm,
  type MetasoContextIR,
  type MetasoTier,
  type MetasoOptionsForm,
} from "@/lib/metasoOptions";
import type { AdapterInfo, ChannelItem, ChannelPayload, MultiKeyMode } from "@/types";

const PREFIX_TIP = '默认 "Bearer "（尾随空格由前端自动补齐）；填 none 表示不加前缀（x-api-key 场景）';
const PATHS_TIP = '键取标准端点（如 /v1/chat/completions），值以 / 开头、可带 query。注意 /v1/models 同时用于连通测试与健康探测，覆盖后务必确保新路径可用';
const PATCH_TIP = '点分路径 → 值；null 表示删除该键；"$model" 引用上游模型名。不能改 model/stream/messages，不支持数组定位，仅 JSON 端点生效';
const RESP_TIP = '仅非流式 chat 生效。content 必填（如 data.answer）；配了 token 才按上游 usage 计费，否则按估算';

const MODE_ITEMS = [
  { key: "polling", label: "轮询" },
  { key: "random", label: "随机" },
];/** 渠道编辑器表单态（数值以字符串承载，提交时归一） */
interface ChannelForm {
  name: string;
  /** 图标 token（空串 = 首字占位）。与模型/供应商同一个选择器、同一个字段语义。 */
  icon: string;
  /**
   * `icon` 对应的可渲染地址（来自渠道响应）。
   *
   * 表单是受控的：没有这一列，编辑已有渠道时图标砖只能显示首字 ——
   * 令牌本身不可渲染（地址规则只在后端）。用户新选的图标由选择器内部记地址，
   * 所以这里只需装「打开表单那一刻服务端给的那张」。
   */
  iconUrl: string;
  adapterId: string;
  baseUrl: string;
  keysText: string;
  multiKeyMode: MultiKeyMode;
  /** 失败时换同渠道另一把密钥重试（默认关 = 与改造前一致） */
  switchKeyRetry: boolean;
  groups: string[];
  autoBan: boolean;
  rpm: string;
  cc: string;
  weight: string;
  priority: string;
  /** 声明式适配配置（三个 JSON 字段以文本承载，提交前统一校验） */
  decl: AdapterConfigForm;
  /**
   * 适配器私有配置（仅 supports_options 的适配器使用）。
   *
   * 每个适配器**各持一份**表单态，而不是共用一个联合字段：切换适配器时两边的值
   * 互不影响，提交时只取当前 adapter_id 对应的那一份。共用字段会让「从 AutoDL
   * 切到 Metaso，保存的是 AutoDL 的默认文档」这种脏写发生。
   */
  opts: AutoDLOptionsForm;
  metaso: MetasoOptionsForm;
  /** 多米API：四块（鉴权前缀 / 路径 / 透传白名单 / 模型覆盖），同样与另外两份互不串写 */
  duomi: DuomiOptionsForm;
}

const emptyForm = (mode: MultiKeyMode = "random"): ChannelForm => ({
  name: "",
  icon: "",
  iconUrl: "",
  adapterId: "openai",
  baseUrl: "",
  keysText: "",
  multiKeyMode: mode,
  switchKeyRetry: false,
  groups: ["default"],
  autoBan: true,
  rpm: "60",
  cc: "10",
  weight: "5",
  priority: "1",
  decl: { ...EMPTY_ADAPTER_FORM },
  opts: emptyAutoDLForm(),
  metaso: emptyMetasoForm(),
  duomi: emptyDuomiForm(),
});

const formFromChannel = (ch: ChannelItem): ChannelForm => ({
  name: ch.name,
  // 缺字段兜底成 ""（桩件/旧响应）：undefined 会让 IconNameField 的受控值跳变，
  // 而 `undefined` 提交上去是「不修改」，等于每次保存都悄悄保留旧图标。
  icon: ch.icon ?? "",
  iconUrl: ch.icon_url ?? "",
  adapterId: ch.adapter_id,
  baseUrl: ch.base_url,
  keysText: "",
  multiKeyMode: ch.multi_key_mode,
  switchKeyRetry: ch.switch_key_retry ?? false,
  groups: [...ch.groups],
  autoBan: ch.auto_ban,
  rpm: String(ch.rpm_limit),
  cc: String(ch.max_concurrency),
  weight: String(ch.weight),
  priority: String(ch.priority),
  // 必须兜底：旧接口/桩件的响应里可能没有该字段
  decl: configToForm(ch.adapter_config),
  // 同上：未配置（null）时回填内置默认，运营能看到真实生效的工作流 ID
  opts: autodlOptionsToForm(ch.adapter_options),
  // 两份回填都跑：读不出自己那份键时落各自默认，只有当前适配器那份会被提交
  metaso: metasoOptionsToForm(ch.adapter_options),
  // 第三份同理（多米四块配置与 Metaso/AutoDL 的键完全不重叠，互不影响）
  duomi: duomiOptionsToForm(ch.adapter_options),
});
/**
 * 渠道新建 / 编辑抽屉（模型服务的「新建渠道」与渠道条上的「编辑」共用）。
 *
 * 与旧渠道页的区别只有两点，都是"合并成一个页面"带来的必然结果：
 *  1. 适配器由**左栏选中项**预填（`defaultAdapterId`），不再是页面顶部下拉选一遍；
 *  2. 只读的"模型绑定"块删除 —— 右栏就在抽屉后面显示同一份数据，
 *     同一事实两处渲染迟早会不一致。
 *
 * 表单态在 `open` 由 false → true 时按 `channel` 重建，之后列表重取
 * （改密钥后失效）不会覆盖正在编辑的草稿。
 */
export function ChannelForm({
  open,
  channel,
  channels,
  adapters,
  groups,
  defaultAdapterId,
  onClose,
  onCreated,
}: {
  open: boolean;
  /** null = 新建 */
  channel: ChannelItem | null;
  /** 当前渠道列表（编辑态取实时密钥数与余额） */
  channels: ChannelItem[];
  adapters: AdapterInfo[];
  groups: string[];
  /** 新建时的预选适配器（从左栏某个适配器下点「+」进来） */
  defaultAdapterId?: string;
  onClose: () => void;
  /**
   * 新建成功后的回调，参数是**服务端返回的整行**。
   *
   * 必须把 item 交出去：页面要立刻 `?c=<新 id>` 落到它上面，否则用户看到的是
   * "保存成功，右边还是空的"。id 只能从响应里拿 —— 客户端不猜主键。
   */
  onCreated?: (ch: ChannelItem) => void;
}) {
  const [form, setForm] = useState<ChannelForm>(() => emptyForm());

  // 打开时按目标重建表单。用 ref 记住"这次打开是为谁"，而不是把 channel 放进依赖：
  // 列表失效重取会换掉对象引用，依赖它会**在编辑中途把草稿重置**。
  const openedFor = useRef("");
  useEffect(() => {
    if (!open) {
      openedFor.current = "";
      return;
    }
    const key = channel ? `e:${channel.id}` : `n:${defaultAdapterId ?? ""}`;
    if (openedFor.current === key) return;
    openedFor.current = key;
    setForm(
      channel
        ? formFromChannel(channel)
        : { ...emptyForm(), adapterId: defaultAdapterId || "openai" },
    );
  }, [open, channel, defaultAdapterId]);

  const createMut = useCreateAdminChannelMutation();
  const updateMut = useUpdateAdminChannelMutation();
  const isPending = createMut.isPending || updateMut.isPending;

  const setField = <K extends keyof ChannelForm>(key: K, value: ChannelForm[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  /**
   * 切换适配器时预填 Base URL。
   *
   * 只在「当前为空」或「当前仍是上一个适配器的默认值」时替换：运营手填过的地址
   * 永远不覆盖 —— 覆盖等于一次保存就把线上渠道打到别的域名。
   *
   * 为什么必须预填：有些适配器的路径前缀挂在 base_url 上（方舟的数据面在 `/api/v3`，
   * 而平台 canonical path 是 `/v1/...`）。少写一段的表现是**全部请求 404**，
   * 而管理台不会报任何错，只能靠一个个模型试出来。
   */
  const onAdapterChange = (nextId: string) => {
    const prevDefault = adapters.find((a) => a.id === form.adapterId)?.default_base_url ?? "";
    const nextDefault = adapters.find((a) => a.id === nextId)?.default_base_url ?? "";
    setForm((f) => {
      const cur = f.baseUrl.trim();
      const replace = cur === "" || cur === prevDefault;
      return { ...f, adapterId: nextId, baseUrl: replace ? nextDefault : f.baseUrl };
    });
  };

  const setDecl = (key: keyof AdapterConfigForm, value: string) =>
    setForm((f) => ({ ...f, decl: { ...f.decl, [key]: value } }));

  // 嵌套对象必须逐层展开：直接把 f.opts.workflows 改掉会让 setForm 认为状态未变
  const setWorkflow = (mode: AutoDLMode, value: string) =>
    setForm((f) => ({
      ...f,
      opts: { ...f.opts, workflows: { ...f.opts.workflows, [mode]: value } },
    }));

  const setResolution = (tier: AutoDLTier, value: string) =>
    setForm((f) => ({
      ...f,
      opts: { ...f.opts, resolution: { ...f.opts.resolution, [tier]: value } },
    }));

  // Metaso 三个 setter 同理逐层展开（嵌套对象直接改不会触发重渲染）
  const setMetasoTier = (tier: MetasoTier, value: string) =>
    setForm((f) => ({
      ...f,
      metaso: { ...f.metaso, resolution: { ...f.metaso.resolution, [tier]: value } },
    }));

  const setMetasoContextIR = (v: MetasoContextIR) =>
    setForm((f) => ({ ...f, metaso: { ...f.metaso, contextIR: v } }));

  const setMetasoQueryPath = (v: string) =>
    setForm((f) => ({ ...f, metaso: { ...f.metaso, queryPath: v } }));

  // 多米四个 setter 同样逐层展开（paths 是嵌套对象，直接改不会触发重渲染）
  const setDuomi = <K extends keyof DuomiOptionsForm>(key: K, value: DuomiOptionsForm[K]) =>
    setForm((f) => ({ ...f, duomi: { ...f.duomi, [key]: value } }));

  const setDuomiPath = (key: DuomiPathKey, value: string) =>
    setForm((f) => ({
      ...f,
      duomi: { ...f.duomi, paths: { ...f.duomi.paths, [key]: value } },
    }));

  /** 白名单勾选：勾上追加到末尾，取消则移除；顺序由 buildDuomiOptions 归一 */
  const toggleDuomiMetadata = (key: DuomiMetadataKey, on: boolean) =>
    setForm((f) => ({
      ...f,
      duomi: {
        ...f.duomi,
        metadataAllow: on
          ? [...f.duomi.metadataAllow.filter((k) => k !== key), key]
          : f.duomi.metadataAllow.filter((k) => k !== key),
      },
    }));

  // 实时校验（纯函数）：错误就地显示在对应字段下，保存时再兜一道。
  const decl = useMemo(() => buildConfig(form.decl), [form.decl]);
  const optsBuild = useMemo(() => buildAutoDLOptions(form.opts), [form.opts]);
  const metasoBuild = useMemo(() => buildMetasoOptions(form.metaso), [form.metaso]);
  const duomiBuild = useMemo(() => buildDuomiOptions(form.duomi), [form.duomi]);
  /**
   * 覆盖表的逐条错误合并成一行交给 `Field.hint`。
   *
   * 只给一句「配置不合法」运营无从下手（覆盖表是手写 JSON，错在哪一行必须说清）；
   * 但也不能每条渲染一个常驻小字 —— UI 契约门禁对 `div.hint` 是按文件冻结基线的，
   * 而 `Field.hint` 那条路径不计入。位置信息用 `模型ID.字段` 前缀表达。
   */
  const overridesErrorText = useMemo(() => {
    const parts: string[] = [];
    for (const [k, v] of Object.entries(duomiBuild.errors)) {
      if (!k.startsWith("model_overrides")) continue;
      parts.push(k === "model_overrides" ? v : `${k.slice("model_overrides.".length)}：${v}`);
    }
    return parts.join("；");
  }, [duomiBuild]);

  const adapterMeta = adapters.find((a) => a.id === form.adapterId);
  // 桩件/旧接口可能不下发该字段：按前缀兜底（规格渠道的鉴权与路径由规格 JSON 管理）。
  const declDisabled = !(adapterMeta?.supports_declarative ?? !form.adapterId.startsWith("spec:"));
  // 私有配置**不做兜底推断**：形状由适配器定义，猜错就是"配了没人读"。
  const optionsEnabled = adapterMeta?.supports_options === true;
  /**
   * 管理台是否**认识**这个适配器的私有配置形状。
   *
   * `supports_options` 只说「后端接受配置」，不说「前端知道怎么填」。两者必须分开判：
   * 未知形状时既不能渲染 AutoDL 表单（会写入完全无关的文档），也不能下发默认文档
   * （等于用 AutoDL 的默认值覆盖别人的配置）。此时保持「不传 = 不修改」。
   */
  const optionsKnown =
    form.adapterId === "autodl" || form.adapterId === "metaso" || form.adapterId === "duomiapi";
  // 当前适配器该用哪一份：未知形状时两者皆空（不渲染、不校验、不下发）
  const optionsForm = optionsEnabled && optionsKnown ? form.adapterId : "";
  const optionsErrors: Record<string, string> =
    optionsForm === "metaso"
      ? (metasoBuild.errors as Record<string, string>)
      : optionsForm === "duomiapi"
        ? (duomiBuild.errors as Record<string, string>)
        : optionsForm === "autodl"
          ? (optsBuild.errors as Record<string, string>)
          : {};
  const optionsValue: Record<string, unknown> | null =
    optionsForm === "metaso"
      ? metasoBuild.value
      : optionsForm === "duomiapi"
        ? duomiBuild.value
        : optionsForm === "autodl"
          ? optsBuild.value
          : null;

  // 下拉文案用中文名（规格是 `spec:<slug>`，直接显示等于让运营读标识符）；
  // 值永远是 id，回填与保存都只认 id。
  const adapterOptions: [string, string][] = adapters.map((a) => [a.id, a.label || a.id]);
  if (channel && !adapters.some((a) => a.id === channel.adapter_id)) {
    adapterOptions.unshift([channel.adapter_id, channel.adapter_id]);
  }

  /** 编辑态密钥池的渠道状态：取列表的实时行，停用/启用会改变"最后一把"的守卫条件。 */
  const liveChannel = channel ? (channels.find((c) => c.id === channel.id) ?? channel) : null;

  /** 新建态的实时解析（供「保存（新增 N 把）」与行级报错共用）。 */
  const parsedKeys = channel ? null : parseKeyLines(form.keysText);
  const keysIssue = parsedKeys ? firstBlockingIssue(parsedKeys.issues) : null;

  const toggleGroup = (g: string) =>
    setForm((f) => ({
      ...f,
      groups: f.groups.includes(g) ? f.groups.filter((x) => x !== g) : [...f.groups, g],
    }));

  const submitForm = () => {
    const name = form.name.trim();
    if (!name) return toast.error("请填写渠道名称");
    if (!form.adapterId.trim()) return toast.error("请选择适配器");
    if (!/^https?:\/\//.test(form.baseUrl.trim()))
      return toast.error("Base URL 需以 http:// 或 https:// 开头");
    if (!form.groups.length) return toast.error("至少选择一个可用分组");

    // 新建：按行解析、逐行报错（密钥内部有空格、脱敏指纹、重复都由 parseKeyLines 判）。
    // 编辑：密钥池不在这个表单里 —— 它只有一条写路径（面板的 PATCH /keys）。
    const parsed = channel ? { keys: [] as string[], issues: [] as KeyLineIssue[] } : parseKeyLines(form.keysText);
    const blocking = firstBlockingIssue(parsed.issues);
    if (blocking) {
      return toast.error(blocking.line > 0 ? `第 ${blocking.line} 行：${blocking.message}` : blocking.message);
    }
    const keys = parsed.keys;
    if (!channel && !keys.length) return toast.error("新建渠道至少填 1 个密钥");

    const weight = Number(form.weight) || 0;
    const priority = Number(form.priority) || 0;
    if (weight < 0 || priority < 1) return toast.error("权重需 ≥0，优先级需 ≥1");

    const firstDeclError = Object.values(decl.errors)[0];
    if (firstDeclError) {
      toast.error(firstDeclError);
      return;
    }
    // 私有配置同理：后端会拒，但它的 400 只有一句话，运营不知道该改哪一项。
    if (optionsEnabled && optionsKnown) {
      const firstOptError = Object.values(optionsErrors)[0];
      if (firstOptError) {
        toast.error(firstOptError);
        return;
      }
    }

    const payload: ChannelPayload = {
      name,
      // 恒提交（含空串 = 清空）：省略等于「保持不变」，清除图标就永远不生效。
      icon: form.icon,
      adapter_id: form.adapterId.trim(),
      base_url: form.baseUrl.trim(),
      multi_key_mode: form.multiKeyMode,
      switch_key_retry: form.switchKeyRetry,
      groups: [...form.groups],
      auto_ban: form.autoBan,
      weight,
      priority,
      rpm_limit: Number(form.rpm) || 0,
      max_concurrency: Number(form.cc) || 0,
      // keys 只在新建时提交：PUT 不受理（后端 400），已有渠道的密钥池走面板。
      ...(channel ? {} : { keys }),
      // 编辑时总是显式带上（null = 清空）；新建时全空不带，保证请求体与改造前一致。
      ...(channel ? { adapter_config: decl.config } : decl.config ? { adapter_config: decl.config } : {}),
      // 私有配置只在「适配器支持 + 管理台认识其形状」时下发：
      //   支持但形状未知时**不传**（后端语义：不传 = 不修改），避免用别的适配器的默认文档覆盖它。
      ...(optionsValue ? { adapter_options: optionsValue } : {}),
    };

    const done = (msg: string) => {
      onClose();
      toast.success(msg);
    };
    const fail = (error: unknown) =>
      toast.error(error instanceof ApiError ? error.message : "保存失败，请稍后重试");

    if (channel) {
      updateMut.mutate({ id: channel.id, body: payload }, { onSuccess: () => done("渠道已更新"), onError: fail });
      return;
    }
    createMut.mutate(payload, {
      onSuccess: (data) => {
        done("渠道已创建");
        onCreated?.(data.item);
      },
      onError: fail,
    });
  };
  return (
    <>
      <Drawer
        open={open}
        onClose={onClose}
        title={channel ? `编辑渠道 · ${channel.name}` : "新建渠道"}
        titleIcon={channel ? { url: channel.icon_url, name: channel.name } : undefined}
        actions={[
          { label: "取消", onClick: onClose },
          { label: "保存", variant: "primary", disabled: isPending, onClick: submitForm },
        ]}
      >
        {/* 身份区：图标 + 名称同一行（左图右文）。识别渠道靠的就是「名字 + 图」，
            分两行等于让运营改完名字再往下找一块只有图的字段。
            渠道没有上游实体可继承，所以不传 fallback —— 留空即首字。 */}
        <Field label="渠道名称" req htmlFor="chz-name">
          <IconNameField
            id="chz-name"
            icon={form.icon}
            iconUrl={form.iconUrl}
            onIconChange={(v) => setField("icon", v)}
            initialFrom="渠道"
            text={form.name}
            onTextChange={(v) => setField("name", v)}
            placeholder="例如：OpenAI 官方"
            maxLength={64}
          />
        </Field>

        <div className="form-grid">
          <Field
            label="适配器"
            req
            htmlFor="chz-adapter"
            tip="决定上游协议"
          >
            <Select
              id="chz-adapter"
              items={adapterOptions}
              value={form.adapterId}
              onChange={(e) => onAdapterChange(e.target.value)}
            />
          </Field>
          <Field
            label="Base URL"
            req
            htmlFor="chz-url"
            hint={
              adapterMeta?.default_base_url
                ? "只到 /api/v3 为止，不要带具体接口路径"
                : undefined
            }
          >
            <input
              id="chz-url"
              type="text"
              placeholder={adapterMeta?.default_base_url || "https://api.openai.com"}
              value={form.baseUrl}
              onChange={(e) => setField("baseUrl", e.target.value)}
            />
          </Field>
        </div>

        {channel ? (
          // 编辑态：密钥池平铺在抽屉里，**不再开第二层抽屉**。
          // 关闭抽屉即卸载：重开时必定重新拉一次（冷却/最近错误是实时态），
          // 也顺带清掉"改到一半的别名/新密钥"这类草稿。
          // 这里不用 Field 包：Field 的 htmlFor 会指向编辑态并不存在的 #chz-keys。
          open ? (
            <ChannelKeysSection
              key={channel.id}
              channelId={channel.id}
              channelStatus={liveChannel?.status ?? channel.status}
            />
          ) : null
        ) : (
          <Field
            label="密钥池"
            req
            htmlFor="chz-keys"
            tip="每行一个密钥；仅管理员可见"
            hint={
              keysIssue
                ? keysIssue.line > 0
                  ? `第 ${keysIssue.line} 行：${keysIssue.message}`
                  : keysIssue.message
                : "至少 1 个；多个密钥由调度模式轮换，单 key 失败自动切换下一个"
            }
            hintWarn={!!keysIssue}
          >
            <textarea
              id="chz-keys"
              rows={3}
              placeholder={"sk-xxxx\nsk-yyyy"}
              value={form.keysText}
              onChange={(e) => setField("keysText", e.target.value)}
            />
          </Field>
        )}

        <Field label="多密钥调度">
          <SegmentedChips
            items={MODE_ITEMS}
            value={form.multiKeyMode}
            onChange={(v) => setField("multiKeyMode", v as MultiKeyMode)}
          />
        </Field>

        <SwitchRow
          label="失败时换同渠道其它密钥重试"
          id="chz-keyretry"
          checked={form.switchKeyRetry}
          onChange={(v) => setField("switchKeyRetry", v)}
        />

        <SwitchRow
          label="失败自动停用"
          tip="失败率超阈值时自动禁用"
          id="chz-autoban"
          checked={form.autoBan}
          onChange={(v) => setField("autoBan", v)}
        />

        <Field label="可用分组" req tip="决定该渠道的报价口径">
          <SegmentedChips
            items={groups.map((g) => ({ key: g, label: g }))}
            multiple
            values={form.groups}
            onChange={toggleGroup}
          />
        </Field>

        <div className="grid grid-2">
          <Field label="每分钟请求上限" htmlFor="chz-rpm">
            <input
              id="chz-rpm"
              type="number"
              min={0}
              value={form.rpm}
              onChange={(e) => setField("rpm", e.target.value)}
            />
          </Field>
          <Field label="并发上限" htmlFor="chz-cc">
            <input
              id="chz-cc"
              type="number"
              min={0}
              value={form.cc}
              onChange={(e) => setField("cc", e.target.value)}
            />
          </Field>
          <Field label="权重" htmlFor="chz-weight" tip="同优先级渠道按权重分摊流量；0 = 视为平滑值 10">
            <input
              id="chz-weight"
              type="number"
              min={0}
              value={form.weight}
              onChange={(e) => setField("weight", e.target.value)}
            />
          </Field>
          <Field
            label="优先级"
            req
            htmlFor="chz-priority"
            tip="数值越小越优先被选路；同级再按权重分摊"
          >
            <input
              id="chz-priority"
              type="number"
              min={1}
              value={form.priority}
              onChange={(e) => setField("priority", e.target.value)}
            />
          </Field>
        </div>

        {optionsForm === "autodl" ? (
          <>
            <div className="blk-title">上游工作流映射（AutoDL）</div>
            <div className="small muted">
              一个对外模型 ID 按请求内容自动识别四种生成模式，并选用下面的上游工作流。
              上游发布新模型时只改这里，无需改代码；四个 ID 不允许重复（同一工作流服务两种模式必有一种跑错）。
              渠道未配置时下面显示的是内置默认值，保存后按显式值写入。
            </div>
            <div className="row-acts">
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setForm((f) => ({ ...f, opts: emptyAutoDLForm() }))}
              >
                恢复内置默认
              </button>
            </div>

            <div className="grid grid-2">
              {AUTODL_MODE_ORDER.map((mode) => (
                <Field
                  key={mode}
                  label={AUTODL_MODE_LABELS[mode]}
                  htmlFor={`chz-wf-${mode}`}
                  tip={AUTODL_MODE_TIPS[mode]}
                  hint={optsBuild.errors[`workflows.${mode}`]}
                  hintWarn={!!optsBuild.errors[`workflows.${mode}`]}
                >
                  <input
                    id={`chz-wf-${mode}`}
                    type="text"
                    maxLength={64}
                    className="mono"
                    placeholder={AUTODL_WORKFLOW_DEFAULTS[mode]}
                    value={form.opts.workflows[mode]}
                    onChange={(e) => setWorkflow(mode, e.target.value)}
                  />
                </Field>
              ))}
            </div>

            <div className="blk-title">分辨率档位映射</div>
            <div className="small muted">
              平台计费档位 → 上游画质基名。上游最高 768p：720p 档映射到 768p 属口径约定；
              1080p 默认「不支持」（静默降档会变成「要 1080p、拿到 768p、按 1080p 付费」）。
            </div>
            <div className="grid grid-2">
              {AUTODL_TIERS.map((tier) => (
                <Field
                  key={tier}
                  label={`${tier} 档`}
                  htmlFor={`chz-res-${tier}`}
                  hint={optsBuild.errors[`resolution_map.${tier}`]}
                  hintWarn={!!optsBuild.errors[`resolution_map.${tier}`]}
                >
                  <Select
                    id={`chz-res-${tier}`}
                    items={AUTODL_RESOLUTION_ITEMS}
                    value={form.opts.resolution[tier]}
                    onChange={(e) => setResolution(tier, e.target.value)}
                  />
                </Field>
              ))}
            </div>
          </>
        ) : null}

        {optionsForm === "metaso" ? (
          <>
            <div className="blk-title">通道选择与请求选项（Metaso / MiniMax-H3）</div>
            <div className="small muted">
              标准通道与优先通道是两个对外模型 ID（metaso-h3 标准价 / metaso-h3-fast 2 倍价）：
              平台先按模型价目计价冻结、再选渠道，所以「走哪条通道」只能由渠道决定，
              请求体里的 fast_mode 一律被忽略（否则用户可自助按标准价买优先通道）。
            </div>

            <SwitchRow
              id="chz-metaso-fast"
              label="使用优先通道（fast_mode）"
              tip="仅绑定优先通道模型的渠道应开启"
              checked={form.metaso.fastMode}
              onChange={(v) => setForm((f) => ({ ...f, metaso: { ...f.metaso, fastMode: v } }))}
            />
            {form.metaso.fastMode ? (
              <div className="hint warn">
                该渠道会按优先通道下单：请确认它绑定的模型是 metaso-h3-fast（价目为标准的 2 倍）。
                标准模型请关掉此开关，否则计费与实付倒挂。
              </div>
            ) : null}

            <Field
              label="上下文理解（context_ir_enabled）"
              htmlFor="chz-metaso-cir"
              tip="不指定 = 不下发该字段"
            >
              <Select
                id="chz-metaso-cir"
                items={METASO_CONTEXT_IR_ITEMS}
                value={form.metaso.contextIR}
                onChange={(e) => setMetasoContextIR(e.target.value as MetasoContextIR)}
              />
            </Field>

            <div className="blk-title">分辨率档位映射</div>
            <div className="small muted">
              平台计费档位 → 上游字面量，四档默认全开（按上游价目表逐档计费）。
              清成「未开通」后该档请求在本地直接拒绝，不会静默降档到其它档位；
              上游 2K 档时长上限 15 秒，超过会在提交前被拦（不是提交后失败）。
            </div>
            <div className="grid grid-2">
              {METASO_TIERS.map((tier) => (
                <Field
                  key={tier}
                  label={`${tier} 档`}
                  htmlFor={`chz-metaso-res-${tier}`}
                  hint={metasoBuild.errors[`resolution_map.${tier}`]}
                  hintWarn={!!metasoBuild.errors[`resolution_map.${tier}`]}
                >
                  <Select
                    id={`chz-metaso-res-${tier}`}
                    items={METASO_RESOLUTION_ITEMS}
                    value={form.metaso.resolution[tier]}
                    onChange={(e) => setMetasoTier(tier, e.target.value)}
                  />
                </Field>
              ))}
            </div>

            <Field
              label="任务查询路径"
              htmlFor="chz-metaso-qp"
              tip="留空用内置默认；通常以 / 结尾"
              hint={
                metasoBuild.errors.query_path || `留空使用默认：${METASO_DEFAULT_QUERY_PATH}`
              }
              hintWarn={!!metasoBuild.errors.query_path}
            >
              <input
                id="chz-metaso-qp"
                type="text"
                className="mono"
                maxLength={METASO_MAX_QUERY_PATH_LEN}
                placeholder={METASO_DEFAULT_QUERY_PATH}
                value={form.metaso.queryPath}
                onChange={(e) => setMetasoQueryPath(e.target.value)}
              />
            </Field>
          </>
        ) : null}

        {optionsForm === "duomiapi" ? (
          <>
            <div className="blk-title">请求选项（多米API）</div>
            <div className="small muted">
              多米把多家上游（火山 Seedance、可灵、Gemini 图像、SUNO）统一在一个域名下，但各自的
              请求/查询信封不同。这里配置的是**本渠道**的出网方式；提交时用哪套信封由「对外模型 ID
              → 协议档案」决定，不是由请求体决定。
            </div>

            <Field
              label="鉴权前缀"
              htmlFor="chz-duomi-auth"
              hint={
                duomiBuild.errors.auth_prefix ||
                (form.duomi.authMode === "default"
                  ? `未下发，运行时使用内置默认「${DUOMI_DEFAULT_AUTH_PREFIX}」（含尾随空格）：Authorization: Bearer␠<token>`
                  : form.duomi.authMode === "none"
                    ? "将下发「-」，运行时归一为空前缀：Authorization: <token>"
                    : "按填写内容原样下发")
              }
              hintWarn={!!duomiBuild.errors.auth_prefix}
            >
              <Select
                id="chz-duomi-auth"
                items={DUOMI_AUTH_ITEMS}
                value={form.duomi.authMode}
                onChange={(e) => setDuomi("authMode", e.target.value as DuomiAuthMode)}
              />
            </Field>
            {form.duomi.authMode === "custom" ? (
              <Field
                label="自定义前缀内容"
                htmlFor="chz-duomi-auth-custom"
                hint={
                  duomiBuild.errors.auth_prefix ||
                  "尾随空格有意义（Bearer 与 Bearer␠ 是两个不同的头），不会被自动裁剪"
                }
                hintWarn={!!duomiBuild.errors.auth_prefix}
              >
                <input
                  id="chz-duomi-auth-custom"
                  type="text"
                  className="mono"
                  maxLength={40}
                  placeholder={DUOMI_DEFAULT_AUTH_PREFIX}
                  value={form.duomi.authCustom}
                  onChange={(e) => setDuomi("authCustom", e.target.value)}
                />
              </Field>
            ) : null}

            <div className="blk-title">上游路径（留空用内置默认）</div>
            <div className="small muted">
              仅在上游改了版本号时必须调整。可灵的路径前缀以 / 结尾，任务号会直接拼在其后；
              填错的表现是「提交成功但一直查不到」，而不是明确的报错。
            </div>
            <div className="grid grid-2">
              {DUOMI_PATH_ITEMS.map(([key, label]) => (
                <Field
                  key={key}
                  label={label}
                  // 路径键本身带点（seedance.submit），不能直接拼进 id：
                  // `#chz-duomi-path-kling.prefix` 会被浏览器当成「id + class」而找不到元素。
                  htmlFor={`chz-duomi-path-${key.replace(/\./g, "-")}`}
                  hint={
                    duomiBuild.errors[`paths.${key}`] ||
                    (key === "kling.prefix"
                      ? duomiKlingPrefixHint(form.duomi.paths[key]) || `默认：${DUOMI_DEFAULT_PATHS[key]}`
                      : `默认：${DUOMI_DEFAULT_PATHS[key]}`)
                  }
                  hintWarn={!!duomiBuild.errors[`paths.${key}`]}
                >
                  <input
                    id={`chz-duomi-path-${key.replace(/\./g, "-")}`}
                    type="text"
                    className="mono"
                    placeholder={DUOMI_DEFAULT_PATHS[key]}
                    value={form.duomi.paths[key]}
                    onChange={(e) => setDuomiPath(key, e.target.value)}
                  />
                </Field>
              ))}
            </div>

            <div className="blk-title">metadata 透传白名单</div>
            <div className="small muted">
              只有勾选的键才会从请求体的 metadata 透传给上游，其余直接 400。默认只放行
              aigc_watermark；generate_audio / watermark 会改变上游产出**与成本**，开放给调用方
              等于让人自助改计费维度 —— 放行前请确认对应模型的价目已把这些成本算进去。
              全部取消勾选 = 一个都不透传（合法的显式关闭，不会回落成默认放行）。
            </div>
            <Field
              label="允许透传的键"
              htmlFor={`chz-duomi-md-${DUOMI_METADATA_ITEMS[0][0]}`}
              hint={duomiBuild.errors.metadata_allow}
              hintWarn={!!duomiBuild.errors.metadata_allow}
            >
              <div>
                {DUOMI_METADATA_ITEMS.map(([key, label]) => (
                  <SwitchRow
                    key={key}
                    id={`chz-duomi-md-${key}`}
                    label={label}
                    checked={form.duomi.metadataAllow.includes(key)}
                    onChange={(v) => toggleDuomiMetadata(key, v)}
                  />
                ))}
              </div>
            </Field>

            <div className="blk-title">音乐默认参数（SUNO）</div>
            <div className="small muted">
              决定这个渠道出的到底是「歌」还是「纯音乐」，属于产品形态而不是每次请求的参数，
              所以放在渠道侧；调用方仍可用请求体里的同名键覆盖。两项都选「沿用内置默认」时
              不写入配置（后端按非自定义 + 带人声处理）。
            </div>
            <div className="grid grid-2">
              <Field
                label="默认自定义歌词模式"
                htmlFor="chz-duomi-suno-custom"
                hint={duomiBuild.errors.suno_defaults}
                hintWarn={!!duomiBuild.errors.suno_defaults}
              >
                <Select
                  id="chz-duomi-suno-custom"
                  value={form.duomi.sunoCustomMode}
                  onChange={(e) => setDuomi("sunoCustomMode", e.target.value as TriBool)}
                  items={DUOMI_TRIBOOL_ITEMS as [string, string][]}
                />
              </Field>
              <Field label="默认纯音乐（不唱）" htmlFor="chz-duomi-suno-inst">
                <Select
                  id="chz-duomi-suno-inst"
                  value={form.duomi.sunoInstrumental}
                  onChange={(e) => setDuomi("sunoInstrumental", e.target.value as TriBool)}
                  items={DUOMI_TRIBOOL_ITEMS as [string, string][]}
                />
              </Field>
            </div>
            <Field
              label="上游模型版本（可选）"
              htmlFor="chz-duomi-suno-mv"
              hint={
                duomiBuild.errors["suno_defaults.mv"] ||
                `留空 = 用绑定表里的上游模型名（例如 chirp-v4）`
              }
              hintWarn={!!duomiBuild.errors["suno_defaults.mv"]}
            >
              <input
                id="chz-duomi-suno-mv"
                type="text"
                maxLength={DUOMI_MAX_SUNO_MV_LEN}
                placeholder="chirp-v4"
                value={form.duomi.sunoMv}
                onChange={(e) => setDuomi("sunoMv", e.target.value)}
              />
            </Field>

            <div className="blk-title">模型协议覆盖（可选，JSON）</div>
            <div className="small muted">{duomiOverridesHint()}</div>
            <Field
              label="model_overrides"
              htmlFor="chz-duomi-ov"
              hint={
                overridesErrorText ||
                `形状：{"模型ID": {"protocol": "...", "mode": "std|pro", "upstream_model": "...", "duration_as_string": true}}；` +
                  `留空 = 用内置档案。配置体积上限 ${DUOMI_MAX_OPTIONS_BYTES} 字节。`
              }
              hintWarn={!!overridesErrorText}
            >
              <textarea
                id="chz-duomi-ov"
                className="mono"
                rows={8}
                spellCheck={false}
                placeholder={'{\n  "my-seedance-lite": { "protocol": "seedance", "upstream_model": "doubao-seedance-lite-260101" }\n}'}
                value={form.duomi.overridesText}
                onChange={(e) => setDuomi("overridesText", e.target.value)}
              />
            </Field>
          </>
        ) : null}

        {optionsEnabled && !optionsKnown ? (
          <div className="small muted">
            适配器「{form.adapterId}」声明了渠道级私有配置，但管理台还没有对应表单。
            编辑已有渠道时不会改动它的配置（不传 = 不修改）；如需调整请走 API。
          </div>
        ) : null}

        <div className="blk-title">非标准上游适配（可选）</div>
        {declDisabled ? (
          <div className="small muted">
            当前适配器（{form.adapterId || "未选择"}）不支持渠道级声明式配置：
            插件渠道的鉴权头与出网路径由插件清单管理。
          </div>
        ) : (
          <>
            <div className="row-acts">
              {ADAPTER_PRESETS.map((p) => (
                <button
                  key={p.key}
                  type="button"
                  className="btn btn-sm"
                  title={p.hint}
                  onClick={() => setForm((f) => ({ ...f, decl: { ...p.form } }))}
                >
                  {p.label}
                </button>
              ))}
            </div>

            <div className="form-grid">
              <Field label="鉴权头名" htmlFor="chz-auth-header" tip="默认 Authorization；需要 x-api-key 的上游选右侧预设" hint={decl.errors.authHeader} hintWarn={!!decl.errors.authHeader}>
                <input id="chz-auth-header" type="text" maxLength={64} placeholder="Authorization" value={form.decl.authHeader} onChange={(e) => setDecl("authHeader", e.target.value)} />
              </Field>
              <Field label="鉴权前缀" htmlFor="chz-auth-prefix" tip={PREFIX_TIP}>
                <input id="chz-auth-prefix" type="text" maxLength={32} placeholder="Bearer" value={form.decl.authPrefix} onChange={(e) => setDecl("authPrefix", e.target.value)} />
              </Field>
            </div>

            <Field label="路径覆盖" htmlFor="chz-paths" tip={PATHS_TIP} hint={decl.errors.pathOverrides} hintWarn={!!decl.errors.pathOverrides}>
              <textarea id="chz-paths" className="mono" rows={4} placeholder={'{\n  "/v1/chat/completions": "/v4/chat/completions"\n}'} value={form.decl.pathOverrides} onChange={(e) => setDecl("pathOverrides", e.target.value)} />
            </Field>

            <Field label="请求体改写" htmlFor="chz-patch" tip={PATCH_TIP} hint={decl.errors.requestPatch} hintWarn={!!decl.errors.requestPatch}>
              <textarea id="chz-patch" className="mono" rows={4} placeholder={'{\n  "temperature": null,\n  "thinking": { "type": "enabled" }\n}'} value={form.decl.requestPatch} onChange={(e) => setDecl("requestPatch", e.target.value)} />
            </Field>

            <Field label="响应字段映射" htmlFor="chz-resp-path" tip={RESP_TIP} hint={decl.errors.responsePath} hintWarn={!!decl.errors.responsePath}>
              <textarea id="chz-resp-path" className="mono" rows={4} placeholder={'{\n  "content": "data.answer"\n}'} value={form.decl.responsePath} onChange={(e) => setDecl("responsePath", e.target.value)} />
            </Field>
          </>
        )}


        {channel ? (
          <div className="kv-grid mt16">
            <div className="kv">
              <div className="k">渠道余额</div>
              <div className="v">{channel.balance_cny === null ? "—" : `¥${channel.balance_cny.toFixed(2)}`}</div>
            </div>
            <div className="kv">
              <div className="k">最近测试</div>
              <div className="v small">
                {channel.health?.last_test_at ? formatDateTime(channel.health.last_test_at) : "未测试"}
                {channel.health?.last_error ? ` · ${channel.health.last_error}` : ""}
              </div>
            </div>
          </div>
        ) : null}
      </Drawer>
    </>
  );
}
