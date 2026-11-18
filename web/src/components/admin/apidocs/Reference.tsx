import type { ApiDocCatalog } from "@/types";

/** 鉴权级别的读法（与后端 Auth 枚举一一对应）。 */
const AUTH_LEGEND: { key: keyof ApiDocCatalog["counts"]; label: string; why: string }[] = [
  { key: "open", label: "免鉴权", why: "注册、登录、找回密码；只需应用标识" },
  { key: "reader", label: "只读", why: "带密钥即可读，不改动任何数据" },
  { key: "writer", label: "受限写", why: "要密钥 + 对应权限点" },
  { key: "session", label: "仅会话票", why: "安全类操作，密钥不能代做" },
];

export interface ReferenceProps {
  data: ApiDocCatalog;
  onExport: (kind: "openapi" | "markdown") => void;
  onOpenSwagger: () => void;
}

/**
 * 右栏：把「读这一页之前要知道的事」放在一屏内。
 *
 * 为什么不塞进中栏底部：这些是**所有**端点共用的前提（鉴权级别怎么读、
 * 托管密钥能做什么、哪些能力故意没开放）。放进某一条端点的详情里，
 * 只有恰好点开那一条的人能看到；放在中栏底部则要滚过 41 条才能遇到。
 *
 * 级别的数量、权限集、不开放清单全部来自后端，本组件不写死任何一条：
 * 契约加一个权限点或改一条不开放理由，页面自动跟上。
 */
export function Reference({ data, onExport, onOpenSwagger }: ReferenceProps) {
  const { counts, hosted_scopes: scopes, not_exposed: hidden } = data;

  return (
    <aside className="adx-ref" aria-label="参考信息">
      <section className="adx-sec">
        <div className="h">
          <b>鉴权级别</b>
          <span>共 {counts.total} 条</span>
        </div>
        {AUTH_LEGEND.map((l) => (
          <div className="adx-kv" key={l.key}>
            <span className="k">
              {l.label} {counts[l.key]}
            </span>
            <span className="v">{l.why}</span>
          </div>
        ))}
      </section>

      <section className="adx-sec">
        <div className="h">
          <b>托管密钥</b>
          <span>默认权限集</span>
        </div>
        {scopes.length ? (
          <div className="adx-meta">
            {scopes.map((s) => (
              <code key={s}>{s}</code>
            ))}
          </div>
        ) : (
          <p className="muted">没有默认权限点。</p>
        )}
      </section>

      <section className="adx-sec">
        <div className="h">
          <b>导出与工具</b>
        </div>
        <div className="adx-lnk">
          <button type="button" className="btn" onClick={onOpenSwagger}>
            打开试调用页
          </button>
          <button type="button" className="btn" onClick={() => onExport("openapi")}>
            OpenAPI
          </button>
          <button type="button" className="btn" onClick={() => onExport("markdown")}>
            Markdown
          </button>
        </div>
        <p className="muted">
          导入 Postman / 生成 SDK 用 OpenAPI；贴进群里或 README 用 Markdown。两份与这一页同源。
        </p>
      </section>

      <section className="adx-sec">
        <div className="h">
          <b>没有开放的能力</b>
          <span>{hidden.length} 项</span>
        </div>
        {hidden.length ? (
          hidden.map((n) => (
            <div className="adx-kv" key={`${n.method}-${n.path}-${n.title}`}>
              <span className="k">{n.title}</span>
              <span className="v">{n.reason}</span>
            </div>
          ))
        ) : (
          <p className="muted">全部能力都已开放。</p>
        )}
      </section>
    </aside>
  );
}
