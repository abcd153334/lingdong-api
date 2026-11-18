import { useMemo, useState } from "react";
import { useNavigate } from "@tanstack/react-router";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Textarea } from "@/components/common/Textarea";
import { toastOk } from "@/components/common/Toaster";
import { SchemaFields } from "@/components/plugins/SchemaFields";
import { toastApiError } from "@/lib/apiToast";
import {
  useAdminAppPluginInstallsQuery,
  useAdminAppPluginQuery,
  useAdminAppPluginsQuery,
  useAdminAppPluginSampleQuery,
  useDeleteAppPluginMutation,
  useImportAppPluginMutation,
  useInstallAppPluginMutation,
  usePatchAppPluginMutation,
  usePurgeAppPluginMutation,
  useSetAppPluginCatalogStatusMutation,
  useUninstallAppPluginMutation,
} from "@/lib/api";
import { toFormValues, type FormValues } from "@/lib/pluginTypes";
import type { AdminAppItem, AdminAppPluginInstall, AdminAppPluginItem, PluginField } from "@/types";

/**
 * 应用管理弹窗的「插件」页签：安装清单 + 导入入口。
 *
 * 两条边界（改之前先读）：
 *  - 这里只做**清单与入口**：已安装的每个插件在弹窗顶部各占一个页签
 *    （`plugin:<key>`），「打开」只是把当前页签切过去，不跳路由。
 *    记录表与表单在 `components/plugins/PluginDataPanel.tsx`。
 *  - 所有写操作都立即生效，没有"保存"按钮：密钥池、公告早就是这个口径，
 *    弹窗底部也没有统一保存条，多一个"先改后存"会让人以为取消能回滚。
 */
export function PluginsTab({ app, onOpenPlugin }: {
  app: AdminAppItem;
  /** 宿主自己接管「打开」时的行为（例如切页签）；不给则回退到跳插件独立页。 */
  onOpenPlugin?: (key: string) => void;
}) {
  const navigate = useNavigate();
  const openPlugin =
    onOpenPlugin ??
    ((key: string) => {
      void navigate({ to: "/admin/apps/$id/p/$plugin", params: { id: String(app.id), plugin: key } });
    });
  const installs = useAdminAppPluginInstallsQuery(app.id);
  // 目录一次取满一页（后端上限 100）。取不满时下面会明说"只显示前 N 个"，
  // 静默截断会让管理员以为插件丢了。
  const catalog = useAdminAppPluginsQuery({ page: 1, page_size: 100 });

  const install = useInstallAppPluginMutation();
  const patch = usePatchAppPluginMutation();
  const uninstall = useUninstallAppPluginMutation();
  const purge = usePurgeAppPluginMutation();
  const drop = useDeleteAppPluginMutation();
  const setCatalogStatus = useSetAppPluginCatalogStatusMutation();

  const [importOpen, setImportOpen] = useState(false);
  const [installTarget, setInstallTarget] = useState<AdminAppPluginItem | null>(null);
  const [configTarget, setConfigTarget] = useState<AdminAppPluginInstall | null>(null);

  const installItems = installs.data?.items ?? [];
  const installedKeys = new Set(installItems.map((i) => i.plugin_key));
  const available = (catalog.data?.items ?? []).filter((p) => !installedKeys.has(p.key));

  return (
    <>
      <div className="plugin-sec">
        <div className="plugin-sec__head">
          <span className="blk-title">已安装</span>
          <span className="muted small">{installItems.length} 个</span>
          <span className="plugin-sec__grow" />
          <button type="button" className="btn btn-sm btn-primary" onClick={() => setImportOpen(true)}>
            导入插件
          </button>
        </div>

        {installs.isPending ? (
          <div className="muted small">正在加载…</div>
        ) : installItems.length === 0 ? (
          <EmptyState
            title="还没有安装插件"
            desc="导入插件定义后，就能给这个应用加自己的数据与页面。"
            pad="18px"
          />
        ) : (
          <div className="plugin-list">
            {installItems.map((it) => (
              <div key={it.plugin_key} className="plugin-row">
                <div className="plugin-row__main">
                  <span className="plugin-row__name">{it.name}</span>
                  <span className={statusClass(it.status)}>{statusText(it.status)}</span>
                  <span className="muted small">
                    {it.record_count} 条 · v{it.plugin_version || it.version}
                  </span>
                </div>
                <div className="plugin-row__acts">
                  {it.status !== 3 ? (
                    <button
                      type="button"
                      className="btn btn-sm btn-text"
                      onClick={() => openPlugin(it.plugin_key)}
                    >
                      打开
                    </button>
                  ) : null}
                  <button
                    type="button"
                    className="btn btn-sm btn-text"
                    onClick={() => setConfigTarget(it)}
                  >
                    配置
                  </button>
                  {it.status === 3 ? (
                    <button
                      type="button"
                      className="btn btn-sm btn-text"
                      disabled={patch.isPending}
                      onClick={() =>
                        patch.mutate(
                          { id: app.id, key: it.plugin_key, status: 1, expectedVersion: it.lock_version },
                          { onSuccess: () => toastOk("已重新启用"), onError: (e) => toastApiError(e, "启用失败") },
                        )
                      }
                    >
                      重新启用
                    </button>
                  ) : (
                    <button
                      type="button"
                      className="btn btn-sm btn-text"
                      disabled={patch.isPending}
                      onClick={() =>
                        patch.mutate(
                          {
                            id: app.id,
                            key: it.plugin_key,
                            status: it.status === 1 ? 2 : 1,
                            expectedVersion: it.lock_version,
                          },
                          {
                            onSuccess: () => toastOk(it.status === 1 ? "已停用" : "已启用"),
                            onError: (e) => toastApiError(e, "操作失败"),
                          },
                        )
                      }
                    >
                      {it.status === 1 ? "停用" : "启用"}
                    </button>
                  )}
                  {it.status !== 3 ? (
                    <button
                      type="button"
                      className="btn btn-sm btn-text"
                      disabled={uninstall.isPending}
                      onClick={() =>
                        void (async () => {
                          const ok = await confirmDialog({
                            title: `卸载「${it.name}」？`,
                            body: `已保存的 ${it.record_count} 条数据会保留，重新启用后可以继续使用。`,
                            okText: "卸载",
                          });
                          if (!ok) return;
                          uninstall.mutate(
                            { id: app.id, key: it.plugin_key },
                            { onSuccess: () => toastOk("已卸载"), onError: (e) => toastApiError(e, "卸载失败") },
                          );
                        })()
                      }
                    >
                      卸载
                    </button>
                  ) : (
                    <button
                      type="button"
                      className="btn btn-sm btn-text plugin-danger"
                      disabled={purge.isPending}
                      onClick={() =>
                        void (async () => {
                          const ok = await confirmDialog({
                            title: `清除「${it.name}」的全部数据？`,
                            body: `${it.record_count} 条记录会被永久删除，无法恢复。`,
                            okText: "永久删除",
                            okVariant: "danger",
                          });
                          if (!ok) return;
                          purge.mutate(
                            { id: app.id, key: it.plugin_key },
                            {
                              onSuccess: (r) => toastOk(`已清除 ${r.deleted} 条`),
                              onError: (e) => toastApiError(e, "清除失败"),
                            },
                          );
                        })()
                      }
                    >
                      清除数据
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="plugin-sec">
        <div className="plugin-sec__head">
          <span className="blk-title">可安装</span>
          <span className="muted small">同一插件在每个应用下各自配置、互不影响</span>
        </div>
        {available.length === 0 ? (
          <div className="muted small">目录里还没有别的插件。点右上角「导入插件」添加。</div>
        ) : (
          <>
            {(catalog.data?.total ?? 0) > (catalog.data?.items.length ?? 0) ? (
              <div className="muted small">
                共 {catalog.data?.total} 个插件，这里只列出前 {catalog.data?.items.length} 个。
              </div>
            ) : null}
            <div className="plugin-list">
              {available.map((p) => (
                <div key={p.key} className="plugin-row">
                  <div className="plugin-row__main">
                    <span className="plugin-row__name">{p.name}</span>
                    <span className="muted small">
                      {p.key} · v{p.version}
                      {p.installs > 0 ? ` · ${p.installs} 个应用在用` : ""}
                    </span>
                    {p.status === 2 ? <span className="badge">已下架</span> : null}
                  </div>
                  <div className="plugin-row__acts">
                    <button
                      type="button"
                      className="btn btn-sm btn-text"
                      disabled={p.status !== 1}
                      onClick={() => setInstallTarget(p)}
                    >
                      安装
                    </button>
                    <button
                      type="button"
                      className="btn btn-sm btn-text"
                      disabled={setCatalogStatus.isPending}
                      onClick={() =>
                        setCatalogStatus.mutate(
                          { key: p.key, status: p.status === 1 ? 2 : 1 },
                          {
                            onSuccess: () => toastOk(p.status === 1 ? "已下架" : "已上架"),
                            onError: (e) => toastApiError(e, "操作失败"),
                          },
                        )
                      }
                    >
                      {p.status === 1 ? "下架" : "上架"}
                    </button>
                    <button
                      type="button"
                      className="btn btn-sm btn-text plugin-danger"
                      disabled={drop.isPending || p.installs > 0}
                      onClick={() =>
                        void (async () => {
                          const ok = await confirmDialog({
                            title: `从目录删除「${p.name}」？`,
                            body: "只会删掉插件定义，已安装应用里的数据不受影响。",
                            okText: "删除",
                            okVariant: "danger",
                          });
                          if (!ok) return;
                          drop.mutate(p.key, {
                            onSuccess: () => toastOk("已删除"),
                            onError: (e) => toastApiError(e, "删除失败"),
                          });
                        })()
                      }
                    >
                      删除
                    </button>
                  </div>
                </div>
              ))}
            </div>
          </>
        )}
      </div>

      <PluginImportDialog open={importOpen} onClose={() => setImportOpen(false)} />

      {installTarget ? (
        <PluginConfigDialog
          key={`install-${installTarget.key}`}
          open
          title={`安装「${installTarget.name}」`}
          pluginKey={installTarget.key}
          okText="安装"
          initial={{}}
          submitting={install.isPending}
          onClose={() => setInstallTarget(null)}
          onSubmit={(config) =>
            install.mutate(
              { id: app.id, pluginKey: installTarget.key, config },
              {
                onSuccess: () => {
                  setInstallTarget(null);
                  toastOk("已安装");
                },
                onError: (e) => toastApiError(e, "安装失败"),
              },
            )
          }
        />
      ) : null}

      {configTarget ? (
        <PluginConfigDialog
          key={`config-${configTarget.plugin_key}`}
          open
          title={`配置「${configTarget.name}」`}
          pluginKey={configTarget.plugin_key}
          okText="保存"
          initial={configTarget.config}
          submitting={patch.isPending}
          onClose={() => setConfigTarget(null)}
          onSubmit={(config) =>
            patch.mutate(
              {
                id: app.id,
                key: configTarget.plugin_key,
                config,
                expectedVersion: configTarget.lock_version,
              },
              {
                onSuccess: () => {
                  setConfigTarget(null);
                  toastOk("已保存");
                },
                onError: (e) => toastApiError(e, "保存失败"),
              },
            )
          }
        />
      ) : null}
    </>
  );
}

function statusText(status: number) {
  return status === 1 ? "启用中" : status === 2 ? "已停用" : "已卸载";
}

function statusClass(status: number) {
  // 只用既有语义色：启用=ok，停用=warn，卸载=中性。不自造色板。
  return status === 1 ? "badge badge-success" : status === 2 ? "badge badge-warning" : "badge badge-info";
}

/** 安装/配置共用：字段描述来自目录详情（后端编译产物），前端不解析 manifest。 */
function PluginConfigDialog({
  open,
  title,
  pluginKey,
  okText,
  initial,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean;
  title: string;
  pluginKey: string;
  okText: string;
  initial: Record<string, unknown>;
  submitting: boolean;
  onClose: () => void;
  onSubmit: (config: Record<string, unknown>) => void;
}) {
  const detail = useAdminAppPluginQuery(pluginKey);
  const fields: PluginField[] = detail.data?.plugin.config ?? [];
  /**
   * 字段描述是**异步**到的，而 `useState(初始化函数)` 只跑第一次 —— 那时 fields 还是空，
   * 于是已保存的配置在对话框里会全部显示为空（管理员以为没配，重填一遍）。
   * 这里不写 effect 同步：服务端值当默认，用户改过的键叠在上面，一份数据一条路径。
   */
  const [draft, setDraft] = useState<FormValues | null>(null);
  const values = draft ?? toFormValues(fields, initial);
  const [problems, setProblems] = useState<{ path: string; message: string }[]>([]);
  const errors = useMemo(() => {
    const out: Record<string, string> = {};
    for (const p of problems) {
      const key = p.path.startsWith("config.") ? p.path.slice("config.".length) : "";
      if (key) out[key] = p.message;
    }
    return out;
  }, [problems]);

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={title}
      closeButton
      // 本弹窗是「应用管理弹窗」里的子弹窗：不升到 body 会被外层 transform 定位、
      // 被 .dl-body 的 overflow:hidden 裁掉（见 Dialog.tsx 的 portal 注释）。
      portal
      layer={2}
      actions={[
        { label: "取消", onClick: onClose },
        {
          label: submitting ? "处理中…" : okText,
          variant: "primary",
          // 字段描述取不到就别让保存：空表单点保存只会写回原值，看起来像"改了没生效"。
          disabled: submitting || detail.isError,
          onClick: () => onSubmit(payloadOf(fields, values)),
        },
      ]}
    >
      {detail.isError ? (
        // 目录里的定义被删掉了：此时既没有字段可填，也不该说"没有需要填写的配置"
        // （那是另一个意思 —— 插件本身没声明配置）。
        <p className="muted small">插件定义已不在目录里，配置无法修改。</p>
      ) : fields.length === 0 ? (
        <p className="muted small">这个插件没有需要填写的配置。</p>
      ) : (
        <SchemaFields
          fields={fields}
          values={values}
          errors={errors}
          idPrefix="plugin-config"
          disabled={submitting}
          onChange={(k, v) => setDraft((prev) => ({ ...(prev ?? values), [k]: v }))}
        />
      )}
    </Dialog>
  );
}

/** 配置提交体：空串不下发，int 转数字（服务端仍会再校验一遍）。 */
function payloadOf(fields: PluginField[], values: FormValues): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of fields) {
    const raw = values[f.key];
    if (f.type === "bool") {
      out[f.key] = raw === true;
      continue;
    }
    const s = typeof raw === "string" ? raw.trim() : raw === undefined || raw === null ? "" : String(raw);
    if (s === "") continue;
    out[f.key] = f.type === "int" ? Number(s) : s;
  }
  return out;
}

/**
 * 导入插件：粘贴 / 上传 / 填示例 → 校验（不落库）→ 导入。
 *
 * 为什么"校验"和"导入"是两个按钮：manifest 是 AI 产出的常见形态，
 * 一次贴进来往往有 3-5 处要改；校验能只报问题不落库，改完再导。
 */
function PluginImportDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [text, setText] = useState("");
  const sample = useAdminAppPluginSampleQuery(open);
  const importMut = useImportAppPluginMutation();

  const parsed = useMemo(() => {
    if (!text.trim()) return { key: "", value: null as unknown, error: "" };
    try {
      const v = JSON.parse(text) as { key?: string };
      return { key: typeof v?.key === "string" ? v.key : "", value: v, error: "" };
    } catch {
      return { key: "", value: null as unknown, error: "不是合法的 JSON" };
    }
  }, [text]);
  /**
   * 版本号从**目录列表**里取，不去 GET 详情。
   *
   * 为什么不是 `useAdminAppPluginQuery(key)`：导入对话框处理的键**可能还不存在** ——
   * 新建插件时详情接口必然 404，控制台里那条红色 `GET /app-plugins/xxx 404`
   * 会让管理员以为导入坏了（也确实白跑一次请求）。列表接口带 `q` 过滤后取第一行即可
   * 拿到 `lock_version`（后端把精确命中的 key 排在最前），不存在时就是空列表，没有错误态。
   *
   * 也不能直接复用父级那一页目录：它是 `page_size=100` 且有"只显示前 N 个"的截断，
   * 第 101 个插件会取不到版本 ⇒ 升级被当成新建（服务端会拒，但用户看不懂）。
   */
  const catalogQuery = useAdminAppPluginsQuery({ q: parsed.key, page: 1, page_size: 20 }, !!parsed.key);
  const known = (catalogQuery.data?.items ?? []).find((i) => i.key === parsed.key);
  const expected = known?.lock_version ?? 0;
  /**
   * 目录还没读回来时不许提交：这一刻 `expected` 恒为 0，会把"升级"发成"新建"，
   * 服务端只能回一个版本冲突（看起来像"导入坏了"）。
   */
  const versionReady = !parsed.key || (!catalogQuery.isFetching && !catalogQuery.isError);
  const res = importMut.data;

  const run = (dryRun: boolean, force: boolean) => {
    if (!parsed.value || !parsed.key) return;
    importMut.mutate(
      { key: parsed.key, manifest: parsed.value, expectedVersion: expected, dryRun, force },
      {
        onSuccess: (r) => {
          if (r.applied) {
            toastOk(r.created ? "已导入" : `已更新到 ${r.version}`);
            onClose();
            setText("");
          }
        },
        onError: (err) => toastApiError(err, dryRun ? "校验失败" : "导入失败"),
      },
    );
  };

  const problems = res?.problems ?? [];
  const breaking = res?.breaking ?? [];

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="导入插件"
      closeButton
      size="wide"
      // 同上：弹窗里的弹窗必须 portal + 第二层。
      portal
      layer={2}
      actions={[
        { label: "关闭", onClick: onClose },
        {
          label: importMut.isPending ? "校验中…" : "校验",
          onClick: () => run(true, false),
          disabled: !parsed.value || !versionReady,
        },
        {
          label: "导入",
          variant: "primary",
          disabled: !parsed.value || importMut.isPending || !versionReady,
          onClick: () => run(false, breaking.length > 0),
        },
      ]}
    >
      <Field
        label="插件定义"
        htmlFor="plugin-import-text"
        hint={parsed.error || undefined}
        hintWarn={!!parsed.error}
      >
        <Textarea
          id="plugin-import-text"
          rows={12}
          value={text}
          placeholder="粘贴插件定义，或从文件读取"
          onChange={(e) => setText(e.target.value)}
        />
      </Field>

      <div className="plugin-toolbar">
        <Field label="从文件读取" htmlFor="plugin-import-file">
          <Input
            id="plugin-import-file"
            type="file"
            accept="application/json,.json"
            onChange={(e) => {
              const f = e.target.files?.[0];
              if (!f) return;
              const reader = new FileReader();
              reader.onload = () => setText(String(reader.result ?? ""));
              reader.readAsText(f);
            }}
          />
        </Field>
        <button
          type="button"
          className="btn btn-sm btn-text"
          disabled={!sample.data}
          onClick={() => {
            if (!sample.data) return;
            setText(JSON.stringify(sample.data.manifest, null, 2));
          }}
        >
          填入示例
        </button>
      </div>

      {catalogQuery.isError ? (
        <div className="plugin-check__bad">读不到插件目录，无法判断是新建还是升级：请关闭对话框重试。</div>
      ) : null}

      {res && !res.applied ? (
        <div className="plugin-check">
          <div className={problems.length ? "plugin-check__bad" : "plugin-check__ok"}>
            {problems.length ? `发现 ${problems.length} 处问题` : "没有发现问题，可以导入"}
          </div>
          {problems.length ? (
            <ul className="plugin-problems">
              {problems.map((p) => (
                <li key={`${p.path}-${p.message}`}>
                  <code>{p.path || "整体"}</code> {p.message}
                </li>
              ))}
            </ul>
          ) : null}
          {breaking.length ? (
            <div className="plugin-problems">
              <div>这次升级会影响已有数据：</div>
              <ul>
                {breaking.map((b) => (
                  <li key={`${b.path}-${b.message}`}>
                    <code>{b.path || "整体"}</code> {b.message}
                  </li>
                ))}
              </ul>
              <div className="muted small">确认无误后点「导入」，会按新定义生效。</div>
            </div>
          ) : null}
          {res.affected.length ? (
            <div className="muted small">
              受影响记录：
              {res.affected.map((a) => `${a.entity}.${a.field} ${a.records} 条（${a.reason}）`).join("；")}
            </div>
          ) : null}
        </div>
      ) : null}
      {importMut.isError ? (
        <div className="plugin-check__bad">
          {(importMut.error as { message?: string })?.message ?? "导入失败"}
        </div>
      ) : null}
    </Dialog>
  );
}
