import { describe, expect, it } from "vitest";

import { imageHitOf } from "@/lib/imageHit";

/**
 * 流式出图的帧识别。
 *
 * 这条判据的三层都不能省，任何一层漏掉都是**静默**的：
 *  - 只看 url → `response_format=b64_json` 的流在预览区一张图都看不到；
 *  - 不看 type → 任意带 url 的帧（错误帧里的文档链接等）都会被当成产物；
 *  - 解析失败就抛 → 心跳帧会让整条调试台报错。
 */
describe("imageHitOf", () => {
  it("partial_succeeded 的 url 帧算一张图", () => {
    const hit = imageHitOf(
      JSON.stringify({
        type: "image_generation.partial_succeeded",
        url: "https://cdn.example.com/a.png",
        image_index: 0,
      }),
    );
    expect(hit).toEqual({ url: "https://cdn.example.com/a.png", kind: "image" });
  });

  it("b64_json 帧也要算（拼成 data URI）", () => {
    const hit = imageHitOf(
      JSON.stringify({ type: "image_generation.partial_succeeded", b64_json: "iVBORw0KGgo=" }),
    );
    expect(hit?.kind).toBe("image");
    expect(hit?.url.startsWith("data:image/png;base64,")).toBe(true);
  });

  it("非 image_generation 类型的 url 帧不算产物", () => {
    expect(imageHitOf(JSON.stringify({ type: "error", url: "https://cdn.example.com/x.png" }))).toBeNull();
  });

  it("非 JSON 帧（心跳 / 纯文本）返回 null 而不是抛错", () => {
    expect(imageHitOf(": keep-alive")).toBeNull();
    expect(imageHitOf("")).toBeNull();
  });

  it("url 不是 http(s) 时不当成可预览产物", () => {
    expect(imageHitOf(JSON.stringify({ type: "image_generation.completed", url: "/v1/x" }))).toBeNull();
  });

  it("completed 帧不带 url/b64 时返回 null（usage-only 帧）", () => {
    expect(
      imageHitOf(JSON.stringify({ type: "image_generation.completed", usage: { generated_images: 2 } })),
    ).toBeNull();
  });
});
