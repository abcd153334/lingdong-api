import { describe, expect, it } from "vitest";
import {
  DUOMI_DEFAULT_AUTH_PREFIX,
  DUOMI_DEFAULT_METADATA_ALLOW,
  DUOMI_DEFAULT_PATHS,
  DUOMI_PATH_KEYS,
  DUOMI_PROTOCOLS,
  buildDuomiOptions,
  duomiKlingPrefixHint,
  duomiOptionsToForm,
  emptyDuomiForm,
} from "@/lib/duomiOptions";

describe("多米 私有配置表单", () => {
  it("空表单：只显式下发 metadata_allow（其余键跟随内置默认）", () => {
    const r = buildDuomiOptions(emptyDuomiForm());
    expect(r.errors).toEqual({});
    expect(r.value).toEqual({ metadata_allow: [...DUOMI_DEFAULT_METADATA_ALLOW] });
    // 三态/可选键不得出现：它们的缺省等于「用内置默认」
    expect("auth_prefix" in r.value).toBe(false);
    expect("paths" in r.value).toBe(false);
    expect("model_overrides" in r.value).toBe(false);
  });

  it("每次调用返回全新对象（避免抽屉实例互相污染）", () => {
    const a = emptyDuomiForm();
    const b = emptyDuomiForm();
    a.paths["kling.prefix"] = "/custom/";
    a.metadataAllow.push("generate_audio");
    a.authMode = "none";
    expect(b.paths["kling.prefix"]).toBe("");
    expect(b.metadataAllow).toEqual([...DUOMI_DEFAULT_METADATA_ALLOW]);
    expect(b.authMode).toBe("default");
  });

  describe("auth_prefix 三态", () => {
    it("default 不下发（跟随内置默认 Bearer␠）", () => {
      const f = emptyDuomiForm();
      f.authMode = "default";
      expect("auth_prefix" in buildDuomiOptions(f).value).toBe(false);
    });

    it("none 下发保留值 -（后端归一成空前缀）", () => {
      const f = emptyDuomiForm();
      f.authMode = "none";
      expect(buildDuomiOptions(f).value.auth_prefix).toBe("-");
    });

    it("custom 原样下发，不裁剪尾随空格", () => {
      const f = emptyDuomiForm();
      f.authMode = "custom";
      f.authCustom = "Token ";
      expect(buildDuomiOptions(f).value.auth_prefix).toBe("Token ");
    });

    it("custom 的空串 / 保留值 - / 换行 / 超长都就地报错", () => {
      const f = emptyDuomiForm();
      f.authMode = "custom";

      f.authCustom = "   ";
      expect(buildDuomiOptions(f).errors.auth_prefix).toContain("不能为空");

      f.authCustom = "-";
      expect(buildDuomiOptions(f).errors.auth_prefix).toContain("保留值");

      f.authCustom = "Bear\ner";
      expect(buildDuomiOptions(f).errors.auth_prefix).toContain("换行");

      f.authCustom = "x".repeat(33);
      expect(buildDuomiOptions(f).errors.auth_prefix).toContain("32");
    });
  });

  describe("回填", () => {
    it("Bearer␠ 回填为 default，Bearer（少空格）回填为 custom（不得静默改写）", () => {
      expect(duomiOptionsToForm({ auth_prefix: DUOMI_DEFAULT_AUTH_PREFIX }).authMode).toBe("default");
      const bare = duomiOptionsToForm({ auth_prefix: "Bearer" });
      expect(bare.authMode).toBe("custom");
      expect(bare.authCustom).toBe("Bearer");
      // 一保存必须原样写回去，不能变成 "Bearer "
      expect(buildDuomiOptions(bare).value.auth_prefix).toBe("Bearer");
    });

    it('"-" 与 "" 都回填为「无前缀」', () => {
      expect(duomiOptionsToForm({ auth_prefix: "-" }).authMode).toBe("none");
      expect(duomiOptionsToForm({ auth_prefix: "" }).authMode).toBe("none");
    });

    it("metadata_allow 去重并丢掉未知键；空数组保留为「全部拒绝」", () => {
      expect(
        duomiOptionsToForm({ metadata_allow: ["aigc_watermark", "aigc_watermark", "unknown_key"] })
          .metadataAllow,
      ).toEqual(["aigc_watermark"]);
      // [] 是合法且必须保留的语义：一个都不透传（不能回落成默认放行）
      const none = duomiOptionsToForm({ metadata_allow: [] });
      expect(none.metadataAllow).toEqual([]);
      expect(buildDuomiOptions(none).value.metadata_allow).toEqual([]);
    });

    it("paths 原样保留非法值（由校验报错，不得悄悄改回默认）", () => {
      const f = duomiOptionsToForm({ paths: { "kling.prefix": "no-leading-slash" } });
      expect(f.paths["kling.prefix"]).toBe("no-leading-slash");
      expect(buildDuomiOptions(f).errors["paths.kling.prefix"]).toContain("/");
    });

    it("未知顶层键被记录下来并要求运营处理（不得静默丢弃）", () => {
      // suno_defaults 从 B6-1 起是**已知**键（渠道级音乐默认），不能再拿它当未知键样本；
      // 这里用一个真正不存在的键，保证「手工改库塞进来的键」这条护栏仍被测到。
      const f = duomiOptionsToForm({ metadata_allow: [], suno_default: { model: "x" } });
      expect(f.unknownKeys).toEqual(["suno_default"]);
      const r = buildDuomiOptions(f);
      expect(r.errors.adapter_options).toContain("suno_default");
    });

    it("非对象原文不白屏（落到默认表单）", () => {
      expect(duomiOptionsToForm(null).metadataAllow).toEqual([...DUOMI_DEFAULT_METADATA_ALLOW]);
      expect(duomiOptionsToForm([]).authMode).toBe("default");
      expect(duomiOptionsToForm("oops").paths["kling.prefix"]).toBe("");
    });
  });

  describe("paths", () => {
    it("与默认值相同则不下发（避免把默认值钉死在库里）", () => {
      const f = emptyDuomiForm();
      f.paths["seedance.submit"] = DUOMI_DEFAULT_PATHS["seedance.submit"];
      f.paths["kling.prefix"] = DUOMI_DEFAULT_PATHS["kling.prefix"];
      expect("paths" in buildDuomiOptions(f).value).toBe(false);
    });

    it("只下发改动过的那一个键", () => {
      const f = emptyDuomiForm();
      f.paths["kling.prefix"] = "/api/v2/videos/";
      expect(buildDuomiOptions(f).value.paths).toEqual({ "kling.prefix": "/api/v2/videos/" });
    });

    it("含 ? / # / 超长都就地报错", () => {
      const f = emptyDuomiForm();
      f.paths["kling.prefix"] = "/a?b";
      expect(buildDuomiOptions(f).errors["paths.kling.prefix"]).toContain("?");
      f.paths["kling.prefix"] = "/a#b";
      expect(buildDuomiOptions(f).errors["paths.kling.prefix"]).toContain("#");
      f.paths["kling.prefix"] = "/" + "a".repeat(200);
      expect(buildDuomiOptions(f).errors["paths.kling.prefix"]).toContain("200");
    });

    it("末尾不是 / 只给非阻断提示", () => {
      expect(duomiKlingPrefixHint("")).toBe("");
      expect(duomiKlingPrefixHint(DUOMI_DEFAULT_PATHS["kling.prefix"])).toBe("");
      expect(duomiKlingPrefixHint("/api/kling/")).toBe("");
      expect(duomiKlingPrefixHint("/api/kling")).toContain("任务号");
    });
  });

  describe("model_overrides", () => {
    it("留空不下发", () => {
      const f = emptyDuomiForm();
      f.overridesText = "   ";
      const r = buildDuomiOptions(f);
      expect(r.errors).toEqual({});
      expect("model_overrides" in r.value).toBe(false);
    });

    it("合法覆盖按白名单重建（只保留 4 个子键）", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({
        "seedance-lite": {
          protocol: "seedance",
          mode: "pro",
          upstream_model: "doubao-seedance-lite",
          duration_as_string: true,
        },
      });
      const r = buildDuomiOptions(f);
      expect(r.errors).toEqual({});
      expect(r.value.model_overrides).toEqual({
        "seedance-lite": {
          protocol: "seedance",
          mode: "pro",
          upstream_model: "doubao-seedance-lite",
          duration_as_string: true,
        },
      });
    });

    it("duration_as_string 必须是 JSON 布尔（字符串会被后端拒）", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ m1: { duration_as_string: "true" } });
      expect(buildDuomiOptions(f).errors["model_overrides.m1.duration_as_string"]).toContain("布尔");
      f.overridesText = JSON.stringify({ m1: { duration_as_string: false } });
      const ok = buildDuomiOptions(f);
      expect(ok.errors).toEqual({});
      expect(ok.value.model_overrides).toEqual({ m1: { duration_as_string: false } });
    });

    it("已删除的 fixed_tier 必须被判为未知字段（否则整渠道运行期 503）", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ "kling-v3": { fixed_tier: "720p" } });
      expect(buildDuomiOptions(f).errors["model_overrides.kling-v3.fixed_tier"]).toContain("未知字段");
    });

    it("kling.multi 不允许带 mode（画质由端点决定，后端同样硬拒）", () => {
      const f = emptyDuomiForm();
      // ① 显式写协议
      f.overridesText = JSON.stringify({
        "kling-v4-multi": { protocol: "kling.multi", mode: "std" },
      });
      expect(buildDuomiOptions(f).errors["model_overrides.kling-v4-multi.mode"]).toContain("不支持 mode");
      // ② 靠内置档案识别协议（不带 protocol 也要拦住）
      f.overridesText = JSON.stringify({ "kling-v1.6-multi": { mode: "pro" } });
      expect(buildDuomiOptions(f).errors["model_overrides.kling-v1.6-multi.mode"]).toContain("不支持 mode");
      // ③ 去掉 mode 就合法
      f.overridesText = JSON.stringify({ "kling-v4-multi": { protocol: "kling.multi" } });
      expect(buildDuomiOptions(f).errors).toEqual({});
    });

    it("JSON 非法 / 顶层不是对象 → 报错", () => {
      const f = emptyDuomiForm();
      f.overridesText = "{oops";
      expect(buildDuomiOptions(f).errors.model_overrides).toContain("JSON");
      f.overridesText = '["a"]';
      expect(buildDuomiOptions(f).errors.model_overrides).toContain("JSON 对象");
    });

    it("未知子键报错（后端 DisallowUnknownFields 同样会拒）", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ m1: { protocal: "seedance" } });
      expect(buildDuomiOptions(f).errors["model_overrides.m1.protocal"]).toContain("未知字段");
    });

    it("枚举越界报错：protocol / mode", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ m1: { protocol: "nano.old" } });
      expect(buildDuomiOptions(f).errors["model_overrides.m1.protocol"]).toContain("协议");
      f.overridesText = JSON.stringify({ m1: { mode: "turbo" } });
      expect(buildDuomiOptions(f).errors["model_overrides.m1.mode"]).toContain("std");
    });

    it("已知模型不得改成不兼容协议（会造成提交走错信封）", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ "kling-v3-pro": { protocol: "seedance" } });
      const r = buildDuomiOptions(f);
      expect(r.errors["model_overrides.kling-v3-pro.protocol"]).toContain("不能改成");
      expect("model_overrides" in r.value).toBe(false);
    });

    it("只改 upstream_model 是允许的（换型号不发版）", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ "kling-v3-pro": { upstream_model: "kling-v3-1" } });
      const r = buildDuomiOptions(f);
      expect(r.errors).toEqual({});
      expect(r.value.model_overrides).toEqual({ "kling-v3-pro": { upstream_model: "kling-v3-1" } });
    });

    it("回填后原文稳定：不修改就保存，生成相同字节", () => {
      const raw = { metadata_allow: ["aigc_watermark"], model_overrides: { m1: { protocol: "kling" } } };
      const f = duomiOptionsToForm(raw);
      const r = buildDuomiOptions(f);
      expect(r.errors).toEqual({});
      expect(r.value).toEqual(raw);
    });
  });

  it("体积上限：超长覆盖表被就地拦下（后端也会拒，但这里能说清原因）", () => {
    const f = emptyDuomiForm();
    const big: Record<string, unknown> = {};
    for (let i = 0; i < 60; i++) {
      big[`model-${i}-`.padEnd(40, "x")] = { upstream_model: "u".repeat(60) };
    }
    f.overridesText = JSON.stringify(big);
    const r = buildDuomiOptions(f);
    expect(r.errors.adapter_options).toContain("4096");
  });

  /* ------------------------- B6-1：图像 / 音乐子协议 ------------------------- */

  describe("B6-1 新协议与路径键", () => {
    it("图像/音乐协议已注册（未注册会被后端按未知协议拒绝）", () => {
      for (const p of ["nano", "gptimage", "suno.music", "suno.lyrics"]) {
        expect(DUOMI_PROTOCOLS as readonly string[]).toContain(p);
      }
    });

    it("muse 视频协议已注册，且查询路径与提交路径无前缀关系", () => {
      expect(DUOMI_PROTOCOLS as readonly string[]).toContain("muse");
      expect(DUOMI_DEFAULT_PATHS["muse.submit"]).toBe("/v1/videos/generations");
      // 上游的查询是 /v1/videos/tasks/{id}，**不是** submit + "/{id}"。
      // 这条断言就是防止有人"顺手"把它改成拼接式 —— 那样不会报错，只会每次轮询 404、
      // 任务一直 running 到超时退款。
      expect(DUOMI_DEFAULT_PATHS["muse.poll"]).toBe("/v1/videos/tasks");
      expect(DUOMI_DEFAULT_PATHS["muse.poll"]).not.toBe(
        `${DUOMI_DEFAULT_PATHS["muse.submit"]}/`,
      );
    });

    it("每个路径键都有内置默认：绝对路径", () => {
      for (const k of DUOMI_PATH_KEYS) {
        const def = DUOMI_DEFAULT_PATHS[k];
        expect(def.startsWith("/"), `${k} 的默认路径不是绝对路径`).toBe(true);
      }
      // 与后端常量逐字一致（写错会导致提交 404 而没有任何提示）
      expect(DUOMI_DEFAULT_PATHS["nano.submit"]).toBe("/api/gemini/nano-banana");
      expect(DUOMI_DEFAULT_PATHS["nano.edit"]).toBe("/api/gemini/nano-banana-edit");
      // 两个提交端点共用一个查询端点（文档只定义了一个 GET）
      expect(DUOMI_DEFAULT_PATHS["nano.poll"]).toBe("/api/gemini/nano-banana");
      expect(DUOMI_DEFAULT_PATHS["gptimage.submit"]).toBe("/v1/images/generations");
      expect(DUOMI_DEFAULT_PATHS["gptimage.poll"]).toBe("/v1/tasks");
      expect(DUOMI_DEFAULT_PATHS["suno.submit"]).toBe("/api/suno/generate");
      expect(DUOMI_DEFAULT_PATHS["suno.feed"]).toBe("/api/suno/feed");
      expect(DUOMI_DEFAULT_PATHS["suno.lyrics"]).toBe("/api/suno/submit/lyrics");
    });

    it("填了与默认相同的路径时不下发（避免把默认值钉死在库里）", () => {
      const f = emptyDuomiForm();
      f.paths["suno.feed"] = DUOMI_DEFAULT_PATHS["suno.feed"];
      expect("paths" in buildDuomiOptions(f).value).toBe(false);
    });

    it("新路径同样做绝对路径与查询串校验", () => {
      const f = emptyDuomiForm();
      f.paths["gptimage.poll"] = "v1/tasks";
      expect(buildDuomiOptions(f).errors["paths.gptimage.poll"]).toContain("/ 开头");
      f.paths["gptimage.poll"] = "/v1/tasks?id=1";
      expect(buildDuomiOptions(f).errors["paths.gptimage.poll"]).toContain("?");
    });

    it("图像/音乐模型的内置协议不可被改成别的协议", () => {
      const f = emptyDuomiForm();
      f.overridesText = JSON.stringify({ "nano-banana": { protocol: "gptimage" } });
      expect(buildDuomiOptions(f).errors["model_overrides.nano-banana.protocol"]).toContain("不能改成");
      f.overridesText = JSON.stringify({ "chirp-v4": { protocol: "suno.lyrics" } });
      expect(buildDuomiOptions(f).errors["model_overrides.chirp-v4.protocol"]).toContain("不能改成");
    });
  });

  describe("B6-1 suno_defaults（渠道级音乐默认）", () => {
    it("三项都不填 → 不下发（后端按非自定义 + 带人声处理）", () => {
      const r = buildDuomiOptions(emptyDuomiForm());
      expect(r.errors).toEqual({});
      expect("suno_defaults" in r.value).toBe(false);
    });

    it("显式 false 与「不配」是两件事，必须原样下发", () => {
      const f = emptyDuomiForm();
      f.sunoInstrumental = "false";
      const r = buildDuomiOptions(f);
      expect(r.errors).toEqual({});
      expect(r.value.suno_defaults).toEqual({ make_instrumental: false });
    });

    it("键名用上游叫法 make_instrumental（写 instrumental 后端会当未知字段整体拒绝）", () => {
      const f = emptyDuomiForm();
      f.sunoInstrumental = "true";
      const raw = buildDuomiOptions(f).value.suno_defaults as Record<string, unknown>;
      expect("instrumental" in raw).toBe(false);
      expect(raw.make_instrumental).toBe(true);
    });

    it("纯音乐 + 自定义歌词互斥（同时开会白花钱）", () => {
      const f = emptyDuomiForm();
      f.sunoCustomMode = "true";
      f.sunoInstrumental = "true";
      expect(buildDuomiOptions(f).errors.suno_defaults).toContain("不能同时");
    });

    it("mv 超长就地报错；正常值原样下发", () => {
      const f = emptyDuomiForm();
      f.sunoMv = "x".repeat(65);
      expect(buildDuomiOptions(f).errors["suno_defaults.mv"]).toContain("64");
      f.sunoMv = " chirp-v4 ";
      expect(buildDuomiOptions(f).value.suno_defaults).toEqual({ mv: "chirp-v4" });
    });

    it("回填：库里已有的 suno_defaults 不被当成未知键丢弃，且能字节一致回写", () => {
      const raw = {
        metadata_allow: ["aigc_watermark"],
        suno_defaults: { custom_mode: false, mv: "chirp-v4" },
      };
      const f = duomiOptionsToForm(raw);
      expect(f.unknownKeys).toEqual([]);
      expect(f.sunoCustomMode).toBe("false");
      expect(f.sunoMv).toBe("chirp-v4");
      expect(buildDuomiOptions(f).value).toEqual(raw);
    });
  });
});
