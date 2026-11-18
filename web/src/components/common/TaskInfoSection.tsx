import { CollapseSection } from "@/components/common/CollapseSection";
import { KV } from "@/components/common/DetailBits";
import { StatusTag } from "@/components/common/StatusTag";
import { TaskProgress } from "@/components/common/TaskProgress";
import { formatDateTime } from "@/lib/format";
import { isActiveTask, taskTimeoutLabel } from "@/lib/task";

/**
 * 任务信息（唯一实现）。
 *
 * 调用记录详情与异步任务详情共用同一个分区：同一份任务事实在两处必须字形一致，
 * 否则「调用记录说排队中、任务页说失败」这类不一致无法被用户解释。
 *
 * 入参就是**全站统一的任务对象**（types 的 TaskView）：调用记录里的 `task` 与
 * 任务端点下发的是同一个结构体，所以这里不需要适配层，也不需要兼容两种形状。
 */
export interface TaskInfoData {
  status: string;
  kind?: string;
  progress?: number;
  /** 分类文案（对象，成功为 null）；原文只在管理台的 tasks.error_msg 里 */
  error?: { message: string } | null;
  /** Unix 秒（不是 RFC3339） */
  created_at?: number;
  /** 只有终态才有 */
  completed_at?: number;
  deadline_at?: number;
  /** 只在详情接口下发；未加载时缺省（不是"0 个"） */
  contents?: { index: number }[];
}

export function TaskInfoSection({
  task,
  loading,
  rawError,
}: {
  task: TaskInfoData;
  loading?: boolean;
  /** 失败原文（仅管理台有）：排障要看上游说了什么，分类文案对定位没有信息量。 */
  rawError?: string;
}) {
  const n = task.contents?.length;
  const active = isActiveTask(task.status);
  const timeout = taskTimeoutLabel(task.kind ?? "");
  const errorText = rawError?.trim() || task.error?.message || "";
  // 摘要不带状态词：状态文案的唯一来源是 StatusTag，这里再抄一份就会漂移。
  const summary =
    n === undefined ? undefined : active ? `${task.progress ?? 0}% · ${n} 个产物` : `${n} 个产物`;

  return (
    <CollapseSection title="任务信息" defaultOpen={task.status !== "succeeded"} summary={summary}>
      <div className="cd-kv">
        <KV k="状态">
          <StatusTag status={task.status} />
        </KV>
        <KV k="进度">
          <TaskProgress status={task.status} progress={task.progress} />
        </KV>
        {n === undefined ? null : <KV k="产物">{loading ? "正在读取…" : `${n} 个`}</KV>}
        {timeout ? <KV k="失败类型">{timeout}</KV> : null}
        {errorText ? (
          <KV k="结束原因">
            <span className="small">{errorText}</span>
          </KV>
        ) : null}
        {task.created_at ? (
          <KV k="创建时间">
            <span className="mono small">{formatDateTime(task.created_at)}</span>
          </KV>
        ) : null}
        {task.completed_at ? (
          <KV k="完成时间">
            <span className="mono small">{formatDateTime(task.completed_at)}</span>
          </KV>
        ) : null}
        {task.deadline_at ? (
          <KV k="超时时间">
            <span className="mono small">{formatDateTime(task.deadline_at)}</span>
          </KV>
        ) : null}
      </div>
    </CollapseSection>
  );
}
