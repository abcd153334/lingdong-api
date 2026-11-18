import { useState } from "react";
import { Dialog } from "@/components/common/Dialog";
import { Field } from "@/components/common/Field";
import { Input } from "@/components/common/Input";
import { Textarea } from "@/components/common/Textarea";
import { Icon } from "@/components/common/Icon";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { ApiError } from "@/lib/apiClient";
import {
  useAgentDocPreviewQuery,
  useAgentDocsQuery,
  useCreateAgentDocMutation,
  useDeleteAgentDocMutation,
} from "@/lib/api/agent";
import { cn } from "@/lib/utils";
import { asArray } from "@/lib/array";

export interface AgentDocsDialogProps {
  open: boolean;
  onClose: () => void;
  quoted: number[];
  onToggleQuote: (id: number) => void;
}

function kb(n: number): string {
  return n >= 1024 * 100 ? `${Math.round(n / 1024)} KB` : `${Math.max(1, Math.round(n / 1024))} KB`;
}

/** 单份文档的预览区（懒加载：展开才请求正文）。 */
function DocPreview({ id }: { id: number }) {
  const pv = useAgentDocPreviewQuery(id, true);
  if (pv.isLoading) return <div className="agent-docs__preview">正在读取…</div>;
  if (pv.isError || !pv.data) return <div className="agent-docs__preview">预览读不到</div>;
  return (
    <div className="agent-docs__preview">
      {asArray(pv.data.sections).length > 0 ? (
        <div className="agent-docs__toc">
          {pv.data.sections.slice(0, 8).map((s) => (
            <span key={s.index} data-level={s.level}>
              {s.title}
            </span>
          ))}
        </div>
      ) : null}
      <pre>{pv.data.text}</pre>
      {pv.data.truncated ? <div className="agent-docs__more">只显示开头部分，助手读取时可分段</div> : null}
    </div>
  );
}

/**
 * 素材文档：粘贴或按链接抓取上游文档，并勾选本轮要引用的那几份。
 *
 * 文档**不进提示词全文**：这里存的是"素材库"，助手按段读取（docs_read）。
 * 一份几十 KB 的厂商文档整份塞进上下文，既贵又会让模型忽略后半段。
 */
export function AgentDocsDialog({ open, onClose, quoted, onToggleQuote }: AgentDocsDialogProps) {
  const docs = useAgentDocsQuery();
  const create = useCreateAgentDocMutation();
  const remove = useDeleteAgentDocMutation();
  const [mode, setMode] = useState<"paste" | "url">("paste");
  const [title, setTitle] = useState("");
  const [content, setContent] = useState("");
  const [url, setUrl] = useState("");
  const [previewId, setPreviewId] = useState(0);
  const [err, setErr] = useState("");
  const [ok, setOk] = useState("");

  const items = docs.data?.items ?? [];
  const limits = docs.data?.limits;

  const submit = () => {
    setErr("");
    setOk("");
    if (mode === "paste" && !content.trim()) {
      setErr("请粘贴文档内容");
      return;
    }
    if (mode === "url" && !url.trim()) {
      setErr("请填写文档链接");
      return;
    }
    create.mutate(
      {
        title: title.trim() || undefined,
        source_type: mode,
        content: mode === "paste" ? content : undefined,
        url: mode === "url" ? url.trim() : undefined,
      },
      {
        onSuccess: (res) => {
          setOk(res.duplicate ? "这份内容已存在，已复用原有记录" : `已保存（${kb(res.item.size_bytes)}）`);
          setContent("");
          setUrl("");
          setTitle("");
        },
        onError: (e) => setErr(e instanceof ApiError ? e.message : "保存失败，请重试"),
      },
    );
  };

  const askDelete = async (id: number, name: string) => {
    const okDel = await confirmDialog({
      title: "删除这份文档？",
      body: `「${name}」删除后助手不能再读取它，已生成的提案不受影响。`,
      okText: "删除",
      okVariant: "danger",
    });
    if (okDel) remove.mutate(id);
  };

  return (
    <Dialog open={open} onClose={onClose} title="素材文档" size="wide" portal layer={2}>
      <div className="agent-dlg" data-testid="agent-docs">
        <p className="agent-dlg__lead">
          勾选的文档会作为本轮参考。助手按段读取，不会把整份塞进对话。
        </p>

        {items.length === 0 ? (
          <div className="agent-dlg__empty">还没有素材文档</div>
        ) : (
          <ul className="agent-docs__list">
            {items.map((d) => {
              const on = quoted.includes(d.id);
              return (
                <li key={d.id} className={cn("agent-docs__item", on && "is-on")}>
                  <button
                    type="button"
                    className="agent-docs__check"
                    aria-pressed={on}
                    onClick={() => onToggleQuote(d.id)}
                    data-testid={`agent-doc-pick-${d.id}`}
                  >
                    {on ? <Icon name="check" size={12} /> : null}
                  </button>
                  <button
                    type="button"
                    className="agent-docs__main"
                    onClick={() => setPreviewId((v) => (v === d.id ? 0 : d.id))}
                    aria-expanded={previewId === d.id}
                  >
                    <span className="agent-docs__title">{d.title}</span>
                    <span className="agent-docs__meta">
                      {d.source_type === "url" ? "链接" : "粘贴"} · {kb(d.size_bytes)}
                    </span>
                  </button>
                  <button
                    type="button"
                    className="agent-iconbtn agent-iconbtn--sm"
                    aria-label={`删除 ${d.title}`}
                    onClick={() => void askDelete(d.id, d.title)}
                    disabled={remove.isPending}
                    data-testid={`agent-doc-del-${d.id}`}
                  >
                    <Icon name="trash" size={13} />
                  </button>
                  {previewId === d.id ? <DocPreview id={d.id} /> : null}
                </li>
              );
            })}
          </ul>
        )}

        <div className="agent-docs__add">
          <div className="agent-seg">
            <button
              type="button"
              className={cn(mode === "paste" && "is-on")}
              onClick={() => setMode("paste")}
            >
              粘贴文本
            </button>
            <button
              type="button"
              className={cn(mode === "url" && "is-on")}
              onClick={() => setMode("url")}
            >
              按链接抓取
            </button>
          </div>

          <div className="form-grid">
            <Field label="标题（可留空自动取）" span>
              <Input
                value={title}
                onChange={(e) => setTitle(e.target.value)}
                placeholder="例如：可灵视频接口文档"
              />
            </Field>
            {mode === "paste" ? (
              <Field
                label="文档内容"
                span
                hint={limits ? `单份上限 ${Math.round(limits.max_bytes / 1024)} KB` : undefined}
              >
                <Textarea
                  rows={4}
                  value={content}
                  onChange={(e) => setContent(e.target.value)}
                  placeholder="把厂商文档的正文粘进来"
                  data-testid="agent-doc-content"
                />
              </Field>
            ) : (
              <Field
                label="文档链接"
                span
                hint={limits ? `只支持公开的文本网页，抓取上限 ${Math.round(limits.max_fetch_bytes / 1024)} KB` : undefined}
              >
                <Input
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                  placeholder="https://…"
                  data-testid="agent-doc-url"
                />
              </Field>
            )}
          </div>

          {err ? <div className="agent-alert agent-alert--bad">{err}</div> : null}
          {ok ? <div className="agent-alert agent-alert--ok">{ok}</div> : null}

          <div className="agent-docs__actions">
            <button
              type="button"
              className="btn btn-primary btn-sm"
              onClick={submit}
              disabled={create.isPending}
              data-testid="agent-doc-save"
            >
              {create.isPending ? "保存中…" : "保存文档"}
            </button>
          </div>
        </div>
      </div>
    </Dialog>
  );
}
