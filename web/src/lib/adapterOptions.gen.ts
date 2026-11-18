// 由 backend/cmd/dump-adapter-options 生成，请勿手改。
// 来源：backend/internal/relay/adapters/options_schema.go
// 重新生成（backend 目录）：go run ./cmd/dump-adapter-options
//
// 这些常量决定「前端能写哪些键」。与后端不一致的代价不是配置不生效，
// 而是运行期整段解析失败 —— 该渠道绑定的所有模型都会 503。

export const DUOMI_PROTOCOLS = ["seedance","kling","kling.multi","kling.omni","muse","nano","gptimage","suno.music","suno.lyrics"] as const;
export const DUOMI_PATH_KEYS = ["seedance.submit","kling.prefix","muse.submit","muse.poll","nano.submit","nano.edit","nano.poll","gptimage.submit","gptimage.poll","suno.submit","suno.feed","suno.lyrics"] as const;
export const DUOMI_OVERRIDE_KEYS = ["protocol","mode","upstream_model","duration_as_string"] as const;
export const DUOMI_METADATA_KEYS = ["aigc_watermark","generate_audio","watermark"] as const;
export const DUOMI_DEFAULT_METADATA_ALLOW = ["generate_audio","watermark"] as const;
export const DUOMI_KNOWN_MODELS = {"chirp-v4":{"protocol":"suno.music","upstream":"chirp-v4","mode":""},"gpt-image-2":{"protocol":"gptimage","upstream":"gpt-image-2","mode":""},"gpt-image-2-vip":{"protocol":"gptimage","upstream":"gpt-image-2-vip","mode":""},"gpt-image-2.5":{"protocol":"gptimage","upstream":"gpt-image-2.5","mode":""},"gpt-image-2.5-flare":{"protocol":"gptimage","upstream":"gpt-image-2.5-flare","mode":""},"gpt-image-2.5-sunburst":{"protocol":"gptimage","upstream":"gpt-image-2.5-sunburst","mode":""},"kling-v1.6-multi":{"protocol":"kling.multi","upstream":"kling-v1-6","mode":""},"kling-v3-omni":{"protocol":"kling.omni","upstream":"kling-v3-omni","mode":"std"},"kling-v3-pro":{"protocol":"kling","upstream":"kling-v3","mode":"pro"},"kling-v3-std":{"protocol":"kling","upstream":"kling-v3","mode":"std"},"muse-video":{"protocol":"muse","upstream":"muse-video","mode":""},"nano-banana":{"protocol":"nano","upstream":"gemini-2.5-flash-image","mode":""},"nano-banana-2":{"protocol":"nano","upstream":"gemini-3.1-flash-image-preview","mode":""},"nano-banana-pro":{"protocol":"nano","upstream":"gemini-3-pro-image-preview","mode":""},"seedance-2.0":{"protocol":"seedance","upstream":"doubao-seedance-2-0-260128","mode":""},"seedance-2.0-fast":{"protocol":"seedance","upstream":"doubao-seedance-2-0-fast-260128","mode":""},"suno-lyrics":{"protocol":"suno.lyrics","upstream":"","mode":""}} as const;
export const METASO_TIERS = ["480p","720p","1080p","2k"] as const;
export const METASO_RESOLUTION_DEFAULTS = {"1080p":"1080P","2k":"2K","480p":"480P","720p":"768P"} as const;
export const METASO_UPSTREAM_RESOLUTIONS = ["","480P","768P","1080P","2K"] as const;
export const AUTODL_MODES = ["multimodal","image","text","first_last"] as const;
export const AUTODL_MODE_LABELS = {"first_last":"首尾帧","image":"图片参考","multimodal":"多模态（图+音）","text":"纯文本"} as const;
export const AUTODL_WORKFLOW_DEFAULTS = {"first_last":"minimax_h3_lightx2v","image":"minimax_h3_lightx2v_v5_15s","multimodal":"minimax_h3_zm_u24","text":"minimax_h3_lightx2v_no_pic"} as const;
export const AUTODL_RESOLUTION_DEFAULTS = {"1080p":"","480p":"480p","720p":"768p"} as const;
export const AUTODL_UPSTREAM_RESOLUTIONS = ["","480p","768p","1080p"] as const;
