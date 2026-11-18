import { AgentWorkspace } from "@/components/admin/agent/AgentWorkspace";

/**
 * AI 接入助手（/admin/agent）。
 *
 * 页面本身只做组装：全高两栏工作台在 AgentWorkspace 里，页面没有任何内联布局
 * （M3/M4 门禁），也不直接请求接口（M5）。
 *
 * 这个页面**没有任何直接编辑渠道/模型的表单**，这是刻意的：
 * 助手只能产出提案，写入生产表必须由人在提案卡上确认。
 * 少一个编辑入口，就少一条绕过确认与审计的路径。
 */
export function AdminAgentPage() {
  return <AgentWorkspace />;
}
