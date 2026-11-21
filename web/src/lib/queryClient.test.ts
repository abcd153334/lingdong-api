import { afterEach, describe, expect, it } from "vitest";
import { clearAccountQueries, isAccountQuery, queryClient } from "@/lib/queryClient";

/** 只构造 isAccountQuery 需要的字段（它有意只依赖 queryKey）。 */
const q = (key: readonly unknown[]) => ({ queryKey: key });

describe("isAccountQuery", () => {
  it("portal/admin 下的账户级数据必须清", () => {
    for (const key of [
      ["portal", "me"],
      ["portal", "keys"],
      // 按当前用户密钥返回"哪些模型可用"，是账户级 —— 放进白名单就会串号。
      ["portal", "defaults"],
      // 带个人已读标记，同样是账户级。
      ["portal", "announcements"],
      ["portal", "orders", 1],
      ["portal", "usage", "24h"],
      ["admin", "users"],
      ["admin", "config"],
    ]) {
      expect(isAccountQuery(q(key)), `${JSON.stringify(key)} 应被清`).toBe(true);
    }
  });

  it("站点级公开数据不清（清了会引发无意义的公开数据重取）", () => {
    for (const key of [["pricing"], ["portal", "config"], ["portal", "packages"]]) {
      expect(isAccountQuery(q(key)), `${JSON.stringify(key)} 应保留`).toBe(false);
    }
  });

  it("非 portal/admin 命名空间不受影响", () => {
    for (const key of [["docs", "guide"], ["call-media-blob", "abc"], []]) {
      expect(isAccountQuery(q(key))).toBe(false);
    }
  });
});

describe("clearAccountQueries", () => {
  afterEach(() => queryClient.clear());

  it("清掉账户级、保留公开级", () => {
    queryClient.setQueryData(["portal", "me"], { balance: 100 });
    queryClient.setQueryData(["portal", "keys"], [{ id: 1 }]);
    queryClient.setQueryData(["admin", "users"], [{ id: 1 }]);
    queryClient.setQueryData(["pricing"], { tiers: [] });
    queryClient.setQueryData(["portal", "config"], { currency: "CNY" });

    clearAccountQueries();

    expect(queryClient.getQueryData(["portal", "me"])).toBeUndefined();
    expect(queryClient.getQueryData(["portal", "keys"])).toBeUndefined();
    expect(queryClient.getQueryData(["admin", "users"])).toBeUndefined();
    // 公开数据必须还在，否则每次会话失效都会重取价目表与站点配置。
    expect(queryClient.getQueryData(["pricing"])).toEqual({ tiers: [] });
    expect(queryClient.getQueryData(["portal", "config"])).toEqual({ currency: "CNY" });
  });
});
