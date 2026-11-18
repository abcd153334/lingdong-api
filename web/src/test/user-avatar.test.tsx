import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { UserAvatar } from "@/components/common/UserAvatar";

/** 默认头像由 Vite 处理成带 hash 的产物路径，断言只认文件名主干。 */
const isDefault = (src: string) => src.includes("avatar-default");

describe("UserAvatar", () => {
  it("没有自定义头像时渲染打包进产物的默认图", () => {
    const { container } = render(<UserAvatar />);
    const img = container.querySelector("img");
    expect(img).toBeTruthy();
    expect(isDefault(img!.getAttribute("src") ?? "")).toBe(true);
  });

  it("有自定义头像时用自定义地址", () => {
    const url = "data:image/webp;base64,AAAA";
    const { container } = render(<UserAvatar url={url} />);
    expect(container.querySelector("img")?.getAttribute("src")).toBe(url);
  });

  it("自定义图加载失败时回落默认图", () => {
    const url = "data:image/webp;base64,BROKEN";
    const { container } = render(<UserAvatar url={url} />);
    const img = container.querySelector("img")!;
    fireEvent.error(img);
    expect(isDefault(container.querySelector("img")?.getAttribute("src") ?? "")).toBe(true);
  });

  it("默认图自身加载失败不会再触发回落（防 onError 死循环）", () => {
    const { container } = render(<UserAvatar />);
    const before = container.querySelector("img")!.getAttribute("src");
    fireEvent.error(container.querySelector("img")!);
    const after = container.querySelector("img")!.getAttribute("src");
    expect(after).toBe(before);
    expect(isDefault(after ?? "")).toBe(true);
  });

  it("url 变化会重置失败态（换过坏图后新图仍能显示）", () => {
    const { container, rerender } = render(<UserAvatar url="data:image/webp;base64,BROKEN" />);
    fireEvent.error(container.querySelector("img")!);
    expect(isDefault(container.querySelector("img")!.getAttribute("src") ?? "")).toBe(true);

    const next = "data:image/webp;base64,GOOD";
    rerender(<UserAvatar url={next} />);
    expect(container.querySelector("img")?.getAttribute("src")).toBe(next);
  });

  it("装饰性渲染：容器 aria-hidden，可访问名由外层按钮提供", () => {
    const { container } = render(<UserAvatar size="lg" />);
    const wrap = container.querySelector(".u-avatar")!;
    expect(wrap.getAttribute("aria-hidden")).toBe("true");
    expect(wrap.className).toContain("u-avatar--lg");
    expect(screen.queryByRole("img")).toBeNull();
  });
});
