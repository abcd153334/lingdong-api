import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { useSecondTick } from "@/components/common/Elapsed";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { RowActions, type RowAction } from "@/components/common/RowActions";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Tag } from "@/components/common/StatusTag";
import { Textarea } from "@/components/common/Textarea";
import { ErrorState, LoadingState } from "@/components/common/QueryStates";
import { useAdminChannelKeysQuery, usePatchAdminChannelKeysMutation } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { copyText } from "@/lib/clipboard";
import { MAX_KEYS, firstBlockingIssue, parseKeyLines } from "@/lib/channelKeys";
import { formatDateTime } from "@/lib/format";
import type { ChannelKeyItem, ChannelKeyOp, ChannelKeysResult } from "@/types";

/**
 * 密钥池（编辑渠道抽屉内平铺，不再开二级抽屉）。
 *
 * 四条不可让步的约定：
 *  1. **永不回显明文**：指纹（前6+后4）来自后端，本组件没有任何"看明文"的入口；
 *  2. 所有写操作走同一个 PATCH（一次原子 mutation），不做"先删后加"两次请求
 *     —— 中间态（删了还没加）恰好是线上取不到 key 的时刻；
 *  3. 提交带 `version`：两个管理员同时改时后者拿 409，提示刷新而不是互相覆盖；
 *  4. 输入/草稿只在**成功之后**清空：409/400 时清掉等于让运营凭记忆重打一遍。
 *
 * 为什么平铺而不另开一层：密钥是**某个渠道**的属性，而两层抽屉的层级本就不可靠
 * （同为 z-index:60，内层遮罩还低于外层）。平铺后主抽屉是唯一可操作面，
 * 也就不存在"在错误的渠道上改密钥"。
 */

/** 最后一把可用密钥的拦截原因（开关与删除共用一句，避免两处口径不同）。 */
const LAST_KEY_REASON = "这是最后一把可用密钥，启用中的渠道不能清空";

/** 行锚点 id：编辑带收起后把焦点还给它（焦点掉到 body，Tab 会从文档头重来）。 */
function rowDomId(r: { key_id: string; index: number }): string {
  return `chz-key-${r.key_id || `i${r.index}`}`;
}

export function ChannelKeysSection({
  channelId,
  channelStatus,
}: {
  channelId: number;
  /** 1 = 启用中：不允许把可用密钥清空 */
  channelStatus: number;
}) {
  const query = useAdminChannelKeysQuery(channelId);
  const patch = usePatchAdminChannelKeysMutation();

  const [addOpen, setAddOpen] = useState(false);
  const [addText, setAddText] = useState("");
  const [addErr, setAddErr] = useState("");
  const [rebuildOpen, setRebuildOpen] = useState(false);
  const [rebuildText, setRebuildText] = useState("");
  const [rebuildErr, setRebuildErr] = useState("");
  /** 同一时刻只编辑一行：{@link keyId} 是唯一判定键（坏条目没有 keyID，也就没有编辑入口）。 */
  const [band, setBand] = useState<{ keyId: string; alias: string; key: string; err: string } | null>(null);
  const bandRef = useRef<HTMLDivElement>(null);

  const data = query.data;
  /** 整列坏（不是个别 token 解不开）：只剩整池重建一条路。 */
  const columnBroken = !!data && data.items.length === 0 && data.broken_count > 0;

  // 数据被刷新后，正在编辑的那把可能已被别人删掉 —— 悬空的编辑带必须收掉。
  useEffect(() => {
    if (!band || !data) return;
    if (!data.items.some((x) => x.key_id === band.keyId)) setBand(null);
  }, [band, data]);

  // 展开后把编辑带滚进可视区（底部 Sheet 下尤其必要：键盘弹出会顶掉半屏）。
  useEffect(() => {
    if (!band) return;
    // 可选调用：jsdom（单测）没有实现 scrollIntoView。
    const id = requestAnimationFrame(() => bandRef.current?.scrollIntoView?.({ block: "center" }));
    return () => cancelAnimationFrame(id);
  }, [band?.keyId]);

  const submit = (ops: ChannelKeyOp[], onSuccess?: (res: ChannelKeysResult) => void) => {
    const version = data?.version;
    if (version === undefined) {
      // 没有版本号就不能提交：后端会把它当作「无条件写」，等于放弃并发保护。
      toast.error("密钥池尚未加载完成，请稍后重试");
      return;
    }
    // 双保险：按钮已禁用，但键盘 Enter 仍可能连发两次。
    if (patch.isPending) return;
    patch.mutate(
      { id: channelId, body: { version, ops } },
      {
        onSuccess: (res) => onSuccess?.(res),
        onError: (error) => {
          if (error instanceof ApiError && error.errorCode === "channel_conflict") {
            // 保留输入：运营刚粘好的密钥不该因为别人同时改了一次而消失。
            toast.error("密钥池已被他人修改，已刷新，请核对后重新提交");
            void query.refetch();
            return;
          }
          if (error instanceof ApiError && error.errorCode === "not_found") {
            toast.error("渠道已不存在（可能已被删除）");
            return;
          }
          toast.error(error instanceof Error ? error.message : "操作失败，请重试");
        },
      },
    );
  };

  const closeBand = (focusKeyId?: string) => {
    setBand(null);
    if (!focusKeyId) return;
    requestAnimationFrame(() =>
      document.getElementById(`chz-key-${focusKeyId}`)?.focus({ preventScroll: true }),
    );
  };

  const onAdd = () => {
    const parsed = parseKeyLines(addText);
    const blocking = firstBlockingIssue(parsed.issues);
    if (blocking) {
      setAddErr(blocking.line > 0 ? `第 ${blocking.line} 行：${blocking.message}` : blocking.message);
      return;
    }
    if (parsed.keys.length === 0) {
      setAddErr("请先粘贴密钥");
      return;
    }
    // 单次粘贴的行数由 parseKeyLines 判；池子现状只有这里知道，两处都要看。
    const before = data?.key_count ?? 0;
    if (before + parsed.keys.length > MAX_KEYS) {
      setAddErr(`最多 ${MAX_KEYS} 把，当前 ${before} 把`);
      return;
    }
    setAddErr("");
    submit(
      parsed.keys.map((key) => ({ op: "add", key })),
      (res) => {
        setAddText("");
        // 前端算不出 keyID，"几把已存在"只能由回包的总数差反推。
        const added = Math.max(0, res.key_count - before);
        const skipped = parsed.keys.length - added;
        toast.success(skipped > 0 ? `已新增 ${added} 把（${skipped} 把已存在）` : `已新增 ${added} 把`);
      },
    );
  };

  const commitBand = async () => {
    if (!band || !data) return;
    const row = data.items.find((x) => x.key_id === band.keyId);
    if (!row) {
      setBand(null);
      return;
    }
    const alias = band.alias.trim();
    const aliasChanged = alias !== (row.alias ?? "");
    const raw = band.key.trim();

    // 新密钥留空 = 只改别名（发 meta）。两样都没变就不发请求，避免"提示已保存但没变"。
    if (!raw) {
      if (!aliasChanged) {
        closeBand(band.keyId);
        return;
      }
      submit([{ op: "meta", key_id: row.key_id, alias }], () => {
        closeBand(band.keyId);
        toast.success("别名已更新");
      });
      return;
    }

    const parsed = parseKeyLines(raw);
    const blocking = firstBlockingIssue(parsed.issues);
    if (blocking) {
      setBand({ ...band, err: blocking.line > 0 ? `第 ${blocking.line} 行：${blocking.message}` : blocking.message });
      return;
    }
    if (parsed.keys.length !== 1) {
      setBand({ ...band, err: "请只填一把密钥" });
      return;
    }
    setBand({ ...band, err: "" });
    const ok = await confirmDialog({
      title: "替换这把密钥的明文？",
      body: `位置、别名与启用态不变，旧明文不可找回。请同步到服务商侧作废旧密钥。${
        aliasChanged ? "（本次同时修改别名）" : ""
      }`,
      okText: "替换",
    });
    if (!ok) return;
    // 只能用 keyID 定位：前端算不出新 keyID，所以别名必须挂在同一个 op 上一起写。
    submit([{ op: "replace", key_id: row.key_id, key: raw, ...(aliasChanged ? { alias } : {}) }], () => {
      closeBand(band.keyId);
      toast.success(aliasChanged ? "密钥与别名已更新" : "密钥已替换");
    });
  };

  const onDelete = async (row: ChannelKeyItem) => {
    const ok = await confirmDialog({
      title: "删除这把密钥？",
      body: "删除后立即从选路中移除；如果它仍在上游使用，请同步到服务商侧作废。",
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    // 能算出 keyID 的必须用 keyID 删：按下标删在并发换序时会删掉别人的 key。
    submit([row.key_id ? { op: "remove", key_id: row.key_id } : { op: "remove", index: row.index }], () =>
      toast.success("已删除"),
    );
  };

  const onRebuild = async () => {
    const parsed = parseKeyLines(rebuildText);
    const blocking = firstBlockingIssue(parsed.issues);
    if (blocking) {
      setRebuildErr(blocking.line > 0 ? `第 ${blocking.line} 行：${blocking.message}` : blocking.message);
      return;
    }
    if (parsed.keys.length === 0) {
      setRebuildErr("请先粘贴密钥");
      return;
    }
    setRebuildErr("");
    const ok = await confirmDialog({
      title: "重建整个密钥池？",
      body: `将用这 ${parsed.keys.length} 把替换现有 ${data?.key_count ?? 0} 把。同值密钥的别名与启用态会保留，其余条目的别名一并丢失。`,
      okText: "重建",
      okVariant: "danger",
    });
    if (!ok) return;
    submit([{ op: "replace_all", keys: parsed.keys }], () => {
      setRebuildText("");
      setRebuildOpen(false);
      toast.success("密钥池已重建");
    });
  };

  const copyKeyID = async (row: ChannelKeyItem) => {
    if (!row.key_id) return;
    if (await copyText(row.key_id)) toast.success("已复制密钥ID");
    else toast.error("复制失败，请手动选择");
  };

  const lastUsable = data
    ? channelStatus === 1 && data.key_enabled_count === 1
      ? data.items.find((x) => x.decrypt_ok && x.enabled)?.key_id ?? ""
      : ""
    : "";

  const cooling = data?.items.filter((x) => x.decrypt_ok && x.enabled && x.cooldown_reason).length ?? 0;
  const notes = [
    data && cooling > 0 ? `${cooling} 把冷却中` : "",
    data && data.broken_count > 0 ? `${data.broken_count} 把无法解密` : "",
  ].filter(Boolean);
  const showAdd = addOpen || (!!data && data.key_count === 0 && !columnBroken);
  /** 服务端给的是"拉取那一刻"的 TTL，逐秒递减的基准必须钉在这个时刻上。 */
  const since = query.dataUpdatedAt || Date.now();

  return (
    <section className="ms-keys" aria-label="密钥池">
      <div className="ms-keys__head">
        <div className="blk-title">密钥池</div>
        <Tag tone="info">即时生效</Tag>
        <span className="ms-keys__grow" />
        {data ? (
          <span className="muted small">
            已存 {data.key_count} 把 · 可用 {data.key_enabled_count} 把
          </span>
        ) : null}
      </div>
      {notes.length ? <div className="ms-keys__note muted small">{notes.join(" · ")}</div> : null}

      {query.isLoading ? <LoadingState title="正在读取密钥池…" /> : null}
      {query.isError ? <ErrorState error={query.error} onRetry={() => void query.refetch()} /> : null}

      {data ? (
        <>
          <div className="ms-keys__acts">
            <button
              type="button"
              className="btn btn-sm"
              disabled={patch.isPending || columnBroken}
              onClick={() => {
                setAddErr("");
                setAddOpen(true);
              }}
            >
              <Icon name="plus" size={14} /> 添加密钥
            </button>
            <button
              type="button"
              className="btn btn-sm btn-text"
              disabled={patch.isPending}
              onClick={() => {
                setRebuildErr("");
                setRebuildOpen((v) => !v);
              }}
            >
              重建密钥池…
            </button>
          </div>

          {showAdd ? (
            <div className="ms-keys__add">
              <Field
                label="新增密钥"
                htmlFor="chz-key-add"
                hint={addErr || undefined}
                hintWarn={addErr !== ""}
              >
                <Textarea
                  id="chz-key-add"
                  rows={3}
                  autoComplete="off"
                  placeholder="每行一个密钥；可一次粘贴多把"
                  value={addText}
                  onChange={(e) => {
                    setAddText(e.target.value);
                    setAddErr("");
                  }}
                />
              </Field>
              <div className="row-acts">
                <button type="button" className="btn btn-primary btn-sm" disabled={patch.isPending} onClick={onAdd}>
                  添加
                </button>
                {addOpen && data.key_count > 0 ? (
                  <button
                    type="button"
                    className="btn btn-sm btn-ghost"
                    onClick={() => {
                      setAddOpen(false);
                      setAddText("");
                      setAddErr("");
                    }}
                  >
                    取消
                  </button>
                ) : null}
              </div>
            </div>
          ) : null}

          {rebuildOpen ? (
            <div className="ms-keys__add">
              <Field
                label="重建密钥池"
                htmlFor="chz-key-rebuild"
                hint={rebuildErr || undefined}
                hintWarn={rebuildErr !== ""}
              >
                <Textarea
                  id="chz-key-rebuild"
                  rows={4}
                  autoComplete="off"
                  placeholder="粘贴完整的新密钥池，每行一把"
                  value={rebuildText}
                  onChange={(e) => {
                    setRebuildText(e.target.value);
                    setRebuildErr("");
                  }}
                />
              </Field>
              <div className="row-acts">
                <button
                  type="button"
                  className="btn btn-danger btn-sm"
                  disabled={patch.isPending}
                  onClick={() => void onRebuild()}
                >
                  确认重建
                </button>
                <button
                  type="button"
                  className="btn btn-sm btn-ghost"
                  onClick={() => {
                    setRebuildOpen(false);
                    setRebuildText("");
                    setRebuildErr("");
                  }}
                >
                  取消
                </button>
              </div>
            </div>
          ) : null}

          {data.key_enabled_count === 0 && channelStatus === 1 && !columnBroken ? (
            <div className="ms-keys__warn">渠道处于启用状态，但没有一把可用密钥，所有请求都会失败。</div>
          ) : null}

          {data.items.length ? (
            <div className="ms-keys__list">
              {data.items.map((r) => {
                const blocked = !!r.key_id && r.key_id === lastUsable;
                const actions: RowAction[] = [
                  {
                    icon: "edit",
                    label: "编辑密钥",
                    hidden: !r.decrypt_ok,
                    disabled: patch.isPending || (!!band && band.keyId !== r.key_id),
                    disabledReason: band ? "先保存或取消当前这一把" : "请求进行中…",
                    onClick: () => setBand({ keyId: r.key_id, alias: r.alias, key: "", err: "" }),
                  },
                  {
                    icon: "refresh",
                    label: "重置冷却",
                    hidden: !r.decrypt_ok || !r.cooldown_reason,
                    busy: patch.isPending,
                    onClick: () => submit([{ op: "reset_cooldown", key_id: r.key_id }]),
                  },
                  {
                    icon: "trash",
                    label: "删除密钥",
                    tone: "danger",
                    disabled: patch.isPending || blocked,
                    disabledReason: blocked ? LAST_KEY_REASON : "请求进行中…",
                    onClick: () => void onDelete(r),
                  },
                ];
                return (
                  <div className="ms-key" key={r.key_id || `i${r.index}`} id={rowDomId(r)} tabIndex={-1}>
                    <div className="ms-key__top">
                      {/* 指纹 + 状态是一组：窄屏放不下时让状态徽标自己换行，
                          而不是把指纹挤成一个字一行（`sk-b…` / `2222` 那种）。
                          右侧开关与行内动作必须始终贴右且不压缩，否则删除按钮会被挤出抽屉。 */}
                      <div className="ms-key__id">
                        {r.decrypt_ok ? (
                          // 点击复制 keyID：管理台「调用记录」的密钥ID搜索框要的正是它，
                          // 而它只在这里能拿到（抽屉之外看不到、也算不出来）。
                          <button
                            type="button"
                            className="ms-key__fp"
                            title="点击复制密钥ID"
                            onClick={() => void copyKeyID(r)}
                          >
                            {r.fingerprint || "—"}
                          </button>
                        ) : (
                          <span className="ms-key__fp is-broken">—</span>
                        )}
                        <KeyStatus row={r} since={since} />
                      </div>
                      <div className="ms-key__end">
                        {r.decrypt_ok ? (
                          <SwitchRow
                            className="switch-row--inline"
                            label=""
                            ariaLabel={`启用密钥 ${r.fingerprint || `#${r.index + 1}`}`}
                            checked={r.enabled}
                            disabled={patch.isPending || blocked}
                            onChange={(v) => submit([{ op: "meta", key_id: r.key_id, enabled: v }])}
                          />
                        ) : null}
                        <RowActions actions={actions} />
                      </div>
                    </div>
                    <div className="ms-key__sub">
                      <span className={r.alias ? "" : "muted"}>{r.alias || "未命名"}</span>
                      {r.last_error_status > 0 ? (
                        <span
                          className="muted small"
                          title={r.last_error_at ? formatDateTime(r.last_error_at) : undefined}
                        >
                          最近错误 {r.last_error_status}
                        </span>
                      ) : null}
                    </div>

                    {band && band.keyId === r.key_id ? (
                      <div
                        className="ms-key__band"
                        ref={bandRef}
                        onKeyDown={(e) => {
                          if (e.key === "Escape") {
                            // 只收编辑带：不拦的话会一路冒到 document，把整个抽屉关掉。
                            e.stopPropagation();
                            closeBand(band.keyId);
                          } else if (e.key === "Enter") {
                            e.preventDefault();
                            void commitBand();
                          }
                        }}
                      >
                        <Field label="别名">
                          <Input
                            autoFocus
                            maxLength={64}
                            value={band.alias}
                            placeholder="例如：主账号 / 备用-欧洲"
                            onChange={(e) => setBand({ ...band, alias: e.target.value })}
                          />
                        </Field>
                        <Field
                          label="新密钥"
                          hint={
                            band.err ||
                            (band.key.trim() ? "填成原值不会有任何变化；位置、别名与启用态不变" : undefined)
                          }
                          hintWarn={band.err !== ""}
                        >
                          <Input
                            type="password"
                            autoComplete="new-password"
                            spellCheck={false}
                            value={band.key}
                            placeholder="留空则不修改"
                            onChange={(e) => setBand({ ...band, key: e.target.value, err: "" })}
                          />
                        </Field>
                        <div className="row-acts">
                          <button type="button" className="btn btn-sm btn-ghost" onClick={() => closeBand(band.keyId)}>
                            取消
                          </button>
                          <button
                            type="button"
                            className="btn btn-sm btn-primary"
                            disabled={patch.isPending}
                            onClick={() => void commitBand()}
                          >
                            保存
                          </button>
                        </div>
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : columnBroken ? (
            <EmptyState
              variant="error"
              pad="18px"
              title="密钥列已损坏"
              desc="只能整池重建：粘贴完整的新密钥池（同值密钥的别名会保留）"
            />
          ) : (
            <EmptyState title="还没有密钥" desc="粘贴密钥后点「添加」，仅管理员可见" />
          )}
        </>
      ) : null}
    </section>
  );
}

/**
 * 行状态徽标。
 *
 * 优先级互斥：无法解密 > 已停用 > 冷却 > 正常 —— 四种状态混着显示时，
 * 运营无法判断"这把到底能不能用"。
 */
function KeyStatus({ row, since }: { row: ChannelKeyItem; since: number }) {
  if (!row.decrypt_ok) return <Tag tone="danger">无法解密</Tag>;
  if (!row.enabled) return <Tag tone="gray">已停用</Tag>;
  if (!row.cooldown_reason) return <Tag tone="success">正常</Tag>;
  return <CooldownTag reason={row.cooldown_reason} remain={row.cooldown_remaining_sec} since={since} />;
}

/** 冷却徽标：逐秒递减只订阅这一小块，避免整个列表每秒重渲染。 */
function CooldownTag({ reason, remain, since }: { reason: string; remain: number; since: number }) {
  useSecondTick();
  const left = Math.max(0, remain - Math.floor((Date.now() - since) / 1000));
  const label = reason === "auth" ? "鉴权失败" : "限流";
  // 到 0 不说"正常"：另一个并发请求可能刚又把它冷却了，只能说"该恢复了"。
  return <Tag tone="warning">{left > 0 ? `${label} · 约 ${left}s` : `${label} · 即将恢复`}</Tag>;
}
