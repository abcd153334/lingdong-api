import { Link } from "@tanstack/react-router";
import { EmptyState } from "@/components/common/EmptyState";

/** 404 页（根路径与未匹配路由均落到此页） */
export function NotFound() {
  return (
    <div className="p-main">
      <EmptyState
        variant="empty"
        title="404"
        desc="页面不存在或已被移除"
        action={
          <Link to="/login" className="btn">
            返回登录
          </Link>
        }
      />
    </div>
  );
}
