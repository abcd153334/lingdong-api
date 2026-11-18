import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";

import { ModelTagsField } from "@/components/admin/ModelTagsField";
import { Dialog } from "@/components/common/Dialog";
import { EntitySelect } from "@/components/common/EntitySelect";
import { Field } from "@/components/common/Field";
import { IconNameField } from "@/components/common/IconNameField";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import {
  useCreateAdminModelMutation,
  useDeleteAdminModelMutation,
  useUpdateAdminModelMutation,
} from "@/lib/api";
import { toastApiError } from "@/lib/apiToast";
import { MODALITY_LABELS } from "@/lib/format";
import { currentSymbol, microOf, microToInput } from "@/lib/money";
import { MAX_IMAGE_N } from "@/lib/quantity";
import { FALLBACK_IMAGE_TIERS, FALLBACK_VIDEO_TIERS } from "@/lib/specTiers";
import { useSchemaCatalogQuery } from "@/lib/api/schema";
import {
  buildSpecPrices,
  emptySpec,
  parseMoneyFields,
  specIsFree,
  tierRank,
  type SpecForm,
} from "@/lib/specForm";
import type {
  AdminModelItem,
  AdminModelPayload,
  BillingMode,
  Modality,
  SpecPrices,
  TagColorOption,
  VendorItem,
} from "@/types";

import {
  PRICE_LANES,
  assignPrice,
  billingModeOptions,
  emptyForm,
  formFromModel,
  intOrZero,
  modalitySpecKind,
  type ModelForm,
  type PriceLane,
} from "./modelForm";

/**
 * 模型新建 / 编辑弹窗。
 *
 * 三处与旧模型页不同（合并成一个页面带来的必然结果）：
 *  1. `initial_binding`：从某条渠道里新建模型时，同一事务里就把它绑到这条渠道 ——
 *     否则"建完还要回列表找它再绑一次"，而那份列表是按渠道过滤的，新模型根本不在里面；
 *  2. 价格告警通过 `onSaved` 交给右栏展示（弹窗关了也要能看到，否则运营改不动）；
 *  3. 表单态在 `open` false→true 时按 `model` 重建，保存后列表失效重取不会重置草稿。
 */
export function ModelDialog({
  open,
  model,
  channel,
  vendors,
  tagPalette,
  onClose,
  onSaved,
  onDeleted,
}: {
  open: boolean;
  /** null = 新建 */
  model: AdminModelItem | null;
  /** 当前选中的渠道（新建时自动绑它；编辑时只用于文案） */
  channel: { id: number; name: string } | null;
  vendors: VendorItem[];
  /** 营销标签色板（随模型列表一起下发） */
  tagPalette: TagColorOption[];
  onClose: () => void;
  /** 保存成功：把后端的价格告警交给页面 */
  onSaved: (warnings: string[]) => void;
  /** 删除成功后回到列表（渠道上下文不变） */
  onDeleted: () => void;
}) {
  const editingId = model?.id ?? null;
  const symbol = currentSymbol();
  const vendorMap = useMemo(() => new Map(vendors.map((v) => [v.id, v] as const)), [vendors]);

  const [form, setForm] = useState<ModelForm>(() => emptyForm(""));
  const [newTier, setNewTier] = useState<{ which: "" | "tier_price" | "second_price"; value: string }>({
    which: "",
    value: "",
  });
  /** 新建时的那一条绑定（编辑态不用它 —— 绑定在列表行内单独改）。 */
  const [binding, setBinding] = useState({ upstream: "", weight: "1" });

  // 打开时按目标重建。用 ref 记住"为谁打开"，不把 model 放进依赖：
  // 列表失效会换掉对象引用，依赖它会在编辑中途重置草稿。
  const openedFor = useRef("");
  useEffect(() => {
    if (!open) {
      openedFor.current = "";
      return;
    }
    const key = model ? `e:${model.id}` : "n";
    if (openedFor.current === key) return;
    openedFor.current = key;
    setForm(model ? formFromModel(model) : emptyForm(vendors[0]?.id ?? ""));
    setNewTier({ which: "", value: "" });
    setBinding({ upstream: "", weight: "1" });
  }, [open, model, vendors]);

  const createModel = useCreateAdminModelMutation();
  const updateModel = useUpdateAdminModelMutation();
  const deleteModel = useDeleteAdminModelMutation();

  // 档位**建议词表**（新建时的预填顺序），不是白名单：档位由运营自由配置，
  // 平台不判定上游能不能出。目录拉不到时回落到本地常量 —— 价目表不能因此整块消失。
  const catalogQuery = useSchemaCatalogQuery();
  const tierKeys = catalogQuery.data?.spec_prices?.tier_keys;
  const suggestVideoTiers = tierKeys?.video?.length ? tierKeys.video : FALLBACK_VIDEO_TIERS;
  const suggestImageTiers = tierKeys?.image?.length ? tierKeys.image : FALLBACK_IMAGE_TIERS;


  /**
   * 档位清单 = 建议词表 ∪ 已配的键。
   *
   * 必须并上已配键：档位由运营自由配置（8K / 2.5K / 1440p…），只渲染建议词表会让
   * 自定义档位在页面上"看不见"，而 `spec_prices` 是整块覆盖写 —— 保存一次就把它悄悄删了。
   */
  const unionTiers = (suggest: string[], prices: Record<string, string>) => {
    const out = [...suggest];
    for (const k of Object.keys(prices)) if (!out.includes(k)) out.push(k);
    return out;
  };
  const imageTiers = unionTiers(suggestImageTiers, form.spec.tier_price);
  const videoTiers = unionTiers(suggestVideoTiers, form.spec.second_price);

  const onModalityChange = (mod: Modality) => {
    setForm((f) => {
      const opts = billingModeOptions(mod);
      const valid = opts.some(([v]) => v === f.billing_mode);
      // 按规格计费的三种模态（图像/视频/音乐）各自有**结构不同**的价目表，
      // 切模态时必须换成对应结构：把 per_image 的矩阵留给 music，保存时会被
      // buildSpec 按 per_task 校验而拦住，运营看到的是"明明填了价却报缺价"。
      const specModality = mod === "image" || mod === "video" || mod === "music";
      return {
        ...f,
        modality: mod,
        billing_mode: valid ? f.billing_mode : opts[0][0],
        spec: specModality ? emptySpec(modalitySpecKind(mod)) : f.spec,
      };
    });
  };

  const setPrice = (key: PriceLane, raw: string) =>
    setForm((f) => ({ ...f, prices: { ...f.prices, [key]: raw } }));

  const setSpecTier = (which: "tier_price" | "second_price" | "ref_video_second_price", tier: string, raw: string) =>
    setForm((f) => {
      const map = { ...f.spec[which] };
      if (raw.trim() === "") delete map[tier];
      else map[tier] = raw;
      return { ...f, spec: { ...f.spec, [which]: map } };
    });

  /**
   * 新增一个档位键（值为空 = 暂不定价，保存时被 moneyMap 跳过）。
   *
   * 为什么允许任意键：档位是**运营**决定卖什么，代码不设白名单；
   * 只给固定几行等于"代码决定这个模型能卖哪几档"。
   */
  const addSpecTier = (which: "tier_price" | "second_price", raw: string) => {
    const t = raw.trim();
    if (!t) return;
    setForm((f) => {
      if (t in f.spec[which]) return f;
      return { ...f, spec: { ...f.spec, [which]: { ...f.spec[which], [t]: "" } } };
    });
    setNewTier({ which: "", value: "" });
  };

  const setSpecField = (key: keyof SpecForm, raw: string) =>
    setForm((f) => ({ ...f, spec: { ...f.spec, [key]: raw } }));

  /** 规格矩阵 → spec_prices（含校验） */
  // 是否已切换到「参考视频按秒」口径（任一档填了价即算切换）
  const hasRefVideoSecond = Object.values(form.spec.ref_video_second_price).some((v) => v.trim() !== "");
  // 超时阈值与倍率只填了一个（不生效，必须就地提示）
  const onlyOneLongField =
    (form.spec.long_after_seconds.trim() === "") !== (form.spec.long_video_multiplier.trim() === "");
  // 存量按次单价（音乐/歌词的老口径）：有值时说明这个模型还没切到按条结算。
  const legacyPerTask = form.spec.per_task_price.trim();
  // 固定档位指向一个还没填单价的档位：保存时会被拦，这里提前就地告警
  const fixedTierName = form.spec.resolution_fixed.trim();
  const fixedTierUnpriced =
    fixedTierName !== "" && (form.spec.second_price[fixedTierName] ?? "").trim() === "";

  /**
   * 表单 → spec_prices。
   *
   * 真正的实现在 `@/lib/specForm`（纯函数、可做往返测试）：
   * 这里只负责把错误翻成 toast —— 合法性判定不该和 UI 混在一起，
   * 否则"页面里能过、库里存错"这类问题永远测不出来。
   */
  const buildSpec = (): SpecPrices | null => {
    const r = buildSpecPrices(form.spec, form.modality);
    if (!r.ok) {
      toast.error(r.error);
      return null;
    }
    return r.value;
  };

  /**
   * 编辑态下「改了计费方式」的即时提示（空串 = 不需要提示）。
   *
   * 后端只按 billing_mode 落一套价目，另一套会在保存时被清空 —— 不在表单里
   * 提前说清，运营保存后发现刚填好的档位价"消失"，第一反应就是平台丢数据。
   * 反过来（改成 spec）也一样：按量的 8 条通道价不会平移成档位价。
   */
  const modeSwitchHint = (() => {
    if (!editingId) return "";
    const original = model?.billing_mode;
    if (!original || original === form.billing_mode) return "";
    if (original === "spec") {
      return "保存后将清除该模型的按规格价目（档位价 / 秒价 / 张数上限）；改回按规格需要重新填写";
    }
    if (form.billing_mode === "spec") {
      return "保存后该模型改为按规格计费，原来的按量 / 按次价格会作废";
    }
    return "保存后价格需按新计费方式重新填写，原来的价格不会沿用";
  })();

  /**
   * 张数上限的提示文案。
   *
   * 两条互相冲突的约束必须在这里同时可见，否则运营只会看到"留空 = 128 张"，
   * 然后把一个支持图层拆分的模型的上限填成 4 —— 保存成功、启用成功，
   * 直到用户第一次请求图层拆分才 400（`imageCountHint=17 > max_n`），
   * 而那时报错信息离「这里少填了一个数字」已经很远了。
   *
   * 判据：模型（编辑态）声明了 image_layer 能力时才提示。
   * 新建时拿不到能力声明（它由后端预设/接口写入，本页不编辑），
   * 所以此时不提示而不是乱猜。
   */
  /**
   * 默认档（`resolution_fixed`）与已填档位的关系（只提示、不拦）。
   *
   * 默认档决定「不传 size / auto」时按哪一档计费，是运营最需要看见的一件事。
   * 判据来自**当前正在编辑的这份表单**，与保存期校验同一条：非空必须命中已配档位。
   */
  const defaultTierHint = (() => {
    const empty = { text: "", warn: false };
    if (!form.spec || form.spec.kind !== "per_image") return empty;
    const tiers = Object.entries(form.spec.tier_price)
      .map(([tier, raw]) => {
        // 档位价是 money 量纲：直接按同一套金额解析取值（非法/为空的不参与提示）
        const v = microOf(raw.trim());
        return v === null ? null : ([tier, v] as [string, number]);
      })
      .filter((x): x is [string, number] => x !== null)
      .sort((a, b) => tierRank(a[0]) - tierRank(b[0]));
    if (tiers.length === 0) return empty;
    const highest = tiers[tiers.length - 1];
    const def = form.spec.resolution_fixed.trim();
    if (!def) {
      return {
        text: `未设置默认档：不传 size 的请求会被拒绝；写价目外的档位按最高档 ${highest[0]} 计费`,
        warn: true,
      };
    }
    if (!(def in form.spec.tier_price)) {
      return { text: `默认档 ${def} 已不在价目里，请重新选择`, warn: true };
    }
    const unit = tiers.find((t) => t[0] === def)?.[1];
    return {
      text: `默认档 ${def}（${unit === undefined ? "未定价" : microToInput(unit)}）：不传 size / auto 按它计费；写价目外的档位按最高档 ${highest[0]} 计费`,
      warn: false,
    };
  })();

  const maxNHint = (() => {
    const base = `必填（1–${MAX_IMAGE_N}）。决定单次生成的最坏金额与结算上限；超出该上限的部分不向用户收费`;
    const caps = editingId ? (model?.capabilities ?? []) : [];
    if (!caps.includes("image_layer")) return base;
    const raw = form.spec.max_n.trim();
    if (raw === "") return `该模型支持图层拆分，上限需 ≥ 17（1 张底图 + 16 张图层）`;
    const n = Number(raw);
    if (Number.isFinite(n) && n > 0 && n < 17) {
      return `该模型支持图层拆分（image_layer）：上限填 ${raw} 会让带 layer_decomposition 的请求被判「张数超限」，请改成 ≥ 17`;
    }
    return base;
  })();

  const submitModel = async () => {
    const id = form.id.trim();
    if (!id) {
      toast.error("模型 ID 必填");
      return;
    }
    if (!form.vendor) {
      toast.error("请选择供应商分组");
      return;
    }
    // 窗口覆盖必须显式发 0（= 继承）：发 undefined 等于"不改"，
    // 于是"清空输入框再保存"永远清不掉已经填过的值。
    const ctxWindow = intOrZero(form.context_window);
    const maxOut = intOrZero(form.max_output_tokens);
    if (Number.isNaN(ctxWindow) || ctxWindow < 0) {
      toast.error("上下文窗口必须是正整数（留空 = 继承助手全局设置）");
      return;
    }
    if (Number.isNaN(maxOut) || maxOut < 0) {
      toast.error("输出上限必须是正整数（留空 = 继承助手全局设置）");
      return;
    }
    const body: AdminModelPayload = {
      id,
      display_name: form.display_name.trim(),
      // 恒提交（含空串）：空串 = 显式清空（回到沿用供应商图标）。
      // 用 undefined 省略，运营就永远删不掉已设的图标。
      icon: form.icon_own,
      description: form.description.trim(),
      vendor: form.vendor,
      modality: form.modality,
      billing_mode: form.billing_mode,
      enabled: form.enabled,
      context_window: ctxWindow,
      max_output_tokens: maxOut,
      // 恒提交（含 []）：省略 = 不改，运营就永远清不掉标签。
      // 空文案行只是"点了添加还没填"，直接丢弃（后端也跳过）。
      tags: form.tags
        .filter((t) => t.label.trim())
        .map((t) => ({
          label: t.label.trim(),
          color: t.color,
          ...(t.untilDate ? { until: t.untilDate } : {}),
        })),
    };
    // 编辑时带版本号做乐观锁：两个管理员同时改价，后写的那次会 409 而不是静默覆盖。
    // 新建没有版本（后端 version 从 1 起）。
    if (editingId) {
      const v = model?.version;
      if (typeof v === "number") body.version = v;
    } else if (channel) {
      // 从某条渠道里新建：同一事务里绑上它。
      // 上游名留空 = 与模型同名（后端 normalizeBindings 的既有规则），
      // 这里**不填默认值**：上游改名是少数情况，预填一串同样的字符只会让人以为要改。
      body.initial_binding = {
        channel_id: channel.id,
        // 留空 = 与模型 ID 同名（后端不猜，空值 400）
        upstream_model: binding.upstream.trim() || id,
        weight: Number(binding.weight) || 1,
        enabled: true,
      };
    }

    if (form.billing_mode === "tokens" || form.billing_mode === "call") {
      const lanes = form.billing_mode === "tokens" ? PRICE_LANES : [{ key: "call" as PriceLane, label: "每次价" }];
      const parsed = parseMoneyFields(lanes.map((l) => ({ key: l.key, label: l.label, raw: form.prices[l.key] })));
      if (!parsed.ok) {
        toast.error(parsed.error);
        return;
      }
      const input = parsed.values.input;

      if (form.billing_mode === "tokens" && input == null) {
        toast.error(`按用量计费必须填写输入价（${symbol} / 1M tokens）`);
        return;
      }
      if (form.billing_mode === "call" && parsed.values.call == null) {
        toast.error(`按次计费必须填写每次价（${symbol} / 次）`);
        return;
      }
      // 半免费：输入价是 0、其它通道有价。后端也会拒（这条规则只写了一遍，
      // 前端提前说清是为了不让运营填完整张表才看到 400）。
      if (input === 0) {
        const otherPaid = lanes.some((l) => l.key !== "input" && (parsed.values[l.key] ?? 0) !== 0);
        if (otherPaid) {
          toast.error("输入价为 0 时其它通道必须也为 0（半免费账单无法解释；要免费请全部填 0）");
          return;
        }
      }
      // 0 价必须二次确认：0 不是"未配置"（未配置是空，且按 token 计费会被拒），
      // 而是明确的「免费」。
      const zeroKey = form.billing_mode === "tokens" ? input === 0 : parsed.values.call === 0;
      if (zeroKey) {
        const ok = await confirmDialog({
          title: "确认 0 价",
          body: (
            <>
              该模型将被设为<b>完全免费</b>（调用量再大也不产生任何扣费）。
              <br />
              如果只是想暂时下线模型，应改用「停用」而不是把价格清零。
            </>
          ),
          okText: "确认为免费",
          okVariant: "danger",
        });
        if (!ok) return;
      }

      for (const l of lanes) {
        assignPrice(body, l.key, parsed.values[l.key] ?? null);
      }
      // 编辑时把"原本有价、现在被清空"的通道显式告诉后端。
      // 否则 nil 会被 FillMissing 当成"本次没提交"，用库内旧价补回去 ——
      // 表现就是"删掉的价格保存后又回来了"。
      if (editingId) {
        const original = model;
        const cleared: string[] = [];
        for (const l of lanes) {
          const had = original?.prices?.[l.key] != null;
          if (had && parsed.values[l.key] == null) cleared.push(l.key);
        }
        if (cleared.length > 0) body.clear_prices = cleared;
      }
    } else {
      const spec = buildSpec();
      if (!spec) return;
      // 0 价必须二次确认（与 tokens/call 同一口径）。
      //
      // tokens/call 早就有这道确认，规格计费一直没有 —— 一条全 0 的档目等于把该模型
      // 白送出去，而账单上只会显示「已结算 0 元」，事后无从分辨是策略还是手滑。
      if (specIsFree(spec)) {
        const ok = await confirmDialog({
          title: "确认 0 价",
          body: (
            <>
              该模型的价目<b>全部为 0</b>，将被设为<b>完全免费</b>（调用量再大也不产生任何扣费）。
              <br />
              如果只是想暂时下线模型，应改用「停用」而不是把价格清零。
            </>
          ),
          okText: "确认为免费",
          okVariant: "danger",
        });
        if (!ok) return;
      }
      body.spec_prices = spec;
    }

    const onSuccess = (res: { item: AdminModelItem; warnings?: string[] }) => {
      const warns = res?.warnings ?? [];
      toast(editingId ? "模型已保存" : "模型已创建");
      // 告警展示在**页面级**（列表上方）：留在弹窗里会让「每次保存都关不掉」，
      // 而视频模型常见的「未填参考图单价」几乎每次保存都会命中。
      onSaved(warns);
      onClose();
    };
    if (editingId) updateModel.mutate({ id: editingId, body }, { onSuccess, onError: (e) => toastApiError(e) });
    else createModel.mutate(body, { onSuccess, onError: (e) => toastApiError(e) });
  };
  /** 删除模型：被绑定或作为默认模型时后端会拒（409），这里只做二次确认。 */
  const askDelete = async () => {
    if (!editingId) return;
    const ok = await confirmDialog({
      title: "删除模型",
      body: (
        <>
          确定删除模型 <b>{editingId}</b>？已被渠道绑定或作为默认模型的不可删除。
        </>
      ),
      okText: "确认删除",
      okVariant: "danger",
    });
    if (!ok) return;
    deleteModel.mutate(editingId, {
      onSuccess: () => {
        toast(`模型 ${editingId} 已删除`);
        onDeleted();
      },
      onError: (e) => toastApiError(e),
    });
  };

  return (
      <Dialog
        open={open}
        onClose={onClose}
        title={editingId ? `编辑模型 ${editingId}` : "新建模型"}
        size="wide"
        actions={[
          // 删除放在这里而不是行内：行操作已有 4 个（RowActions 上限 4），
          // 再塞一个会被裁掉或把动作列撑宽。编辑弹窗是"要不要这个模型"的决策现场。
          ...(editingId
            ? [
                {
                  label: "删除模型",
                  onClick: () => void askDelete(),
                  disabled: deleteModel.isPending,
                },
              ]
            : []),
          { label: "取消", onClick: onClose },
          {
            label: "保存",
            variant: "primary" as const,
            disabled: createModel.isPending || updateModel.isPending,
            onClick: submitModel,
          },
        ]}
      >
        <div className="form-grid">
          {/* 身份区：图标 + 名称同一行（左图右文），模型 ID 与供应商随后。
             不用 tip/hint：UI 契约门禁对 Tip/Hint 有基线棘轮，这里靠 placeholder 表达即可。 */}
          <Field label="名称" htmlFor="mf-name" span>
            <IconNameField
              id="mf-name"
              icon={form.icon_own}
              iconUrl={form.iconUrl}
              onIconChange={(v) => setForm((f) => ({ ...f, icon_own: v }))}
              fallback={vendorMap.get(form.vendor)?.icon}
              fallbackUrl={vendorMap.get(form.vendor)?.icon_url}
              fallbackLabel="沿用供应商"
              color={vendorMap.get(form.vendor)?.color}
              initialFrom={form.id}
              text={form.display_name}
              onTextChange={(v) => setForm((f) => ({ ...f, display_name: v }))}
              placeholder="留空则回落为模型 ID"
              maxLength={30}
            />
          </Field>
          <Field label="模型 ID" req htmlFor="mf-id">
            <input
              id="mf-id"
              type="text"
              className="mono"
              placeholder="provider-model-name"
              readOnly={editingId !== null}
              value={form.id}
              onChange={(e) => setForm((f) => ({ ...f, id: e.target.value }))}
            />
          </Field>
          <Field label="供应商" req htmlFor="mf-vendor">
            <EntitySelect
              id="mf-vendor"
              ariaLabel="供应商"
              value={form.vendor}
              placeholder="请选择供应商"
              options={vendors.map((v) => ({ value: v.id, label: v.name, iconUrl: v.icon_url, color: v.color }))}
              onChange={(v) => setForm((f) => ({ ...f, vendor: v }))}
            />
          </Field>
          <Field label="模态" htmlFor="mf-mod">
            <Select
              id="mf-mod"
              items={Object.entries(MODALITY_LABELS) as [Modality, string][]}
              value={form.modality}
              onChange={(e) => onModalityChange(e.target.value as Modality)}
            />
          </Field>
          <Field
            label="计费方式"
            htmlFor="mf-mode"
            hint={
              modeSwitchHint ||
              "模态决定了可选的计费方式：图像/视频/音乐只能按规格，语音只能按次或按规格"
            }
          >
            <Select
              id="mf-mode"
              items={billingModeOptions(form.modality)}
              value={form.billing_mode}
              onChange={(e) => setForm((f) => ({ ...f, billing_mode: e.target.value as BillingMode }))}
            />
          </Field>
          <Field label="描述" htmlFor="mf-desc" span>
            <textarea
              id="mf-desc"
              rows={2}
              maxLength={200}
              placeholder="例如：出图更快、单价更高"
              value={form.description}
              onChange={(e) => setForm((f) => ({ ...f, description: e.target.value }))}
            />
          </Field>
          <Field label="营销标签" span>
            {/* key 绑到当前编辑对象：弹窗常驻（不随 open 卸载），换模型时必须重挂，
                否则上一个模型没提交的「待添加」输入会带过来。 */}
            <ModelTagsField
              key={editingId ?? "new"}
              value={form.tags}
              onChange={(tags) => setForm((f) => ({ ...f, tags }))}
              palette={tagPalette}
            />
          </Field>
          <Field
            label="上下文窗口（Token）"
            htmlFor="mf-ctxwin"
            tip="该模型真实支持的窗口。留空 = 跟随助手全局设置；填小了接入助手会被上游 400"
          >
            <input
              id="mf-ctxwin"
              type="text"
              inputMode="numeric"
              placeholder="跟随全局"
              value={form.context_window}
              onChange={(e) => setForm((f) => ({ ...f, context_window: e.target.value }))}
              data-testid="mf-ctxwin"
            />
          </Field>
          <Field
            label="单次输出上限（Token）"
            htmlFor="mf-maxout"
            tip="该模型单次最多生成多少 token。留空 = 跟随助手全局设置"
          >
            <input
              id="mf-maxout"
              type="text"
              inputMode="numeric"
              placeholder="跟随全局"
              value={form.max_output_tokens}
              onChange={(e) => setForm((f) => ({ ...f, max_output_tokens: e.target.value }))}
              data-testid="mf-maxout"
            />
          </Field>
        </div>

        {form.billing_mode === "tokens" && (
          <>
            <div className="hint">
              以下 8 条通道按「{symbol} / 百万 Token」填写。
              <b>留空 = 不单独配置</b>（按输入价计）；填 0 = 该通道免费。
              已配置的通道要清除，把输入框清空后保存即可。
            </div>
            <div className="price-grid">
              {PRICE_LANES.map((l) => (
                <Field
                  key={l.key}
                  label={l.key === "input" ? `${l.label}（${symbol} / 1M）` : l.label}
                  htmlFor={`mf-p-${l.key}`}
                  req={l.key === "input"}
                  labelExtra={l.hint ? <span className="hint-inline">{l.hint}</span> : undefined}
                >
                  <input
                    id={`mf-p-${l.key}`}
                    type="text"
                    inputMode="decimal"
                    placeholder={l.key === "input" ? "1.25" : ""}
                    value={form.prices[l.key]}
                    onChange={(e) => setPrice(l.key, e.target.value)}
                  />
                </Field>
              ))}
            </div>
          </>
        )}

        {form.billing_mode === "call" && (
          <Field
            label={`每次价（${symbol} / 次）`}
            req
            htmlFor="mf-call"
            hint="每次成功调用固定扣这么多；失败不扣费"
          >
            <input
              id="mf-call"
              type="text"
              inputMode="decimal"
              placeholder="0.02"
              value={form.prices.call}
              onChange={(e) => setPrice("call", e.target.value)}
            />
          </Field>
        )}

        {form.billing_mode === "spec" && (
          <>
            {form.modality === "video" ? (
              <>
                <Field
                  label={`视频按秒单价（${symbol} / 秒）`}
                  hint="只填要开放的档位：留空 = 不开放该档（请求该档会按兜底档计价，不会拦截）"
                >
                  <div className="price-matrix">
                    <table>
                      <thead>
                        <tr>
                          <th>分辨率档位</th>
                          <th>每秒单价</th>
                        </tr>
                      </thead>
                      <tbody>
                        {videoTiers.map((tier) => (
                          <tr key={tier}>
                            <th>{tier}</th>
                            <td>
                              <input
                                type="text"
                                inputMode="decimal"
                                value={form.spec.second_price[tier] ?? ""}
                                onChange={(e) => setSpecTier("second_price", tier, e.target.value)}
                              />
                            </td>
                          </tr>
                        ))}
                        <tr>
                          <th>
                            <input
                              type="text"
                              placeholder="自定义档位（如 1440p）"
                              aria-label="新增视频自定义档位"
                              value={newTier.which === "second_price" ? newTier.value : ""}
                              onChange={(e) => setNewTier({ which: "second_price", value: e.target.value })}
                              onKeyDown={(e) => {
                                if (e.key === "Enter") {
                                  e.preventDefault();
                                  addSpecTier("second_price", newTier.value);
                                }
                              }}
                            />
                          </th>
                          <td>
                            <button
                              type="button"
                              className="btn btn-sm"
                              disabled={!newTier.value.trim()}
                              onClick={() => addSpecTier("second_price", newTier.value)}
                            >
                              添加档位
                            </button>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </Field>

                <Field
                  label="固定计费档位（可选）"
                  htmlFor="mf-fixedtier"
                  hint={
                    fixedTierUnpriced
                      ? `固定档位 ${fixedTierName} 还没有配置每秒单价，保存会被拒绝：请先在上面的价目表里填上。`
                      : !fixedTierName
                        ? "上游没有 resolution 参数、画质由模型自身决定时填写（例：可灵按 mode 出片）。填写后，「不传 resolution」的请求自动按该档计价；留空则该模型必须由调用方显式传 resolution。"
                        : `已固定为 ${fixedTierName}：调用方不传 resolution 时按该档每秒单价计价；传了不同的档位会被直接拒绝（防止按低价档买高档画质）。`
                  }
                  hintWarn={fixedTierUnpriced}
                >
                  <Select
                    id="mf-fixedtier"
                    value={form.spec.resolution_fixed}
                    onChange={(e) => setSpecField("resolution_fixed", e.target.value)}
                    items={[
                      ["", "不固定（必须由调用方传 resolution）"],
                      ...videoTiers.map((tier): [string, string] => [tier, tier]),
                    ]}
                  />
                </Field>

                <Field
                  label={`参考视频每秒加价（${symbol} / 秒）`}
                  hint="填写后参考视频改为按秒计费；全部留空则沿用下面的按条加价。两套口径互斥。"
                >
                  <div className="price-matrix">
                    <table>
                      <thead>
                        <tr>
                          <th>分辨率档位</th>
                          <th>每秒单价</th>
                        </tr>
                      </thead>
                      <tbody>
                        {videoTiers.map((r) => (
                          <tr key={r}>
                            <th>{r}</th>
                            <td>
                              <input
                                id={`mf-refsec-${r}`}
                                type="text"
                                inputMode="decimal"
                                value={form.spec.ref_video_second_price[r] ?? ""}
                                onChange={(e) => setSpecTier("ref_video_second_price", r, e.target.value)}
                              />
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </Field>

                <div className="form-grid">
                  <Field label={`参考图加价（${symbol} / 张）`} htmlFor="mf-refimg">
                    <input
                      id="mf-refimg"
                      type="text"
                      inputMode="decimal"
                      value={form.spec.ref_image_price}
                      onChange={(e) => setSpecField("ref_image_price", e.target.value)}
                    />
                  </Field>
                  {hasRefVideoSecond ? (
                    <Field
                      label={`参考视频加价（${symbol} / 条）`}
                      htmlFor="mf-refvid"
                      hint="已切换到按秒计费，该字段不再生效"
                      hintWarn
                    >
                      <input id="mf-refvid" type="text" value={form.spec.ref_video_price} disabled />
                    </Field>
                  ) : (
                    <Field label={`参考视频加价（${symbol} / 条）`} htmlFor="mf-refvid">
                      <input
                        id="mf-refvid"
                        type="text"
                        inputMode="decimal"
                        value={form.spec.ref_video_price}
                        onChange={(e) => setSpecField("ref_video_price", e.target.value)}
                      />
                    </Field>
                  )}
                  <Field label={`参考音频加价（${symbol} / 个）`} htmlFor="mf-refaud">
                    <input
                      id="mf-refaud"
                      type="text"
                      inputMode="decimal"
                      value={form.spec.ref_audio_price}
                      onChange={(e) => setSpecField("ref_audio_price", e.target.value)}
                    />
                  </Field>
                  <Field
                    label="参考图免费张数"
                    htmlFor="mf-freeimg"
                    hint="留空或 0 = 无免费（每张都计费）；要全部免费请把单价配成 0"
                  >
                    <input
                      id="mf-freeimg"
                      type="text"
                      inputMode="numeric"
                      value={form.spec.ref_image_free}
                      onChange={(e) => setSpecField("ref_image_free", e.target.value)}
                    />
                  </Field>
                  <Field
                    label="最大秒数"
                    req
                    htmlFor="mf-maxsec"
                    hint="必填。决定单条视频的最坏金额与结算上限；超过该上限的时长按上限收费"
                  >
                    <input
                      id="mf-maxsec"
                      type="text"
                      inputMode="numeric"
                      value={form.spec.max_seconds}
                      onChange={(e) => setSpecField("max_seconds", e.target.value)}
                    />
                  </Field>
                  <SwitchRow
                    id="mf-autodur"
                    label="允许智能时长"
                    tip="开启后才接受 seconds=-1，按上游实际时长结算"
                    checked={form.spec.allow_auto_duration === "true"}
                    onChange={(v) => setSpecField("allow_auto_duration", v ? "true" : "")}
                    testId="model-allow-auto-duration"
                  />
                  <Field
                    label="超时阈值（秒）"
                    htmlFor="mf-longafter"
                    hint="时长超过该值时，时长费 × 倍率"
                    hintWarn={onlyOneLongField}
                  >
                    <input
                      id="mf-longafter"
                      type="text"
                      inputMode="numeric"
                      value={form.spec.long_after_seconds}
                      onChange={(e) => setSpecField("long_after_seconds", e.target.value)}
                    />
                  </Field>
                  <Field
                    label="超时倍率"
                    htmlFor="mf-longmult"
                    hint="按倍数填（1.5 = 加价 50%），必须大于 1；与阈值必须成对填写"
                    hintWarn={onlyOneLongField}
                  >
                    <input
                      id="mf-longmult"
                      type="text"
                      inputMode="decimal"
                      placeholder="1.5"
                      value={form.spec.long_video_multiplier}
                      onChange={(e) => setSpecField("long_video_multiplier", e.target.value)}
                    />
                  </Field>
                </div>
              </>
            ) : form.modality === "music" ? (
              <div className="grid-2">
                <Field
                  label={`每条单价（${symbol} / 条）`}
                  // 存量按次模型不必立刻改成按条：价目里已经有一条合法口径，
                  // 逼着运营现填一个新单价，等于让他去猜一次任务该收多少钱。
                  req={legacyPerTask === ""}
                  htmlFor="mf-perclip"
                  hint={
                    legacyPerTask !== ""
                      ? `按实际交付条数结算。该模型当前仍是按次 ${legacyPerTask} / 次（存量口径），保存会原样保留；填上本栏即切换为按条。`
                      : "按实际交付条数结算：一次出 2 首收 2 条的钱，只出 1 首收 1 条，0 条免单"
                  }
                >
                  <input
                    id="mf-perclip"
                    type="text"
                    inputMode="decimal"
                    value={form.spec.per_clip_price}
                    onChange={(e) => setSpecField("per_clip_price", e.target.value)}
                  />
                </Field>
                <Field
                  label="计费条数上限"
                  htmlFor="mf-clipmax"
                  hint="留空按 2 条（一次任务通常出两首）；歌词模型填 1"
                >
                  <input
                    id="mf-clipmax"
                    type="text"
                    inputMode="numeric"
                    placeholder="2"
                    value={form.spec.max_n}
                    onChange={(e) => setSpecField("max_n", e.target.value)}
                  />
                </Field>
              </div>
            ) : form.modality === "audio" ? (
              <>
                <div className="grid-2">
                  <Field
                    label={`每秒单价（${symbol} / 秒）`}
                    req
                    htmlFor="mf-persec"
                    hint={
                      form.spec.per_second_price.trim() !== "" &&
                      microOf(form.spec.per_second_price) === 0
                        ? "填 0 = 该模型完全免费（调用不产生任何扣费）"
                        : "按生成音频的实际时长计费；时长由上游返回或平台从音频头读取"
                    }
                  >
                    <input
                      id="mf-persec"
                      type="text"
                      inputMode="decimal"
                      placeholder="0.001"
                      value={form.spec.per_second_price}
                      onChange={(e) => setSpecField("per_second_price", e.target.value)}
                    />
                  </Field>
                  <Field
                    label={`最低消费（${symbol} / 次）`}
                    htmlFor="mf-minprice"
                    hint="上游「最低按 x 秒收」时填它：不足按此额收；不需要最低消费就留空"
                  >
                    <input
                      id="mf-minprice"
                      type="text"
                      inputMode="decimal"
                      placeholder="0.01"
                      value={form.spec.min_price}
                      onChange={(e) => setSpecField("min_price", e.target.value)}
                    />
                  </Field>
                </div>
                <Field
                  label="最大计费秒数"
                  req
                  htmlFor="mf-maxsec-audio"
                  hint="必填。决定单次音频的最坏金额与结算上限；ASR 冻结期拿不到时长，全靠它兜底"
                >
                  <input
                    id="mf-maxsec-audio"
                    type="text"
                    inputMode="numeric"
                    value={form.spec.max_seconds}
                    onChange={(e) => setSpecField("max_seconds", e.target.value)}
                  />
                </Field>
              </>
            ) : (
              <>
                <Field
                  label={`图片按张单价（${symbol} / 张）`}
                  hint={defaultTierHint.text || undefined}
                  hintWarn={defaultTierHint.warn}
                >
                  <div className="price-matrix">
                    <table>
                      <thead>
                        <tr>
                          <th>分辨率档位</th>
                          <th>单价</th>
                        </tr>
                      </thead>
                      <tbody>
                        {imageTiers.map((t) => (
                          <tr key={t}>
                            <th>{t}</th>
                            <td>
                              <input
                                type="text"
                                inputMode="decimal"
                                value={form.spec.tier_price[t] ?? ""}
                                onChange={(e) => setSpecTier("tier_price", t, e.target.value)}
                              />
                            </td>
                          </tr>
                        ))}
                        <tr>
                          <th>
                            <input
                              type="text"
                              placeholder="自定义档位（如 8K）"
                              aria-label="新增图片自定义档位"
                              value={newTier.which === "tier_price" ? newTier.value : ""}
                              onChange={(e) => setNewTier({ which: "tier_price", value: e.target.value })}
                              onKeyDown={(e) => {
                                if (e.key === "Enter") {
                                  e.preventDefault();
                                  addSpecTier("tier_price", newTier.value);
                                }
                              }}
                            />
                          </th>
                          <td>
                            <button
                              type="button"
                              className="btn btn-sm"
                              disabled={!newTier.value.trim()}
                              onClick={() => addSpecTier("tier_price", newTier.value)}
                            >
                              添加档位
                            </button>
                          </td>
                        </tr>
                        <tr>
                          <th>默认档（不传 size 时计费）</th>
                          <td>
                            <select
                              aria-label="默认档"
                              value={form.spec.resolution_fixed}
                              onChange={(e) => setSpecField("resolution_fixed", e.target.value)}
                            >
                              <option value="">未设置（不传 size 会被拒绝）</option>
                              {Object.keys(form.spec.tier_price)
                                .filter(Boolean)
                                .sort((a, b) => tierRank(a) - tierRank(b))
                                .map((t) => (
                                  <option key={t} value={t}>
                                    {t}
                                  </option>
                                ))}
                            </select>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </Field>
                <Field
                  label="单次最多张数"
                  req
                  htmlFor="mf-nmax"
                  hint={maxNHint}
                >
                  <input
                    id="mf-nmax"
                    type="text"
                    inputMode="numeric"
                    value={form.spec.max_n}
                    onChange={(e) => setSpecField("max_n", e.target.value)}
                  />
                </Field>
              </>
            )}
            <div className="hint">
              所有单价按结算货币的绝对金额填写（{symbol}）；库里存的是同一笔金额的微单位，
              不存在倍率或刻度换算。
            </div>
          </>
        )}

        {!editingId && channel ? (
          <>
            <div className="blk-title">在「{channel.name}」上创建并绑定</div>
            <div className="form-grid">
              <Field
                label="上游模型名"
                htmlFor="mf-upstream"
                hint="留空 = 与本模型同名。上游叫法不同时才需要填"
              >
                <input
                  id="mf-upstream"
                  type="text"
                  className="mono"
                  placeholder={form.id || "与模型 ID 相同"}
                  value={binding.upstream}
                  onChange={(e) => setBinding((b) => ({ ...b, upstream: e.target.value }))}
                />
              </Field>
              <Field label="权重" htmlFor="mf-weight" hint="同优先级渠道按权重分摊流量">
                <input
                  id="mf-weight"
                  type="number"
                  min={1}
                  value={binding.weight}
                  onChange={(e) => setBinding((b) => ({ ...b, weight: e.target.value }))}
                />
              </Field>
            </div>
          </>
        ) : null}

        <div className="mt16">
          <SwitchRow
            label="启用"
            checked={form.enabled}
            onChange={(checked) => setForm((f) => ({ ...f, enabled: checked }))}
          />
        </div>
      </Dialog>
  );
}
