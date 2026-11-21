import { useEffect, useRef, useState } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import {
  adminNav,
  adminSectionIdOf,
  matchPath,
  readCollapsedSections,
  writeCollapsedSections,
} from "@/lib/nav";
import { useIsMobile } from "@/lib/responsive";
import { useSite } from "@/lib/site";
import { cn } from "@/lib/utils";
import { Icon } from "@/components/common/Icon";
import { SiteLogo } from "@/components/common/SiteLogo";
import { useOverlayBehavior } from "@/components/common/useOverlayBehavior";
import { ReleaseDialog } from "@/components/admin/ReleaseDialog";
import { useAdminReleaseQuery } from "@/lib/api";

export interface AdminSidebarProps {
  /** ≤767px 抽屉展开态（≥768px 恒为常驻侧栏，本值被忽略） */
  open: boolean;
  onClose: () => void;
}

/**
 * 管理台侧栏（≥768px 固定；≤767px 汉堡抽屉）。
 *
 * 抽屉态与全站弹层（`Dialog` / `Drawer` / 文档站侧栏）走**同一条**行为链
 * `useOverlayBehavior`：Esc 关闭、Tab 焦点环闭合、打开时聚焦、关闭后焦点归还、
 * 页面滚动锁。此前只有一条裸 `.mask`，锁滚动/键盘/读屏一样都没有。
 *
 * 三个必须区分「常驻」与「弹层」的点（照抄 CSS 会漏）：
 *  - `role="dialog"` + `aria-modal` 只能在抽屉态下出现。常驻侧栏是页面骨架，
 *    标成 modal 会让读屏把整页其余内容判为不可达。
 *  - `inert` 同理：抽屉关闭时它只是被 `translateX(-105%)` 移出视口，DOM 仍在，
 *    键盘 Tab 能一路走进「看不见的菜单」。只在移动端关闭态加 `inert`。
 *  - 滚动锁 / 焦点陷阱只在抽屉态生效，宽屏上不该有（否则一进页面焦点就被抢走）。
 *
 * 抽屉的可访问名用「导航菜单」而不是「管理台导航」：内层 `<nav>` 已经叫后者，
 * 两层同名读屏会念两遍还分不清谁是谁。
 *
 * 分组折叠（≤1279px 也生效，与抽屉无关）：默认全展开，收起状态写 localStorage；
 * 折叠只加 `.is-collapsed{display:none}` 类、**不移除节点** —— 15 条 `nav a` 是
 * 全站 10 处断言的基座，删了节点一半 e2e 直接红。
 *
 * 层级（组头 vs 子项）只由结构与字重表达，**不靠缩小字号**：组头是 `.nav-group`
 * （600 字重、可折叠），子项装在 `.nav-sub` 里（缩进 + 1px 导轨），两者的行骨架
 * `.nav-row` 共用同一套度量。顶级项（无标题分区：仪表盘 / 系统配置 / 模型服务 /
 * 用户管理 / 应用管理）**不进 `.nav-sub`**，否则这些一级入口会被当成某组的子项缩进进去。
 */
export function AdminSidebar({ open, onClose }: AdminSidebarProps) {
  const { pathname } = useLocation();
  const site = useSite();
  const isMobile = useIsMobile();
  /** 抽屉态 = 移动端断点下被打开。宽屏上 `open` 无意义（汉堡按钮本身不可见）。 */
  const overlay = isMobile && open;
  const ref = useRef<HTMLElement>(null);
  useOverlayBehavior(overlay, onClose, ref);

  /** 版本与更新弹窗（入口是底栏的版本号）。 */
  const [releaseOpen, setReleaseOpen] = useState(false);
  const release = useAdminReleaseQuery();
  const version = release.data?.current.version ?? "";
  const updateAvailable = Boolean(release.data?.manifest.update_available);
  const verLabel = updateAvailable
    ? `当前版本 ${version || "未知"}，有新版本可用`
    : `当前版本 ${version || "未知"}`;

  /** 已收起的分区 id；空数组 = 全展开（默认）。 */
  const [collapsed, setCollapsed] = useState<string[]>(readCollapsedSections);
  const currentSection = adminSectionIdOf(pathname);

  // 进入「某分区内的页面」时自动展开：书签 / 刷新 / 浏览器后退都可能落在已收起的分区里，
  // 不展开就只剩面包屑知道自己在哪。
  // 依赖只有 currentSection ⇒ 用户手动收起当前分区不会被顶回来（pathname 没变，effect 不重跑）。
  useEffect(() => {
    if (!currentSection) return;
    setCollapsed((prev) =>
      prev.includes(currentSection) ? prev.filter((id) => id !== currentSection) : prev,
    );
  }, [currentSection]);

  // 唯一写出口：状态一变就落盘（首帧顺带把脏数据规范化）。
  useEffect(() => {
    writeCollapsedSections(collapsed);
  }, [collapsed]);

  const toggleSection = (id: string) =>
    setCollapsed((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));

  return (
    <>
      {/* 遮罩不再用 `lg:hidden` 兜：那个断点(1024)与侧栏抽屉化断点(767)是两把尺子，
          768~1023 之间会盖住整页。渲染条件直接由 overlay 决定。 */}
      {overlay ? <div className="mask" onClick={onClose} /> : null}
      <aside
        ref={ref}
        className={cn("a-side", open && "open")}
        inert={isMobile && !open}
        role={overlay ? "dialog" : undefined}
        aria-modal={overlay ? true : undefined}
        aria-label={overlay ? "导航菜单" : undefined}
      >
        <div className="a-side-head">
          {/* 品牌链接同样要收抽屉：它是全抽屉唯一不经过 nav 的跳转入口，
              漏了它就会「点了 logo 换了页，遮罩和焦点锁还挂在上面」。 */}
          <Link to="/admin/dashboard" className="logo" onClick={onClose}>
            <SiteLogo />
            <span className="a-side-name">{site.name}</span>
          </Link>
          {/* 抽屉态必须有可聚焦的关闭入口：焦点被圈在抽屉里，顶栏那个汉堡按钮
              在遮罩下面，键盘与读屏用户除 Esc 之外无路可退。 */}
          <button type="button" className="icon-btn a-side-close" aria-label="关闭导航" onClick={onClose}>
            <Icon name="close" size={18} />
          </button>
        </div>
        <nav aria-label="管理台导航">
          {adminNav.map((section) => {
            const expanded = !section.title || !collapsed.includes(section.id);
            const itemsId = `nav-sec-${section.id}`; // 全站只有这一个侧栏实例，不用 useId
            return (
              <div className="nav-sec" key={section.id}>
                {section.title ? (
                  <button
                    type="button"
                    // 当前页就在这组里、而这组被收起了 → 只靠字色提示，不额外造元素
                    className={cn(
                      "nav-row nav-group",
                      !expanded && currentSection === section.id && "on",
                    )}
                    aria-expanded={expanded}
                    aria-controls={itemsId}
                    // 绝不调 onClose：移动端点一下组头会把整个抽屉关掉
                    onClick={() => toggleSection(section.id)}
                  >
                    <Icon name={section.icon} size={17} />
                    {section.title}
                    <Icon
                      name="chevronDown"
                      size={14}
                      // 收起时转成 ▸（与文档站 .dc-side-caret 同一套手势），展开时朝下
                      className={cn("nav-caret", !expanded && "closed")}
                    />
                  </button>
                ) : null}

                {/* 折叠只加类、不移除节点：nav a 恒 15 条（计数断言与跳转后高亮都依赖它）。
                    `.nav-sub` 只给**有组标题**的分区：顶级项是同级入口，不能缩进成别人的子项。 */}
                <div
                  id={itemsId}
                  className={cn(
                    "nav-items",
                    Boolean(section.title) && "nav-sub",
                    !expanded && "is-collapsed",
                  )}
                >
                  {section.items.map((item) => {
                    // 前缀匹配：子路径页面（如带查询参数之外的运行期深链）要落在
                    // 所属分区上，否则从那里进来侧栏一片无高亮（看起来像不属于管理台）。
                    const active = matchPath(item.path, pathname);
                    return (
                      <Link
                        key={item.path}
                        to={item.path}
                        className={cn("nav-row nav-item", active && "active")}
                        aria-current={active ? "page" : undefined}
                        onClick={onClose}
                      >
                        <Icon name={item.icon} size={17} />
                        {item.title}
                      </Link>
                    );
                  })}
                </div>
              </div>
            );
          })}
        </nav>
        {/* 底栏固定行：对外 API 文档入口 + 版本号。
            为什么放在 nav 之外：管理台导航是「能改数据的页面」，文档站是只读的对外面，
            混进 nav 会让「仪表盘/系统配置…」的计数与分区断言全部变动，也让人误以为
            它是又一个后台页。放在底栏一级链接，与门户页脚那个「API 文档」同一个去处。 */}
        <div className="foot">
          <Link to="/docs" className="a-side-doc" onClick={onClose}>
            <Icon name="book" size={16} />
            API 文档
          </Link>
          {/* 版本号就是"版本与更新"的入口：后台里唯一与"这台机器跑的是哪一版"
              直接相关的信息。放在底栏而不是新开一个后台页 —— 否则导航分区与
              全站的 nav 计数断言都要跟着变。
              数据来自 /admin/system/release（只读缓存，不出站）；拉不到时只显示站名。 */}
          <button
            type="button"
            className="a-side-ver a-side-ver--btn"
            onClick={() => {
              setReleaseOpen(true);
              onClose();
            }}
            aria-label={verLabel}
            title={verLabel}
          >
            灵动人{version ? ` ${version}` : ""}
            {updateAvailable ? <i className="a-side-dot" aria-hidden /> : null}
          </button>
        </div>
      </aside>
      {/* 只在打开时挂载：Dialog 关闭态仍会渲染一个 .mask（隐藏），
          而侧栏是全局 chrome —— 常驻一个隐藏遮罩会让全站 `.mask` 计数断言变红。 */}
      {releaseOpen ? <ReleaseDialog open onClose={() => setReleaseOpen(false)} /> : null}
    </>
  );
}
