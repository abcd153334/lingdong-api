import { useCallback, useMemo } from "react";
import { useNavigate, useSearch } from "@tanstack/react-router";

import { EmptyState } from "@/components/common/EmptyState";
import { PageHead } from "@/components/common/PageHead";
import { toastErr, toastOk } from "@/components/common/Toaster";
import { EndpointDetail } from "@/components/admin/apidocs/EndpointDetail";
import { EndpointNav } from "@/components/admin/apidocs/EndpointNav";
import { Reference } from "@/components/admin/apidocs/Reference";
import { apiDocsViewURL, exportApiDocFile, useApiDocCatalogQuery } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import type { ApiDocEndpoint } from "@/types";

/**
 * 开放接口手册（仅管理台可见）。
 *
 * 为什么不做成公开文档站：这一面是给**接入方**看的，而接入方是平台逐个拉进来的
 * 应用（应用由管理员创建、标识由管理员分配）。公开它会把这套接口的存在与完整
 * 权限模型暴露给任何访客；普通用户用门户，不需要知道 `client_order_no` 是什么。
 *
 * 数据源只有一个：后端 `apicontract`（运行期守卫与本文档同源）。
 * 三栏都只**排版**，不做二次加工（不重拼路径、不推断鉴权级别、不补默认参数）——
 * 否则文档会与守卫分叉，而那正是"照着文档写却拿到 403"的根源。
 *
 * 选中态放 URL（`?ep=`）：接入方问"这条怎么调"时，把地址发过去比让对方
 * 在一长串目录里找快得多。参数值与密钥**不进 URL**（会被粘进聊天记录）。
 */
export function AdminApiDocsPage() {
  const { data, isPending, error } = useApiDocCatalogQuery();
  // `strict: false` 与 pageTabs / Keys / Topup 同一写法：本页只关心 `ep` 一位，
  // 其余参数原样透传（`?create=1` 之类的既有约定不该被这里吃掉）。
  const search = useSearch({ strict: false }) as { ep?: string };
  const navigate = useNavigate();
  const ep = search.ep;

  const items = data?.items ?? [];

  /**
   * 当前展示哪一条。
   *
   * 优先用 URL 里的 `ep`；它不在目录里（手改了地址、或契约删了这条）时
   * 退回第一条 —— 而不是白屏。这也是唯一一处「URL 与数据不一致」的裁决点。
   */
  const active: ApiDocEndpoint | undefined = useMemo(() => {
    if (!items.length) return undefined;
    return items.find((e) => e.key === ep) ?? items[0];
  }, [items, ep]);

  const pick = useCallback(
    (key: string) => {
      // replace：切换目录不该在浏览器历史里堆 41 条记录（返回键会变成
      // "上一条接口"而不是"离开这一页"）。
      // `to: "."` = 留在当前路由，只改 search —— 不写死路径，路由表改动时不用同步。
      void navigate({ to: ".", search: { ep: key }, replace: true });
    },
    [navigate],
  );

  const exportFile = async (kind: "openapi" | "markdown") => {
    try {
      await exportApiDocFile(kind);
      toastOk("已开始下载");
    } catch (err) {
      toastErr(err instanceof ApiError ? err.message : "导出失败，请稍后重试");
    }
  };

  const openSwagger = () => {
    if (!data) return;
    // 必须新标签页直连后端：Swagger UI 要读管理台 Cookie 才拉得到 spec。
    // noopener 是必须的 —— 否则新窗口能通过 window.opener 反向操作本页。
    window.open(apiDocsViewURL(data.links.swagger_ui), "_blank", "noopener,noreferrer");
  };

  return (
    <>
      <PageHead
        title="开放接口"
        desc="接入方可用的用户侧接口契约：参数、返回、失败码与试运行"
      />

      {isPending ? <EmptyState variant="loading" title="正在加载接口契约" /> : null}
      {error ? (
        <EmptyState
          variant="error"
          title="接口契约加载失败"
          desc={error instanceof ApiError ? error.message : "请刷新页面重试"}
        />
      ) : null}

      {data && active ? (
        <div className="adx">
          <EndpointNav items={items} groups={data.groups} activeKey={active.key} onPick={pick} />
          <EndpointDetail e={active} />
          <Reference data={data} onExport={(k) => void exportFile(k)} onOpenSwagger={openSwagger} />
        </div>
      ) : null}

      {data && !active ? (
        <EmptyState title="没有可展示的接口" desc="契约目录为空，请检查后端配置。" />
      ) : null}
    </>
  );
}
