import { useEffect, useMemo, useRef, useState } from "react";
import { useSearch } from "@tanstack/react-router";
import { toast } from "sonner";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Card } from "@/components/common/Card";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { EntityIcon } from "@/components/common/EntityIcon";
import { Field } from "@/components/common/Field";
import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { ModelTagList } from "@/components/common/ModelTag";
import { PageHead } from "@/components/common/PageHead";
import { SegmentedChips } from "@/components/common/SegmentedChips";
import { Select } from "@/components/common/Select";
import { ModalityTag, StatusTag } from "@/components/common/StatusTag";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Tip } from "@/components/common/Tip";
import {
  useCreateKeyMutation,
  useDeleteKeyMutation,
  useKeysQuery,
  useMeQuery,
  usePricingQuery,
  useResetKeyMutation,
  useIssueHostedKeyMutation,
  useRevealKeyMutation,
  useRotateHostedKeyMutation,
  useUpdateKeyMutation,
  useUpdateKeyStatusMutation,
} from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { formatDateTime, modalityLabel } from "@/lib/format";
import { openaiBase, useSite } from "@/lib/site";
import { copyToast } from "@/lib/clipboard";
import { cn } from "@/lib/utils";
import { currentSymbol, groupDiscountText, microOf, microToInput, moneyOf } from "@/lib/money";
import type { ApiKeyItem, KeyPayload, Modality } from "@/types";

/** 模态白名单（冻结 8 类，勾选顺序固定）
 *
 * 顺序必须与后端 `controller/api_key.go` 的模态表一致：这里少一类，门户就无法给
 * 该类模型单独授权（key 的模态白名单一旦非空就只放行列出的模态，漏掉 = 直接 403）。
 */
const MODALITIES: Modality[] = [
  "chat",
  "image",
  "video",
  "audio",
  "music",
  "embedding",
  "rerank",
  "moderation",
];

/** KEY 名称长度上限（对齐原型 maxlength=30） */
const NAME_MAX = 30;

/** IPv4 / CIDR 校验（对齐原型 CIDR_RE） */
const CIDR_RE =
  /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}(\/(3[0-2]|[12]?\d))?$/;

/** 业务错误码 → 用户可读提示（C-06） */
const KEY_ERROR_MESSAGES: Record<string, string> = {
  key_conflict: "已存在同名密钥，请更换名称",
  hosted_key_managed: "托管密钥由应用管理，请在卡片上轮换",
};

function toastError(error: unknown): void {
  if (error instanceof ApiError && error.errorCode && KEY_ERROR_MESSAGES[error.errorCode]) {
    toast.error(KEY_ERROR_MESSAGES[error.errorCode]);
    return;
  }
  toast.error(error instanceof ApiError ? error.message : "操作失败，请稍后重试");
}

/** 创建 / 编辑表单状态（额度按结算货币金额输入，提交时转成微） */
interface KeyFormState {
  name: string;
  group: string;
  /** 消费上限（金额文本；0 / 空 = 不限） */
  amountLimit: string;
  expiredAt: string;
  models: string[];
  modalities: Modality[];
  ips: string;
  rpm: string;
  cc: string;
  allowQuery: boolean;
}

/**
 * 门户 API 密钥（S3）
 * 卡片列表：三态密钥显示（脱敏 / 完整回显 / 复制）、额度进度、白名单 chips 与操作；
 * 创建 / 编辑弹窗（模型白名单标签输入 + 模态多选 + IP 校验）；创建 / 重置后一次性明文展示。
 */
export function KeysPage() {
  const keysQuery = useKeysQuery();
  const site = useSite();
  const pricing = usePricingQuery();
  const me = useMeQuery();
  // 门户只显「自己分组」，公开端点不下发整张折扣表。
  // 分组下拉只保留自己那一档（防止用户给自己的密钥自选到别的分组享折扣）。
  const myGroup = me.data?.group ?? "default";
  const myDiscount = me.data?.group_discount;
  // 历史密钥若属于其它分组（管理员改过），当前分组折扣不适用 —— 显示「未知折扣」
  // 而不是硬套自己的折扣：那会让用户以为"改个分组就能享受这个价"。
  const discountOf = (g: string) => (g === myGroup ? groupDiscountText(myDiscount) : "未知折扣");
  const groupKeys = [myGroup];
  const defaultGroup = myGroup;
  const keys = keysQuery.data?.items ?? [];

  const createMut = useCreateKeyMutation();
  const updateMut = useUpdateKeyMutation();
  const statusMut = useUpdateKeyStatusMutation();
  const resetMut = useResetKeyMutation();
  const deleteMut = useDeleteKeyMutation();
  const revealMut = useRevealKeyMutation();
  // 托管密钥：轮换入口。slug 与 app 名随列表直出（key.app_slug / key.app_name），
  // 不再另调 /portal/apps 反查 —— 那会让"能不能轮换"取决于另一个请求是否先成功。
  const rotateMut = useRotateHostedKeyMutation();
  // 首次接入：为某个应用换取托管密钥。**不提供「可接入应用清单」**——
  // 那等于把平台接入了哪些产品交给任何登录用户（后端 UserAppList 的注释写了同一条理由），
  // 标识由应用方提供（平台预置的那一个是文档里写死的 default）。
  const issueMut = useIssueHostedKeyMutation();

  /* ---------------------------- 三态显示状态 ---------------------------- */
  const [plaintext, setPlaintext] = useState<Record<number, string>>({});
  const [shown, setShown] = useState<Record<number, boolean>>({});

  /* ---------------------------- 弹窗状态 ---------------------------- */
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<ApiKeyItem | null>(null);
  const [form, setForm] = useState<KeyFormState>(() => emptyForm(defaultGroup));
  const [modelKw, setModelKw] = useState("");
  const [sugOpen, setSugOpen] = useState(false);
  /** 一次性明文（创建 / 重置后），null 即关闭 */
  // hosted：托管密钥的明文丢失后只能「轮换」，自建密钥可以「重置」。
  // 两种凭证的补救路径不同，所以一次性弹窗的警示语不能共用一句。
  const [secret, setSecret] = useState<{
    name: string;
    value: string;
    hosted?: boolean;
  } | null>(null);
  const [issueOpen, setIssueOpen] = useState(false);
  const [issueSlug, setIssueSlug] = useState("");

  const modelSuggestions = useMemo(() => {
    const kw = modelKw.trim().toLowerCase();
    // 目录只下发可路由模型，无需再判 enabled（该字段已不在用户端契约里）。
    const all = pricing.data?.models ?? [];
    return all
      .filter(
        (m) =>
          !form.models.includes(m.id) &&
          (!kw || m.id.toLowerCase().includes(kw) || m.display_name.toLowerCase().includes(kw)),
      )
      .slice(0, 8);
  }, [pricing.data, form.models, modelKw]);

  /** 已选模型 id → 目录行（tag 上的图标要它；找不到就退回首字母） */
  const modelByID = useMemo(
    () => new Map((pricing.data?.models ?? []).map((m) => [m.id, m])),
    [pricing.data],
  );

  const setField = <K extends keyof KeyFormState>(key: K, value: KeyFormState[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  const openCreate = () => {
    setEditing(null);
    setForm(emptyForm(defaultGroup));
    setModelKw("");
    setFormOpen(true);
  };

  const openEdit = (key: ApiKeyItem) => {
    setEditing(key);
    setForm(formFromKey(key));
    setModelKw("");
    setFormOpen(true);
  };

  const closeForm = () => setFormOpen(false);

  /** 主页快捷入口 /keys?create=1 直达创建弹窗（仅触发一次，关闭后不重开） */
  const autoOpened = useRef(false);
  const createParam = Number((useSearch({ strict: false }) as { create?: unknown }).create) || 0;
  useEffect(() => {
    if (createParam && !autoOpened.current) {
      autoOpened.current = true;
      openCreate();
    }
  }, [createParam, openCreate]);

  const addModel = (id: string) => {
    setForm((f) => (f.models.includes(id) ? f : { ...f, models: [...f.models, id] }));
    setModelKw("");
  };
  const removeModel = (id: string) =>
    setForm((f) => ({ ...f, models: f.models.filter((m) => m !== id) }));
  const toggleModality = (m: Modality) =>
    setForm((f) => ({
      ...f,
      modalities: f.modalities.includes(m)
        ? f.modalities.filter((x) => x !== m)
        : [...f.modalities, m],
    }));

  const submitForm = () => {
    const name = form.name.trim();
    if (!name) {
      toast.error("请填写密钥名称");
      return;
    }
    if (name.length > NAME_MAX) {
      toast.error(`名称不超过 ${NAME_MAX} 字`);
      return;
    }
    const ips = form.ips
      .split(/[\n,;]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    const badIndex = ips.findIndex((ip) => !CIDR_RE.test(ip));
    if (badIndex >= 0) {
      toast.error(`IP 白名单第 ${badIndex + 1} 条「${ips[badIndex]}」不是合法的 IP 段（如 203.0.113.0/24）`);
      return;
    }
    // 上限按金额文本解析成微；空串与 "0" 都是「不限」。
    const limitRaw = form.amountLimit.trim();
    const limitMicro = limitRaw === "" ? 0 : microOf(limitRaw);
    if (limitMicro === null) {
      toast.error(`额度上限「${limitRaw}」不是合法金额（只能填数字，最多 6 位小数）`);
      return;
    }
    // 不提交 group：它不是密钥属性。
    //
    // 分组决定折扣倍率与可用渠道池，属于**账号**（users.group），由管理员维护。
    // 曾经这里可以自选，而默认种子就把 vip 配成 9 折并挂进了渠道分组 ——
    // 用户点一下下拉就能给自己打折。后端现在会拒绝非空 group（400），
    // 所以这里必须真的不传，而不是传一个"看起来等于自己分组"的值。
    const payload: KeyPayload = {
      name,
      amount_limit: limitMicro > 0 ? limitMicro : 0,
      expired_at: form.expiredAt || null,
      models_allowed: form.models.length ? [...form.models] : null,
      modalities_allowed: form.modalities.length ? [...form.modalities] : null,
      allow_ips: ips.length ? ips : null,
      rpm_limit: Number(form.rpm) || 0,
      concurrency_limit: Number(form.cc) || 0,
      allow_user_quota_query: form.allowQuery,
    };
    if (editing) {
      updateMut.mutate(
        { id: editing.id, body: payload },
        { onSuccess: () => { closeForm(); toast.success("密钥已更新"); }, onError: toastError },
      );
      return;
    }
    createMut.mutate(payload, {
      onSuccess: (data) => {
        closeForm();
        setSecret({ name: data.key.name, value: data.plaintext });
        toast.success("密钥创建成功");
      },
      onError: toastError,
    });
  };

  const toggleReveal = (key: ApiKeyItem) => {
    if (shown[key.id]) {
      setShown((s) => ({ ...s, [key.id]: false }));
      return;
    }
    const cached = plaintext[key.id];
    if (cached) {
      setShown((s) => ({ ...s, [key.id]: true }));
      return;
    }
    revealMut.mutate(key.id, {
      onSuccess: (data) => {
        setPlaintext((p) => ({ ...p, [key.id]: data.plaintext }));
        setShown((s) => ({ ...s, [key.id]: true }));
      },
      onError: toastError,
    });
  };

  const handleCopy = (key: ApiKeyItem) => {
    const cached = plaintext[key.id];
    if (cached) {
      void copyToast(cached);
      return;
    }
    revealMut.mutate(key.id, {
      onSuccess: (data) => {
        setPlaintext((p) => ({ ...p, [key.id]: data.plaintext }));
        void copyToast(data.plaintext);
      },
      onError: toastError,
    });
  };

  const askReset = async (key: ApiKeyItem) => {
    const ok = await confirmDialog({
      title: "重置密钥",
      body: (
        <>
          确定重置 <b>{key.name}</b>（<span className="mono">{key.key_prefix}…</span>）？<br />
          旧密钥<b style={{ color: "var(--danger)" }}>立即失效</b>，新密钥需重新保存。
        </>
      ),
      okText: "确认重置",
      okVariant: "primary",
    });
    if (!ok) return;
    resetMut.mutate(key.id, {
      onSuccess: (data) => {
        setPlaintext((p) => ({ ...p, [key.id]: data.plaintext }));
        setShown((s) => ({ ...s, [key.id]: false }));
        setSecret({ name: key.name, value: data.plaintext });
        toast.success("密钥已重置");
      },
      onError: toastError,
    });
  };

  /** 轮换托管密钥：换新明文 + 续期；旧明文在重叠窗口内仍可用（窗口由平台配置）。 */
  const askRotate = async (key: ApiKeyItem) => {
    const ok = await confirmDialog({
      title: "轮换托管密钥",
      body: (
        <>
          确定轮换 <b>{key.name}</b>（<span className="mono">{key.key_prefix}…</span>）？<br />
          轮换后立即生成新密钥，旧密钥仍可继续使用一段时间（默认 72 小时，以轮换结果为准）。
          请在这段时间内把新明文配置到应用 —— 窗口结束后旧密钥将返回{" "}
          <span className="mono">key_retired</span>，应用随即中断。
        </>
      ),
      okText: "确认轮换",
      okVariant: "primary",
    });
    if (!ok) return;
    const slug = key.app_slug;
    if (!slug) {
      toast.error("该密钥没有归属应用信息，无法轮换");
      return;
    }
    rotateMut.mutate(slug, {
      onSuccess: (data) => {
        if (data.plaintext) {
          setPlaintext((p) => ({ ...p, [key.id]: data.plaintext }));
          setShown((s) => ({ ...s, [key.id]: false }));
          setSecret({ name: key.name, value: data.plaintext, hosted: true });
        }
        // 必须回显窗口终点：用户需要知道"还有多久必须换完"，
        // 否则只能等应用报错才知道。
        toast.success(
          data.key.previous_expires_at
            ? `托管密钥已轮换，旧密钥可用至 ${formatDateTime(data.key.previous_expires_at)}`
            : "托管密钥已轮换，旧密钥已立即失效",
        );
      },
      onError: toastError,
    });
  };

  const askToggle = async (key: ApiKeyItem) => {
    const disable = key.status === 1;
    const ok = await confirmDialog({
      title: disable ? "禁用密钥" : "启用密钥",
      body: disable ? (
        <>
          确定禁用 <b>{key.name}</b>？禁用后该密钥的所有请求将立即被拒绝（403），余额与配置保留。
        </>
      ) : (
        <>
          确定启用 <b>{key.name}</b>？启用后请求立即恢复。
        </>
      ),
    });
    if (!ok) return;
    statusMut.mutate(
      { id: key.id, status: disable ? 2 : 1 },
      { onSuccess: () => toast.success(disable ? "密钥已禁用" : "密钥已启用"), onError: toastError },
    );
  };

  const askDelete = async (key: ApiKeyItem) => {
    const ok = await confirmDialog({
      title: "删除密钥",
      body: (
        <>
          确定删除 <b>{key.name}</b>（<span className="mono">{key.key_prefix}…</span>）？
          {key.status === 1 ? (
            <>
              <br />
              <b style={{ color: "var(--danger)" }}>该密钥处于启用状态，删除后其所有请求立即失败。</b>
            </>
          ) : null}
          <br />
          删除后不可恢复，历史调用记录保留。
        </>
      ),
      okText: "确认删除",
      okVariant: "danger",
    });
    if (!ok) return;
    deleteMut.mutate(key.id, {
      onSuccess: () => {
        setPlaintext((p) => {
          const next = { ...p };
          delete next[key.id];
          return next;
        });
        toast.success("密钥已删除");
      },
      onError: toastError,
    });
  };

  /**
   * 首次接入：用应用标识换取托管密钥。
   *
   * 这里**不做"可接入应用"的下拉**：平台接入了哪些产品属于拓扑信息，列出来等于
   * 让任何登录用户枚举一遍（后端 `UserAppList` 的第一条注释就是这条约束）。
   * 标识由应用方提供；平台预置的那个（default）写在接入指南里。
   */
  const submitIssue = async () => {
    const slug = issueSlug.trim();
    if (!slug) return;
    try {
      const data = await issueMut.mutateAsync(slug);
      setIssueOpen(false);
      setIssueSlug("");
      if (data.plaintext) {
        setSecret({
          name: `${data.app.name} 的托管密钥`,
          value: data.plaintext,
          hosted: true,
        });
        toast.success("已换取托管密钥");
        return;
      }
      // 空明文 = 该应用下你本来就有一把，服务端没有生成新的。
      // 这时**不能**弹一次性明文框：那会让人把空串当成密钥复制进配置。
      toast.info(
        `你已经持有「${data.app.name}」的托管密钥（${data.key.prefix}…），` +
          "如需换新，请在下方密钥卡片上轮换",
      );
    } catch (e) {
      if (e instanceof ApiError && e.errorCode === "app_not_found") {
        // 「不存在」与「已停用」共用这个码（分开返回等于给了枚举开关），文案要跟着合并。
        toast.error("应用标识不存在，或该应用已停用；请与应用方确认标识");
        return;
      }
      if (e instanceof ApiError && e.errorCode === "forbidden") {
        toast.error("该应用已关闭自助接入，请联系运营获取接入凭证");
        return;
      }
      toastError(e);
    }
  };

  /* ------------------------------ 渲染 ------------------------------ */
  const loading = keysQuery.isLoading;
  // 后端已按「配置 → 站点地址 → 请求 origin」回落；这里只兜住"连请求 origin 都拿不到"
  // 的空列表 —— 那时的 apiBase 也已经是同源，两种取值等价。
  const bases = site.apiBases.length > 0 ? site.apiBases : [site.apiBase];

  return (
    <>
      <PageHead
        title="API 密钥"
        actions={[
          {
            type: "btn",
            label: "接入应用",
            icon: "link",
            onClick: () => {
              setIssueSlug("");
              setIssueOpen(true);
            },
          },
          { type: "btn", label: "创建 API KEY", variant: "primary", icon: "plus", onClick: openCreate },
        ]}
      />

      {/* 地址栏与具体密钥无关，放在页头下方：空态（还没建 KEY）时也要能看见，
          否则新用户拿不到接入地址，只能去猜域名。 */}
      <Card
        title="OpenAI 兼容地址"
        // 兜底状态写在 sub 里：地址栏自己就是区块级说明位，
        // 再加一行常驻小字等于把「当前用的是兜底值」藏进正文噪音。
        sub={
          site.apiSource === "config"
            ? "SDK 的 base_url 指向这里，鉴权用下方任一 API KEY"
            : "未配置对外地址，当前展示的是兜底地址（可在「通用 · 站点品牌」配置）"
        }
      >
        {bases.map((origin, i) => {
          const url = openaiBase(origin);
          return (
            <div className="gr-row gr-row--url" key={url}>
              <Input
                readOnly
                className="url-input mono"
                value={url}
                aria-label={i === 0 ? "OpenAI 兼容地址" : `备用 OpenAI 兼容地址 ${i + 1}`}
              />
              <button
                type="button"
                className="btn btn-sm"
                aria-label={`复制地址 ${i + 1}`}
                onClick={() => void copyToast(url)}
              >
                <Icon name="copy" size={15} />
                复制
              </button>
            </div>
          );
        })}
      </Card>

      {loading ? (
        <div className="card">
          <EmptyState variant="loading" title="正在加载密钥…" />
        </div>
      ) : keysQuery.isError ? (
        <div className="card">
          <EmptyState
            variant="error"
            title="密钥加载失败"
            desc={keysQuery.error instanceof ApiError ? keysQuery.error.message : "请稍后重试"}
            action={
              <button type="button" className="btn btn-primary" onClick={() => void keysQuery.refetch()}>
                重试
              </button>
            }
          />
        </div>
      ) : !keys.length ? (
        <div className="card">
          <EmptyState
            icon="🔑"
            title="还没有密钥"
            desc="创建第一个密钥后即可调用全部模型；不确定怎么接入？先看看模型广场的接入示例。"
            action={
              <button type="button" className="btn btn-primary" onClick={openCreate}>
                创建第一个密钥
              </button>
            }
          />
        </div>
      ) : (
        keys.map((key) => {
          const usedPct = key.amount_limit > 0 ? Math.min(100, (key.used / key.amount_limit) * 100) : 0;
          const barTone = usedPct >= 90 ? "danger" : usedPct >= 70 ? "warn" : "";
          const isShown = !!shown[key.id];
          const full = plaintext[key.id];
          // 托管密钥：明文不可取（无常驻 reveal/copy），启停/删除被后端 409 拦截，
          // 唯一操作是轮换（换新明文 + 续期）；禁用态只能联系平台解锁。
          const hosted = key.kind === "hosted";
          return (
            <div key={key.id} className={cn("key-card", key.status !== 1 && "disabled")}>
              <div className="kc-main">
                <div className="kc-name">
                  {key.name}
                  <StatusTag status={key.status} />
                  {hosted ? (
                    <span className="badge badge-info">
                      应用托管{key.app_name ? ` · ${key.app_name}` : ""}
                    </span>
                  ) : (
                    <span className="badge badge-info">
                      {key.group} · {discountOf(key.group)}
                    </span>
                  )}
                </div>
                <div className="key-line">
                  {/* 托管密钥无常驻明文（轮换时一次性弹窗给出），恒显示掩码；
                      自建密钥保留三态显示与复制。 */}
                  <span className="kv-text" title={!hosted && isShown ? full : undefined}>
                    {!hosted && isShown && full ? full : `${key.key_prefix}••••••••`}
                  </span>
                  {!hosted && (
                    <>
                      <button
                        type="button"
                        className="icon-btn"
                        aria-label={isShown ? "隐藏完整密钥" : "显示完整密钥"}
                        title={isShown ? "隐藏完整密钥" : "显示完整密钥"}
                        onClick={() => toggleReveal(key)}
                      >
                        <Icon name="eye" size={15} />
                      </button>
                      <button
                        type="button"
                        className="icon-btn"
                        aria-label="复制完整密钥"
                        title="复制完整密钥"
                        onClick={() => handleCopy(key)}
                      >
                        <Icon name="copy" size={15} />
                      </button>
                    </>
                  )}
                </div>
                <div className="kc-meta">
                  {hosted && key.scopes?.length
                    ? key.scopes.slice(0, 3).map((s) => (
                        <span key={s} className="badge badge-primary mono">
                          {s}
                        </span>
                      ))
                    : null}
                  {hosted && (key.scopes?.length ?? 0) > 3 ? (
                    <span className="muted">等 {key.scopes?.length} 项权限</span>
                  ) : null}
                  {key.models_allowed?.length ? (
                    <>
                      {key.models_allowed.slice(0, 3).map((id) => (
                        <span key={id} className="key-model-id">
                          <span className="badge badge-primary mono">
                            {/* 白名单里的模型也是实体：只写 id 时，用户得自己回忆这串是哪个模型。
                                文本仍用 id（badge 里是"能填进请求里的那个值"），只补图标。 */}
                            <EntityCell
                              iconUrl={modelByID.get(id)?.icon_url}
                              name={id}
                              color={modelByID.get(id)?.vendor_color}
                              size="xs"
                            />
                          </span>
                          {/* 一张卡已有 3 个 badge + 模态 + 限速，标签只留 1 个，其余折成 +N */}
                          <ModelTagList tags={modelByID.get(id)?.tags} max={1} />
                        </span>
                      ))}
                      {key.models_allowed.length > 3 ? (
                        <span className="muted">等 {key.models_allowed.length} 个</span>
                      ) : null}
                    </>
                  ) : null}
                  {key.modalities_allowed?.length
                    ? key.modalities_allowed.map((m) => <ModalityTag key={m} modality={m} />)
                    : null}
                  {!key.models_allowed?.length && !key.modalities_allowed?.length ? (
                    <span className="muted">全部类型</span>
                  ) : null}
                  <span>{key.rpm_limit ? `每分钟 ${key.rpm_limit} 次` : "不限速"}</span>
                  <span>{key.concurrency_limit ? `并发 ${key.concurrency_limit}` : "并发不限"}</span>
                  <span>
                    {key.expired_at
                      ? // 托管密钥是「闲置过期」：活跃使用会自动续期，所以措辞是"有效期至"而不是"过期"——
                        // 写成"过期"会让用户以为到期必断，去手工轮换（其实不必要）。
                        `${hosted ? "有效期至" : "过期"} ${key.expired_at.slice(0, 10)}${hosted ? "（活跃自动续期）" : ""}`
                      : "永不过期"}
                  </span>
                  {key.allow_ips?.length ? <span>IP 白名单 {key.allow_ips.length} 条</span> : null}
                  <span>{key.allow_user_quota_query ? "可查余额" : "隐藏余额"}</span>
                  <span>创建于 {formatDateTime(key.created_at)}</span>
                </div>
              </div>
              <div className="kc-quota">
                <div className="progress">
                  <i className={barTone} style={{ width: `${usedPct}%` }} />
                </div>
                <div className="small muted">
                  已用 {moneyOf(key.used)} / {key.amount_limit ? moneyOf(key.amount_limit) : "不限"}
                </div>
              </div>
              <div className="kc-acts">
                {hosted ? (
                  <>
                    {/* 只有"换钥有意义"时才给按钮：启用中，或闲置超期（轮换即恢复）。
                        平台封禁的钥轮换也不会解除，摆一个注定失败的按钮只会让人
                        反复点、反复看到失败。 */}
                    {key.status === 1 || key.disabled_reason === "expired" ? (
                      <button
                        type="button"
                        className="btn btn-sm"
                        // 没有 app_slug 就轮换不了（端点按 slug 寻址）。正常情况下
                        // 它随列表直出，不可点只可能是列表接口降级。
                        disabled={!key.app_slug}
                        onClick={() => void askRotate(key)}
                      >
                        轮换
                      </button>
                    ) : null}
                    {key.status !== 1 ? (
                      <span className="muted">
                        {key.disabled_reason === "expired"
                          ? "长期未使用已停用"
                          : key.disabled_reason === "admin_block"
                            ? "已被平台封禁"
                            : "已停用"}
                        <Tip
                          text={
                            key.disabled_reason === "expired"
                              ? "超过有效期没有任何调用。点「轮换」即可重新启用。"
                              : key.disabled_reason === "admin_block"
                                ? "由平台管理员操作，轮换不会解除，需联系平台。"
                                : "状态异常，请联系平台。"
                          }
                        />
                      </span>
                    ) : null}
                  </>
                ) : (
                  <>
                    <button type="button" className="btn btn-sm" onClick={() => openEdit(key)}>
                      编辑
                    </button>
                    <button type="button" className="btn btn-sm" onClick={() => void askReset(key)}>
                      重置
                    </button>
                    {key.status === 1 ? (
                      <button type="button" className="btn btn-sm btn-ghost-danger" onClick={() => void askToggle(key)}>
                        禁用
                      </button>
                    ) : (
                      <button type="button" className="btn btn-sm" onClick={() => void askToggle(key)}>
                        启用
                      </button>
                    )}
                    <button type="button" className="btn btn-sm btn-ghost-danger" onClick={() => void askDelete(key)}>
                      删除
                    </button>
                  </>
                )}
              </div>
            </div>
          );
        })
      )}

      {/* 创建 / 编辑弹窗 */}
      <Dialog
        open={formOpen}
        onClose={closeForm}
        size="wide"
        title={editing ? "编辑密钥" : "创建密钥"}
        actions={[
          { label: "取消", onClick: closeForm },
          {
            label: "保存",
            variant: "primary",
            disabled: createMut.isPending || updateMut.isPending,
            onClick: submitForm,
          },
        ]}
      >
        {editing ? (
          <Field label="密钥前缀（不可变）" htmlFor="k-prefix">
            <input id="k-prefix" type="text" readOnly value={`${editing.key_prefix}（创建后不可变）`} />
          </Field>
        ) : null}

        <div className="form-grid">
          <Field
            label="名称"
            req
            htmlFor="k-name"
            hint={`最多 ${NAME_MAX} 字（${form.name.trim().length}/${NAME_MAX}）`}
          >
            <input
              id="k-name"
              type="text"
              maxLength={NAME_MAX}
              placeholder="例如：灵动视频应用"
              value={form.name}
              onChange={(e) => setField("name", e.target.value)}
            />
          </Field>
          <Field
            label="分组"
            tip="分组由账号决定，密钥不能单独设置"
            htmlFor="k-group"
          >
            {/* 只读展示：值来自账号（me.group），不接受编辑。
                之前是下拉 —— 即使选项只有自己那一档，值也会随建 KEY 时的
                账号分组被"钉死"，管理员之后改分组对存量 KEY 不生效。 */}
            <input
              id="k-group"
              type="text"
              readOnly
              value={`${myGroup} · ${groupDiscountText(myDiscount)}`}
            />
          </Field>
          <Field
            label={`额度上限（${currentSymbol()}，0 = 不限）`}
            tip="该密钥累计消费超出上限后，其调用会被拒绝"
            htmlFor="k-quota"
          >
            <input
              id="k-quota"
              type="text"
              inputMode="decimal"
              placeholder="0"
              value={form.amountLimit}
              onChange={(e) => setField("amountLimit", e.target.value)}
            />
          </Field>
          <Field label="过期时间" tip="不填则永不过期" htmlFor="k-exp">
            <input
              id="k-exp"
              type="date"
              min={new Date().toISOString().slice(0, 10)}
              value={form.expiredAt}
              onChange={(e) => setField("expiredAt", e.target.value)}
            />
          </Field>
        </div>

        <Field label="模态白名单" tip="不勾选时可用全部模态">
          <SegmentedChips
            items={MODALITIES.map((m) => ({ key: m, label: modalityLabel(m) }))}
            multiple
            values={form.modalities}
            onChange={(key) => toggleModality(key as Modality)}
          />
        </Field>

        <Field label="模型白名单" tip="留空不限制；建议从本分组可用模型中挑选" htmlFor="k-models">
          <div className="tag-wrap">
            <div className="tag-input">
              {form.models.map((id) => (
                <span key={id} className="tg">
                  <EntityIcon
                    url={modelByID.get(id)?.icon_url ?? ""}
                    name={id}
                    color={modelByID.get(id)?.vendor_color}
                    size="xs"
                  />
                  {id}
                  <button type="button" className="btn-text" aria-label={`移除模型 ${id}`} onClick={() => removeModel(id)}>
                    ✕
                  </button>
                </span>
              ))}
              <input
                id="k-models"
                type="text"
                placeholder={form.models.length ? "继续搜索添加…" : "搜索模型，留空不限制"}
                autoComplete="off"
                value={modelKw}
                onFocus={() => setSugOpen(true)}
                onBlur={() => setSugOpen(false)}
                onChange={(e) => {
                  setModelKw(e.target.value);
                  setSugOpen(true);
                }}
              />
            </div>
            {sugOpen && modelSuggestions.length > 0 ? (
              <div className="tag-sug open">
                {modelSuggestions.map((m) => (
                  <a
                    key={m.id}
                    href="#k-models"
                    onClick={(e) => e.preventDefault()}
                    onMouseDown={(e) => {
                      e.preventDefault();
                      addModel(m.id);
                    }}
                  >
                    <EntityCell iconUrl={m.icon_url} name={m.id} color={m.vendor_color} size="xs">
                      <span className="muted small">· {m.display_name}</span>
                    </EntityCell>
                    {/* 选模型时正是最需要看卖点的场景 */}
                    <ModelTagList tags={m.tags} max={2} />
                  </a>
                ))}
              </div>
            ) : null}
          </div>
        </Field>

        <div className="form-grid">
          <Field label="每分钟请求数（0 = 不限）" htmlFor="k-rpm">
            <input
              id="k-rpm"
              type="number"
              min={0}
              value={form.rpm}
              onChange={(e) => setField("rpm", e.target.value)}
            />
          </Field>
          <Field label="并发上限（0 = 不限）" htmlFor="k-cc">
            <input
              id="k-cc"
              type="number"
              min={0}
              value={form.cc}
              onChange={(e) => setField("cc", e.target.value)}
            />
          </Field>
        </div>

        <Field label="IP 白名单" tip="每行一个 IP 或 CIDR 段，如 203.0.113.0/24；留空不限制" htmlFor="k-ip">
          <textarea
            id="k-ip"
            rows={3}
            placeholder={"203.0.113.0/24"}
            value={form.ips}
            onChange={(e) => setField("ips", e.target.value)}
          />
        </Field>

        <SwitchRow
          label="允许密钥查询余额"
          checked={form.allowQuery}
          onChange={(v) => setField("allowQuery", v)}
        />
      </Dialog>

      {/* 接入应用：用应用标识换取托管密钥（明文只回一次） */}
      <Dialog
        open={issueOpen}
        onClose={() => setIssueOpen(false)}
        title="接入应用"
        actions={[
          { label: "取消", onClick: () => setIssueOpen(false) },
          {
            label: issueMut.isPending ? "换取中…" : "换取密钥",
            variant: "primary",
            disabled: issueMut.isPending || !issueSlug.trim(),
            onClick: () => void submitIssue(),
          },
        ]}
      >
        <p className="dl-lead">
          填写应用方给你的应用标识。换到的托管密钥只对该应用有效，调用都会算在这个应用名下；明文只显示一次。
        </p>
        <Field label="应用标识" htmlFor="issue-slug">
          <Input
            id="issue-slug"
            value={issueSlug}
            onChange={(e) => setIssueSlug(e.target.value)}
            placeholder="例如 default"
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
      </Dialog>

      {/* 一次性明文展示 */}
      <Dialog
        open={secret !== null}
        onClose={() => setSecret(null)}
        title={secret?.hosted ? "保存托管密钥" : "保存你的 API 密钥"}
        footer={
          <button type="button" className="btn btn-primary" onClick={() => setSecret(null)}>
            我已妥善保存
          </button>
        }
      >
        {secret ? (
          <div className="key-reveal">
            <div className="kt">
              <Icon name="key" size={15} />
              {secret.name} 的密钥
            </div>
            <div className="kv">
              <span>{secret.value}</span>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => void copyToast(secret.value)}
              >
                <Icon name="copy" size={15} />
                复制
              </button>
            </div>
            <div className="warn-text">
              {secret.hosted
                ? "只显示一次，请立即保存。托管密钥无法回显，丢失后只能在密钥卡片上轮换生成新密钥。"
                : "只显示一次，请立即保存。关闭后无法再次查看完整密钥，可随时重置生成新密钥。"}
            </div>
          </div>
        ) : null}
      </Dialog>
    </>
  );
}

/** 空表单（分组默认取首个可用分组） */
function emptyForm(group: string): KeyFormState {
  return {
    name: "",
    group,
    amountLimit: "0",
    expiredAt: "",
    models: [],
    modalities: [],
    ips: "",
    rpm: "60",
    cc: "5",
    allowQuery: true,
  };
}

/** 由已有密钥回填表单（微 → 金额文本） */
function formFromKey(key: ApiKeyItem): KeyFormState {
  return {
    name: key.name,
    group: key.group || "default",
    amountLimit: key.amount_limit ? microToInput(key.amount_limit) : "0",
    expiredAt: key.expired_at ? key.expired_at.slice(0, 10) : "",
    models: key.models_allowed ? [...key.models_allowed] : [],
    modalities: key.modalities_allowed ? [...key.modalities_allowed] : [],
    ips: (key.allow_ips ?? []).join("\n"),
    rpm: String(key.rpm_limit),
    cc: String(key.concurrency_limit),
    allowQuery: key.allow_user_quota_query,
  };
}
