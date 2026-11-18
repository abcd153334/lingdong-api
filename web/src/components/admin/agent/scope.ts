import { createContext, useContext } from "react";

/**
 * 当前助手实例的「入口作用域」（新建适配器入口 = `["adapter_spec"]`）。
 *
 * 为什么用 context 而不是三层 props：它是**环境**（这个工作台是从哪个按钮打开的），
 * 不是某个组件的数据。用 props 要穿透 AgentThread → AgentTurn → AgentProposalCard，
 * 只为了给一个按钮加一个字段，改一处要动三层签名。
 *
 * 为什么不存进提案：scope 是"这一次点击的语境"。存进库会让历史提案在换入口后行为漂移
 * （在助手页生成的提案，从适配器入口点应用就被拒 —— 用户完全无法理解）。
 */
const AgentScopeContext = createContext<readonly string[] | undefined>(undefined);

export const AgentScopeProvider = AgentScopeContext.Provider;

/** 读取入口作用域；undefined = 不限制（独立助手页）。 */
export function useAgentScope(): readonly string[] | undefined {
  return useContext(AgentScopeContext);
}
