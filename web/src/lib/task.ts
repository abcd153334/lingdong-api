/**
 * 异步任务的前端规则（唯一实现）。
 *
 * 与后端 `model.TaskProgress` / `model.TaskPortalError` 一样，这里只放"同一份事实"，
 * 页面不得再内联 `status === "queued" || status === "running"`：那样一旦将来多一个
 * 中间态，列表金额口径、轮询条件与按钮可见性会各自漂移。
 */

/** 仍在推进的状态（决定轮询、金额口径与可取消性）。 */
const ACTIVE_STATUS = new Set(["queued", "running"]);

export function isActiveTask(status: string): boolean {
  return ACTIVE_STATUS.has(status);
}

/**
 * 任务类型的**诊断**文案。
 *
 * `kind` 已改为模态（提交时写 modality），正常任务与 `modality` 同值，
 * 由 `ModalityTag` 统一展示；唯一需要额外解释的是历史诊断标记 `timeout`
 * （轮询网络失败超限，任务被判失败但根因不在业务参数）。
 */
export function taskTimeoutLabel(kind: string): string {
  return kind === "timeout" ? "轮询网络失败超限" : "";
}
