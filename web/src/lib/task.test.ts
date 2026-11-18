import { describe, expect, it } from "vitest";

import { isActiveTask, taskTimeoutLabel } from "./task";

describe("isActiveTask", () => {
  it("只有排队与运行算进行中", () => {
    expect(isActiveTask("queued")).toBe(true);
    expect(isActiveTask("running")).toBe(true);
  });

  it("四个终态与空串都不算进行中", () => {
    for (const s of ["succeeded", "failed", "expired", "canceled", ""]) {
      expect(isActiveTask(s)).toBe(false);
    }
  });
});

describe("taskTimeoutLabel", () => {
  it("只有 timeout 需要额外解释", () => {
    expect(taskTimeoutLabel("timeout")).toBe("轮询网络失败超限");
  });

  it("模态值原样不作解释（由 ModalityTag 展示）", () => {
    for (const k of ["video", "image", "audio", "music", ""]) {
      expect(taskTimeoutLabel(k)).toBe("");
    }
  });
});
