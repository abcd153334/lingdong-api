import { useEffect, useMemo, useRef, useState } from "react";
import { useSearch } from "@tanstack/react-router";

import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Field } from "@/components/common/Field";
import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { PageTabPanels } from "@/components/common/PageTabs";
import { Select } from "@/components/common/Select";
import { Tabs } from "@/components/common/Tabs";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { toastErr, toastOk } from "@/components/common/Toaster";
import { DocsPreview } from "@/docs/DocsApp";
import { GuideView } from "@/docs/GuideView";
import {
  useCreateEndpoint,
  useCreateNode,
  useDeleteEndpoint,
  useDeleteNode,
  useDocsAdminEndpoint,
  useDocsAdminNode,
  useDocsAdminTransports,
  useDocsAdminTree,
  useEndpointAction,
  useEndpointPreview,
  useGeneratePresetSamples,
  useGenerateSamples,
  useNodeAction,
  useNodePreview,
  useSaveEndpoint,
  useSaveNode,
  useSaveSection,
} from "@/lib/api/adminDocs";
import { usePageTabs, useLooseNavigate, type PageTab } from "@/lib/pageTabs";
import { useIsDocsAdminDrawer } from "@/lib/responsive";
import type { DocsAdminSearch } from "@/lib/searchParams";
import type { DocV7Endpoint } from "@/types/docsv7";
import type {
  DocsAdminEndpoint,
  DocsAdminEndpointData,
  DocsAdminNode,
  DocsAdminNodeData,
  DocsAdminSection,
  DocsAdminSectionData,
} from "@/types/docsadmin";

import { EndpointForm } from "@/components/admin/docs/EndpointForm";
import { ProblemsPanel } from "@/components/admin/docs/ProblemsPanel";
import { NodeForm } from "@/components/admin/docs/NodeForm";
import { Rail } from "@/components/admin/docs/Rail";
import {
  openPath,
  RAIL_CLOSED,
  reconcile,
  seedOpen,
  toggle,
  type RailOpen,
  type Selection,
} from "@/components/admin/docs/railModel";
import { SectionForm } from "@/components/admin/docs/SectionForm";
import { TopBar } from "@/components/admin/docs/TopBar";

/**
 * API 文档编辑页：左栏级联 + 右栏「编辑 / 预览」。
 *
 * 一条直线：**库是唯一真相源**。这里不合成任何内容（示例代码、响应变体、板块名
 * 全部来自后端），保存就是把表单原样提交；预览走与线上同一个渲染器。
 *
 * 状态归属（写 URL 的只有「选了谁」和「看的哪个页签」）：
 *   ?s= / ?node= / ?ep=   选中对象，三者互斥
 *   ?view=preview         右栏页签，缺省即编辑
 *   展开态 / 抽屉开合 / 弹窗    本地 state（不是内容，不该进历史记录）
 *
 * 草稿用「身份 + 快照」判定，不用 effect 重置：`edited.id !== identity` 时自然
 * 回落到服务器值，省掉一整类"保存后草稿被旧快照覆盖"的竞态。
 */

/** 页签与面板**共用**一个 id 前缀：`{P}-tab-{k}` ↔ `{P}-panel-{k}` 的 aria 关联靠它。
    写成常量而不是两处字面量，是因为门禁 M18 只看字面量是否重复（它守的是"同页两组
    页签撞 id"），而这里重复是**设计**。 */
const TAB_IDS = "adp-view";

const TABS = [
  { key: "edit", label: "编辑", icon: "edit" },
  { key: "preview", label: "预览", icon: "eye" },
] as const satisfies readonly PageTab<"edit" | "preview">[];

type Draft =
  | { kind: "section"; key: string; base: DocsAdminSectionData; data: DocsAdminSectionData }
  | { kind: "node"; key: string; base: DocsAdminNodeData; data: DocsAdminNodeData }
  | { kind: "ep"; node: string; key: string; base: DocsAdminEndpointData; data: DocsAdminEndpointData };

export function DocsAdmin() {
  const search = useSearch({ strict: false }) as DocsAdminSearch;
  const go = useLooseNavigate();
  const drawer = useIsDocsAdminDrawer();
  const [sideOpen, setSideOpen] = useState(false);

  const sel: Selection = { section: search.s, node: search.node, ep: search.ep };
  const treeQ = useDocsAdminTree();
  const nodeQ = useDocsAdminNode(!search.ep && search.node ? search.node : "");
  const epQ = useDocsAdminEndpoint(search.node ?? "", search.ep ?? "");
  const transportsQ = useDocsAdminTransports();

  const tree = treeQ.data;
  const sections = useMemo(() => tree?.sections ?? [], [tree]);

  /* ---------------- 选中纠偏 ---------------- */
  useEffect(() => {
    if (!tree) return;
    const r = reconcile(tree, sel);
    if (!r.changed) return;
    go({ search: () => pickSearch(r.sel), replace: true, resetScroll: false });
    // sel 是每次渲染新建的对象：依赖它的字段而不是对象本身。
  }, [tree, search.s, search.node, search.ep, go]);

  /* ---------------- 展开态 ---------------- */
  // 两级各一份展开位（板块 / 节点），互相独立，只由箭头按钮改动。
  // 种子只播一次：树首次到手时摊开「选中项所在路径 / 第一个有节点的板块」，
  // 之后完全交给点击与 openPath —— 否则用户刚收起的行会被下一次 render 又撑开。
  const [open, setOpen] = useState<RailOpen>(RAIL_CLOSED);
  const seeded = useRef(false);
  useEffect(() => {
    if (!tree) return;
    const cur: Selection = { section: search.s, node: search.node, ep: search.ep };
    if (!seeded.current) {
      seeded.current = true;
      setOpen(seedOpen(tree, cur));
      return;
    }
    setOpen((prev) => openPath(prev, tree, cur));
  }, [tree, search.s, search.node, search.ep]);

  /* ---------------- 草稿 ---------------- */
  const loaded = useMemo<Draft | null>(() => {
    if (sel.ep) {
      const ep = epQ.data;
      if (!ep) return null;
      const d = epData(ep);
      return { kind: "ep", node: sel.node ?? "", key: sel.ep, base: d, data: d };
    }
    if (sel.node) {
      const n = nodeQ.data;
      if (!n) return null;
      const d = nodeData(n);
      return { kind: "node", key: sel.node, base: d, data: d };
    }
    if (sel.section) {
      const s = sections.find((x) => x.key === sel.section);
      if (!s) return null;
      const d = sectionData(s);
      return { kind: "section", key: sel.section, base: d, data: d };
    }
    return null;
  }, [sel.ep, sel.node, sel.section, epQ.data, nodeQ.data, sections]);

  /** 身份：换了对象或对象版本变了（保存成功/别人改过）就丢弃本地草稿 */
  const identity = loaded
    ? `${loaded.kind}:${loaded.kind === "ep" ? `${loaded.node}/${loaded.key}` : loaded.key}@${loaded.base.version}`
    : "";

  const [edited, setEdited] = useState<{ id: string; draft: Draft } | null>(null);
  const draft: Draft | null = edited && edited.id === identity ? edited.draft : loaded;
  const dirty = !!draft && JSON.stringify(draft.data) !== JSON.stringify(draft.base);

  const patchData = (d: Draft["data"]) => {
    if (!draft) return;
    setEdited({ id: identity, draft: { ...draft, data: d } as Draft });
  };

  /* ---------------- 动作 ---------------- */
  const saveSection = useSaveSection(sel.section ?? "");
  const saveNode = useSaveNode(sel.node ?? "");
  const saveEp = useSaveEndpoint(search.node ?? "", search.ep ?? "");
  const epAction = useEndpointAction(search.node ?? "", search.ep ?? "");
  const nodeAction = useNodeAction(sel.node ?? "");
  const delEp = useDeleteEndpoint(search.node ?? "");
  const delNode = useDeleteNode();
  const genSamples = useGenerateSamples(search.node ?? "", search.ep ?? "");
  const genPreset = useGeneratePresetSamples(search.node ?? "", search.ep ?? "");
  const [generatingPreset, setGeneratingPreset] = useState<number | null>(null);

  const busy =
    saveSection.isPending ||
    saveNode.isPending ||
    saveEp.isPending ||
    epAction.isPending ||
    nodeAction.isPending;

  const fail = (e: unknown) => toastErr(messageOf(e));

  const onSave = async () => {
    if (!draft) return;
    try {
      if (draft.kind === "section") {
        await saveSection.mutateAsync(draft.data as DocsAdminSectionData);
      } else if (draft.kind === "node") {
        await saveNode.mutateAsync(draft.data as DocsAdminNodeData);
      } else {
        await saveEp.mutateAsync(draft.data as DocsAdminEndpointData);
      }
      toastOk("已保存，前台已更新");
    } catch (e) {
      // 409：别人先保存了。不要静默覆盖，也不要假装成功。
      if (isConflict(e)) {
        const ok = await confirmDialog({
          title: "这篇刚被别人改过",
          body: "加载最新内容会丢弃你现在的改动。要继续编辑，请先复制你的修改。",
          okText: "加载最新",
          cancelText: "留在这里",
        });
        if (ok) {
          void epQ.refetch();
          void nodeQ.refetch();
          void treeQ.refetch();
        }
        return;
      }
      fail(e);
    }
  };

  const onToggleStatus = async () => {
    if (!draft) return;
    try {
      if (draft.kind === "ep") {
        const published = epQ.data?.status === "published";
        const r = await epAction.mutateAsync(published ? "unpublish" : "publish");
        toastOk(published ? "已转为草稿" : "已发布（节点一并上线）");
        // 发布可能连带改节点：接口与节点的版本都变了，两份 base 都要重取。
        if (r?.node) void nodeQ.refetch();
      } else if (draft.kind === "node") {
        const published = nodeQ.data?.status === "published";
        await nodeAction.mutateAsync(published ? "unpublish" : "publish");
        toastOk(published ? "已转为草稿" : "已发布");
      }
    } catch (e) {
      fail(e);
    }
  };

  const onDelete = async () => {
    const isEp = draft?.kind === "ep";
    const ok = await confirmDialog({
      title: isEp ? "删除这篇接口文档？" : "删除这个节点？",
      body: isEp
        ? "删除后不可恢复，读者将看不到这一页。"
        : "节点下的全部接口都会一并删除，不可恢复。",
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    try {
      if (isEp) {
        await delEp.mutateAsync(draft.key);
      } else if (draft?.kind === "node") {
        await delNode.mutateAsync(draft.key);
      }
      setEdited(null);
      // 删接口 → 回到它所属的节点（左栏点进来的 URL 只有 node+ep，没有 s；
      // 用 `{s}` 会掉回"从左边选一篇"，等于把用户踢出这棵树）。
      // 删节点 / 板块 → 回到板块；板块 key 未知就回空态。
      const back: Record<string, string> = isEp
        ? sel.node
          ? { node: sel.node }
          : {}
        : sel.section
          ? { s: sel.section }
          : {};
      go({ search: () => back, replace: true });
      toastOk("已删除");
    } catch (e) {
      fail(e);
    }
  };

  /* ---------------- 新建 ---------------- */
  const [newNode, setNewNode] = useState<{ open: boolean; section: string }>({
    open: false,
    section: "",
  });
  const [newEp, setNewEp] = useState<{ open: boolean; node: string }>({ open: false, node: "" });

  /* ---------------- 页签 / 预览 ---------------- */
  const { tab, setTab } = usePageTabs(TABS, "edit", { param: "view" });
  const canPreview = draft?.kind === "ep" || (draft?.kind === "node" && nodeQ.data?.kind === "guide");
  const epPreview = useEndpointPreview(
    search.node ?? "",
    search.ep ?? "",
    draft?.kind === "ep" ? (draft.data as DocsAdminEndpointData) : null,
    tab === "preview",
  );
  const nodePreview = useNodePreview(
    sel.node ?? "",
    draft?.kind === "node" ? (draft.data as DocsAdminNodeData) : null,
    tab === "preview",
  );

  /* ---------------- 未保存离开提示 ---------------- */
  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  const pick = async (next: Selection) => {
    if (dirty) {
      const ok = await confirmDialog({
        title: "有未保存的改动",
        body: "切换对象后这些改动会丢失。",
        okText: "放弃修改",
        cancelText: "继续编辑",
        okVariant: "danger",
      });
      if (!ok) return;
    }
    setSideOpen(false);
    setEdited(null);
    go({ search: () => pickSearch(next), resetScroll: false });
  };

  const sectionOfSel = sel.node
    ? sections.find((s) => s.nodes.some((n) => n.key === sel.node))
    : sections.find((s) => s.key === sel.section);

  const headerTitle =
    draft?.kind === "ep"
      ? (draft.data.name || "未命名接口")
      : draft?.kind === "node"
        ? (draft.data.title || "未命名节点")
        : draft?.kind === "section"
          ? (draft.data.name || "未命名板块")
          : "";

  const headerMeta =
    draft?.kind === "ep"
      ? `${draft.data.method} ${draft.data.path}`
      : draft?.kind === "node"
        ? `${sectionOfSel?.name ?? ""} · ${nodeQ.data?.kind ?? ""}`
        : draft?.kind === "section"
          ? `板块 · ${draft.key}`
          : "";

  const problems =
    draft?.kind === "ep"
      ? (epQ.data?.problems ?? [])
      : draft?.kind === "node"
        ? (nodeQ.data?.problems ?? [])
        : [];

  return (
    <div className="adp-ws">
      <Rail
        tree={tree}
        loading={treeQ.isLoading}
        error={treeQ.error}
        onRetry={() => void treeQ.refetch()}
        sel={sel}
        open={open}
        drawer={drawer}
        drawerOpen={sideOpen}
        onCloseDrawer={() => setSideOpen(false)}
        onSelect={(next) => void pick(next)}
        onToggle={(level, key) => setOpen((prev) => toggle(prev, level, key))}
        onCreateEndpoint={(node) => setNewEp({ open: true, node })}
        onCreateNode={(section) => setNewNode({ open: true, section })}
      />

      <main className="adp-main">
        {!draft ? (
          <div className="adp-blank">
            {treeQ.isLoading ? (
              <EmptyState variant="loading" title="加载中…" />
            ) : (
              <EmptyState
                title="从左边选一篇"
                desc="板块 → 节点 → 接口。也可以先新建一个节点。"
                action={
                  <button
                    type="button"
                    className="btn btn-primary btn-sm"
                    onClick={() => setNewNode({ open: true, section: sections[0]?.key ?? "" })}
                  >
                    <Icon name="plus" size={14} />
                    新建节点
                  </button>
                }
              />
            )}
          </div>
        ) : (
          <>
            <TopBar
              icon={draft.kind === "section" ? "grid" : draft.kind === "node" ? "doc" : "rows"}
              title={headerTitle}
              meta={headerMeta}
              status={
                draft.kind === "ep"
                  ? (epQ.data?.status ?? "draft")
                  : draft.kind === "node"
                    ? (nodeQ.data?.status ?? "draft")
                    : "draft"
              }
              dirty={dirty}
              busy={busy}
              published={
                draft.kind === "ep"
                  ? epQ.data?.status === "published"
                  : draft.kind === "node"
                    ? nodeQ.data?.status === "published"
                    : false
              }
              canPreview={canPreview}
              railButton={
                drawer ? (
                  <button
                    type="button"
                    className="adp-openrail"
                    aria-label="打开文档目录"
                    onClick={() => setSideOpen(true)}
                  >
                    <Icon name="menu" size={16} />
                  </button>
                ) : undefined
              }
              tabs={
                <Tabs
                  items={TABS}
                  value={tab}
                  onChange={setTab}
                  variant="pill"
                  idPrefix={TAB_IDS}
                  label="编辑或预览"
                  panelId={`${TAB_IDS}-panel-${tab}`}
                />
              }
              onSave={() => void onSave()}
              onToggleStatus={() => void onToggleStatus()}
              onDelete={() => void onDelete()}
              onOpenPublic={
                draft.kind === "ep" && epQ.data?.status === "published"
                  ? () =>
                      window.open(
                        `/docs/${encodeURIComponent(sel.node ?? "")}/${encodeURIComponent(draft.key)}`,
                        "_blank",
                        "noopener",
                      )
                  : undefined
              }
            />

            <PageTabPanels value={tab} idPrefix={TAB_IDS} keepMounted>
              {(k) =>
                k === "edit" ? (
                  <div className="adp-body">
                    {draft.kind === "section" ? (
                      <SectionForm
                        section={sectionOfSel as DocsAdminSection}
                        data={draft.data as DocsAdminSectionData}
                        onChange={(d) => patchData(d)}
                      />
                    ) : draft.kind === "node" && nodeQ.data ? (
                      <NodeForm
                        node={nodeQ.data}
                        sections={sections}
                        data={draft.data as DocsAdminNodeData}
                        onChange={(d) => patchData(d)}
                      />
                    ) : draft.kind === "ep" && epQ.data ? (
                      <EndpointForm
                        ep={epQ.data}
                        data={draft.data as DocsAdminEndpointData}
                        transports={transportsQ.data ?? []}
                        onChange={(d) => patchData(d)}
                        onGenerateSamples={() => {
                          genSamples.mutate(draft.data as DocsAdminEndpointData, {
                            onSuccess: (r) => {
                              if (!r.samples) {
                                toastErr("没有可生成的字段示例值：先给字段填「示例值」");
                                return;
                              }
                              patchData({ ...(draft.data as DocsAdminEndpointData), samples: r.samples });
                              toastOk("已按当前字段示例值生成，保存后生效");
                            },
                            onError: (e) => fail(e),
                          });
                        }}
                        onGeneratePresetSamples={(i) => {
                          const d = draft.data as DocsAdminEndpointData;
                          const preset = d.presets[i];
                          setGeneratingPreset(i);
                          genPreset.mutate(preset.body, {
                            onSuccess: (r) => {
                              if (!r.samples) {
                                toastErr("这个用法还没有请求体，先生成不了示例");
                                return;
                              }
                              const next = d.presets.slice();
                              next[i] = { ...next[i], samples: r.samples };
                              patchData({ ...d, presets: next });
                              toastOk("已生成");
                            },
                            onError: (e) => fail(e),
                            onSettled: () => setGeneratingPreset(null),
                          });
                        }}
                        generating={genSamples.isPending}
                        generatingPreset={generatingPreset}
                      />
                    ) : null}

                    <ProblemsPanel problems={problems} />
                  </div>
                ) : (
                  <div className="adp-body adp-body--preview">
                    {draft.kind === "ep" ? (
                      epPreview.isError ? (
                        <EmptyState
                          variant="error"
                          title="预览生成失败"
                          desc={messageOf(epPreview.error)}
                          action={
                            <button type="button" className="btn btn-sm" onClick={() => void epPreview.refetch()}>
                              重试
                            </button>
                          }
                        />
                      ) : epPreview.data ? (
                        <DocsPreview doc={epPreview.data as DocV7Endpoint} baseUrl={tree?.base_url ?? ""} />
                      ) : (
                        <EmptyState variant="loading" title="正在生成预览…" />
                      )
                    ) : draft.kind === "node" && nodeQ.data?.kind === "guide" ? (
                      nodePreview.isError ? (
                        <EmptyState variant="error" title="预览生成失败" desc={messageOf(nodePreview.error)} />
                      ) : nodePreview.data ? (
                        <div className="d-root d-embed">
                          <GuideView node={nodePreview.data} />
                        </div>
                      ) : (
                        <EmptyState variant="loading" title="正在生成预览…" />
                      )
                    ) : (
                      <EmptyState title="这个对象没有预览" desc="板块只有名字与顺序，直接看左栏就是最终效果。" />
                    )}
                  </div>
                )
              }
            </PageTabPanels>
          </>
        )}
      </main>

      {/* 只在打开时**挂载**：Dialog 关闭时仍留在 DOM 里（靠 .hidden 藏），
          里面的输入框会与编辑表单抢同名标签，也让读屏多念一遍。 */}
      {newNode.open ? (
        <NewNodeDialog
          open
          sections={sections}
          section={newNode.section}
          onClose={() => setNewNode({ open: false, section: "" })}
          onCreated={(nodeKey) => {
            setNewNode({ open: false, section: "" });
            void treeQ.refetch();
            void pick({ node: nodeKey });
          }}
        />
      ) : null}
      {newEp.open ? (
        <NewEndpointDialog
          open
          node={newEp.node}
          onClose={() => setNewEp({ open: false, node: "" })}
          onCreated={(epKey, nodeKey) => {
            setNewEp({ open: false, node: "" });
            void treeQ.refetch();
            void pick({ node: nodeKey, ep: epKey });
          }}
        />
      ) : null}
    </div>
  );
}

/* ------------------------------ 新建弹窗 ------------------------------ */

const KINDS: [string, string][] = [
  ["family", "模型族"],
  ["protocol", "协议篇"],
  ["general", "通用接口"],
  ["guide", "说明页"],
];

function NewNodeDialog({
  open,
  sections,
  section,
  onClose,
  onCreated,
}: {
  open: boolean;
  sections: DocsAdminSection[];
  section: string;
  onClose: () => void;
  onCreated: (key: string) => void;
}) {
  const create = useCreateNode();
  const [key, setKey] = useState("");
  const [title, setTitle] = useState("");
  const [kind, setKind] = useState("general");
  const [sec, setSec] = useState(section);

  useEffect(() => {
    if (open) setSec(section || sections[0]?.key || "");
  }, [open, section, sections]);

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="新建节点"
      actions={[
        { label: "取消", onClick: onClose },
        {
          label: "创建",
          variant: "primary",
          disabled: !key.trim() || !title.trim() || create.isPending,
          onClick: () => {
            create.mutate(
              { key: key.trim(), kind, title: title.trim(), section: sec },
              {
                onSuccess: (n) => {
                  toastOk("已创建（草稿）");
                  setKey("");
                  setTitle("");
                  onCreated(n.key);
                },
                onError: (e) => toastErr(messageOf(e)),
              },
            );
          },
        },
      ]}
    >
      <div className="adp-form">
        <Field label="标题">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} />
        </Field>
        <Field label="标识（小写字母开头，建好后不能改）">
          <Input value={key} placeholder="如 images-jimeng" onChange={(e) => setKey(e.target.value)} />
        </Field>
        <Field label="类型">
          <Select items={KINDS.map(([v, l]) => [v, l] as [string, string])} value={kind} onChange={(e) => setKind(e.target.value)} />
        </Field>
        <Field label="板块">
          <Select
            items={sections.map((s) => [s.key, s.name] as [string, string])}
            value={sec}
            onChange={(e) => setSec(e.target.value)}
          />
        </Field>
      </div>
    </Dialog>
  );
}

function NewEndpointDialog({
  open,
  node,
  onClose,
  onCreated,
}: {
  open: boolean;
  node: string;
  onClose: () => void;
  onCreated: (ep: string, node: string) => void;
}) {
  const create = useCreateEndpoint(node);
  const [key, setKey] = useState("");
  const [name, setName] = useState("");

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="新建接口"
      actions={[
        { label: "取消", onClick: onClose },
        {
          label: "创建",
          variant: "primary",
          disabled: !key.trim() || !name.trim() || create.isPending,
          onClick: () => {
            create.mutate(
              {
                key: key.trim(),
                name: name.trim(),
                summary: "",
                method: "POST",
                path: "/v1/endpoint",
                transport_doc_id: "",
                aliases: [],
                run_mode: "json",
                run_hint: "",
                max_body: "",
                notes: [],
                hidden: false,
                sort: 0,
                version: 0,
                fields: [],
                // 新建就给一条 200：这是**用户看得见的数据**（不是后台偷补的），
                // 删光后再发布会被 E_NO_SUCCESS_VARIANT 拦下。
                response: {
                  kind: "json",
                  envelope: false,
                  content_type: "",
                  variants: [
                    { status: 200, label: "成功", content_type: "", kind: "json", envelope: false, returns: [], example: [], raw: "", notes: [] },
                  ],
                },
                samples: null,
                samples2: null,
                example_title: "",
                example_title2: "",
                presets: [],
                errors: [],
              },
              {
                onSuccess: (d) => {
                  toastOk("已创建（草稿）");
                  setKey("");
                  setName("");
                  onCreated(d.key, node);
                },
                onError: (e) => toastErr(messageOf(e)),
              },
            );
          },
        },
      ]}
    >
      <div className="adp-form">
        <Field label="接口名">
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="标识（小写字母开头，建好后不能改）">
          <Input value={key} placeholder="如 ep-generations" onChange={(e) => setKey(e.target.value)} />
        </Field>
      </div>
    </Dialog>
  );
}

/* ------------------------------ 纯函数 ------------------------------ */

/** 选中 → URL 参数（三个选中参数互斥，一次写全）。 */
function pickSearch(sel: Selection): Record<string, unknown> {
  if (sel.ep && sel.node) return { node: sel.node, ep: sel.ep };
  if (sel.node) return { node: sel.node };
  if (sel.section) return { s: sel.section };
  return {};
}

function sectionData(s: DocsAdminSection): DocsAdminSectionData {
  return {
    name: s.name,
    en: s.en,
    desc: s.desc,
    color: s.color,
    sort: s.sort,
    hidden: s.hidden,
    version: s.version,
  };
}

function nodeData(n: DocsAdminNode): DocsAdminNodeData {
  return {
    title: n.title,
    summary: n.summary,
    section: n.section,
    protocol: n.protocol,
    hidden: n.hidden,
    sort: n.sort,
    version: n.version,
    blocks: n.blocks,
  };
}

function epData(e: DocsAdminEndpoint): DocsAdminEndpointData {
  return {
    name: e.name,
    summary: e.summary,
    method: e.method,
    path: e.path,
    transport_doc_id: e.transport_doc_id,
    aliases: e.aliases,
    run_mode: e.run_mode,
    run_hint: e.run_hint,
    max_body: e.max_body,
    notes: e.notes,
    hidden: e.hidden,
    sort: e.sort,
    version: e.version,
    fields: e.fields,
    response: e.response,
    samples: e.samples,
    samples2: e.samples2,
    example_title: e.example_title,
    example_title2: e.example_title2,
    presets: e.presets,
    errors: e.errors,
  };
}

/** 错误 → 人话（后端把「哪里不对」写在 message 里，别吞掉）。 */
function messageOf(e: unknown): string {
  const anyErr = e as { message?: string; data?: { message?: string } };
  return anyErr?.data?.message || anyErr?.message || "操作失败，请重试";
}

function isConflict(e: unknown): boolean {
  const s = JSON.stringify(e ?? "");
  return s.includes("version_conflict") || s.includes("409");
}
