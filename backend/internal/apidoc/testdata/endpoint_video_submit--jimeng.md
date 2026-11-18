# 创建视频任务（即梦格式）

> `POST /v1/video/generations` · 即梦 / Seedance 原生视频报文：素材放在 content[] 里，其余参数与厂商文档同名同义。只有声明了「原生参数」能力的模型可用；平台只替换 model，其余字节原样转发。
>
> 自动生成 · 契约版本 2 · 接口文档 `ep-video-submit--jimeng`

## 0. 概览

即梦 / Seedance 原生视频报文：素材放在 content[] 里，其余参数与厂商文档同名同义。只有声明了「原生参数」能力的模型可用；平台只替换 model，其余字节原样转发。

- **Base URL**：`https://api.example.com`（`example.com` 是占位域名，换成平台实际地址）
- **接口**：`POST /v1/video/generations`（别名：`/v1/videos`）

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "content": [
    {
      "type": "text",
      "text": "小猫对着镜头打哈欠"
    }
  ],
  "resolution": "720p",
  "ratio": "16:9",
  "duration": 5,
  "watermark": false
}'
```

## 1. 鉴权

所有接口都用一个 Bearer Token 鉴权：

```
Authorization: Bearer <你的 API KEY>
```

- KEY 只放服务端；不要写进前端代码、不要提交进代码仓库。
- KEY 无效、过期或被禁用 → `401 invalid_api_key`。
- KEY 没有该模型权限 → `403 model_not_allowed`。

```bash
curl "https://api.example.com/v1/models" -H "Authorization: Bearer $LD_API_KEY"
```

## 2. 请求

| 项目 | 值 |
| --- | --- |
| 方法 | `POST` |
| 路径 | `/v1/video/generations` |
| 请求体 | application/json |

别名路径与方法语义完全一致：`/v1/videos`。

这是**异步提交**接口：成功只代表任务已受理，要拿返回的 `id` 去查状态、再取产物。完整流程见下文「响应」与「相关接口」。

## 3. 参数

| 字段 | 类型 | 必填 | 默认 | 取值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `model` | string | 必填 | — | doubao-seedance-2-0-260128 | 平台模型 ID；出站时替换成厂商侧的真实模型名 |
| `content` | array | 必填 | — | — | 输入内容数组（文本 / 图 / 视频 / 音频 / 样片任务混排）；允许的组合：纯文本；文本 + 图；文本 + 视频；文本 + 图 + 音频；文本 + 图 + 视频；文本 + 视频 + 音频；四者全有；或只给样片任务（draft_task）。文本元素必须排第一，厂商按顺序解析指代 |
| `content[].type` | string | 必填 | — | text / image_url / video_url / audio_url / draft_task | 这一项是什么 |
| `content[].text` | string | 条件必填：type=text 时必填。 | — | — | 文本提示词（type=text 时必填）；字数建议：中文 ≤ 500 字、英文 ≤ 1000 词 |
| `content[].image_url.url` | string | 条件必填：type=image_url 时必填。 | — | — | 图片地址（type=image_url 时必填）：公网 URL、data: 内联或素材 ID；格式 jpeg / png / webp / bmp / tiff / gif（2.0 及以上另有 heic / heif）；宽高比 [0.4, 2.5]，宽高均 ∈ [300, 6000]px，单张 ≤ 30MB。张数：首帧 1 张、首尾帧 2 张、全模态参考 1~30 张（2.0 系列 1~9 张） |
| `content[].video_url.url` | string | 条件必填：type=video_url 时必填。 | — | — | 待编辑 / 待延长 / 参考视频地址（type=video_url 时必填） |
| `content[].audio_url.url` | string | 条件必填：type=audio_url 时必填。 | — | — | 参考音频地址（type=audio_url 时必填）；格式 wav / mp3，单段 ≤ 15MB；2.5 允许单独传音频，2.0 系列至少要配 1 个图或视频 |
| `content[].draft_task.id` | string | 条件必填：type=draft_task 时必填。 | — | — | 样片任务 ID（type=draft_task 时必填），有效期 7 天 |
| `content[].role` | string | 选填 | — | first_frame / last_frame / reference_image / reference_video / reference_audio | 这一项的用途，决定它被当成首帧、尾帧还是参考素材；图生视频-首帧填 first_frame（或不填）；首尾帧再给一个 last_frame；全模态参考生视频填 reference_image / reference_video / reference_audio |
| `duration` | integer | 选填 | 随模型 | — | 视频时长（秒）；可取值范围随模型，以厂商官方文档为准；平台不截断、不改写。传 -1 = 让模型自行选时长（仅支持该能力的模型接受）。与 frames 二选一 |
| `frames` | integer | 选填 | — | — | 按帧数精确指定时长（1.0 pro / pro fast 支持）；帧率 24：133 帧 ≈ 5.54 秒。与 duration 二选一，frames 优先；29 ~ 289，且须为 25+4n |
| `resolution` | string | 选填 | — | 480p / 720p / 1080p / 4k | 清晰度档位；写法原样透传；该模型支持哪些档位以厂商官方文档为准。未配置的档位不会拦截，按兜底档处理 |
| `ratio` | string | 选填 | — | 16:9 / 9:16 / 1:1 / 4:3 / 3:4 / 21:9 / adaptive | 画幅比例；adaptive = 按任务类型与素材自动适配。2.5 的首帧 / 首尾帧、视频编辑、视频延长任务只接受 adaptive。实际宽高像素值随档位与比例不同，见模型文档的对照表 |
| `seed` | integer | 选填 | 随机 | — | 随机种子：相同种子生成结果相近（不保证完全一致）；-1 ~ 2147483647（-1 = 随机） |
| `camera_fixed` | boolean | 选填 | false | — | 固定镜头：true 时不做运镜 |
| `watermark` | boolean | 选填 | false | — | 成片右下角是否加「AI 生成」水印 |
| `generate_audio` | boolean | 选填 | true | — | 是否生成与画面同步的声音；默认有声；要静音请显式传 false |
| `return_last_frame` | boolean | 选填 | false | — | 是否额外返回成片的尾帧图（jpeg）；常用来把上一段尾帧接成下一段首帧，连续生成多段 |
| `draft` | boolean | 选填 | false | — | 样片模式：先出低清预览；样片的任务 ID 与正式任务不同，确认后再用 draft_task 提交正式视频 |
| `service_tier` | string | 选填 | default | default / flex | 服务等级；flex 是离线推理：配额更高、价格约为在线的一半，但时延不保证；2.5 / 2.0 系列不支持。提交后不可修改 |
| `priority` | integer | 选填 | 0 | — | 排队优先级：数值越大越靠前（只影响排队，不打断运行中的任务）；flex 离线模式不支持优先级；0 ~ 9 |
| `tools` | array | 选填 | — | — | 工具列表（目前只有联网搜索）；写法 [{type:"web_search"}]；实际搜索次数见查询结果的 usage.tool_usage.web_search |
| `tools[].type` | string | 必填 | — | web_search | 工具类型 |
| `output_format` | string | 选填 | mp4 | mp4 / mov | 成片容器格式；mov 是高色深格式（H.264 + yuv444p + PCM），适合调色 / 抠像 / 合成，部分播放器不支持。2.5 支持 |
| `callback_url` | string | 选填 | — | — | 回调地址；本平台不接收回调：这个地址会原样发给厂商，但你不会在本平台收到推送，请轮询任务查询接口 |
| `execution_expires_after` | integer | 选填 | 172800 | — | 任务过期阈值（秒），从提交时间起算；超过即被厂商终止并标记 expired。平台自己的超时判据更短、会先生效，一般不必填；3600 ~ 259200 |
| `safety_identifier` | string | 选填 | — | — | 终端用户标识，用于厂商侧合规检测；传哈希后的用户标识，不要传原始手机号 / 邮箱；≤ 64 字符 |
| `omni_reference_task_type` | string | 选填 | auto | auto / reference / edit / extend | 全模态参考生视频的子任务类型引导（2.5）；显式指定可在提交时就校验该类型的参数限制；auto 由模型判定，不合规会变成异步报错（任务已建、随后失败）。edit / extend 要求 content 里至少有一个 reference_video，且 ratio 必须为 adaptive（edit 还要求 duration=-1） |

## 4. 响应

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 任务号 |

响应示例（字段含义见上表）：

```json
{
  "id": "<string>"
}
```


### 常见用法

**原生参数 — 按厂商原始写法提交，字段一个不改。**

content[] 原样转发，字段与厂商文档一致；此类任务不支持重试。

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "doubao-seedance-2-0-260128",
  "content": [
    {
      "type": "text",
      "text": "一只橘猫在钢琴前专注弹奏，镜头缓缓推近"
    }
  ],
  "duration": -1,
  "ratio": "adaptive",
  "resolution": "480p"
}'
```

## 5. 示例代码

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "content": [
    {
      "type": "text",
      "text": "小猫对着镜头打哈欠"
    }
  ],
  "resolution": "720p",
  "ratio": "16:9",
  "duration": 5,
  "watermark": false
}'
```

```python
import os
import requests

BASE = "https://api.example.com"
HEADERS = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "<model>",
    "content": [
        {
            "type": "text",
            "text": "小猫对着镜头打哈欠",
        },
    ],
    "resolution": "720p",
    "ratio": "16:9",
    "duration": 5,
    "watermark": False,
}

r = requests.post(BASE + "/v1/video/generations", headers=HEADERS, json=payload, timeout=(10, 300))
r.raise_for_status()
print(r.json())
```

```javascript
const BASE = "https://api.example.com";
const KEY = process.env.LD_API_KEY;

const res = await fetch(BASE + "/v1/video/generations", {
  method: "POST",
  headers: { Authorization: `Bearer ${KEY}`, "Content-Type": "application/json" },
  body: JSON.stringify({
  "model": "<model>",
  "content": [
    {
      "type": "text",
      "text": "小猫对着镜头打哈欠"
    }
  ],
  "resolution": "720p",
  "ratio": "16:9",
  "duration": 5,
  "watermark": false
}),
});
if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
console.log(await res.json());
```

```go
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	const base = "https://api.example.com"

	body := []byte(`{
  "model": "<model>",
  "content": [
    {
      "type": "text",
      "text": "小猫对着镜头打哈欠"
    }
  ],
  "resolution": "720p",
  "ratio": "16:9",
  "duration": 5,
  "watermark": false
}`)
	req, err := http.NewRequest("POST", base + "/v1/video/generations", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("LD_API_KEY"))
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(res.Body)
		panic(fmt.Sprintf("%d %s", res.StatusCode, msg))
	}
	out, _ := io.ReadAll(res.Body)
	fmt.Println(strings.TrimSpace(string(out)))
}
```

以上示例的请求体是「基础示例」；字段含义见第 3 节。

**图生视频（首帧 + 智能时长）**

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "content": [
    {
      "type": "text",
      "text": "镜头缓缓推近，海面泛起晨雾"
    },
    {
      "type": "image_url",
      "image_url": {
        "url": "https://cdn.example.com/sea.jpg"
      },
      "role": "first_frame"
    }
  ],
  "ratio": "adaptive",
  "duration": -1,
  "generate_audio": true
}'
```

```python
import os
import requests

BASE = "https://api.example.com"
HEADERS = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "<model>",
    "content": [
        {
            "type": "text",
            "text": "镜头缓缓推近，海面泛起晨雾",
        },
        {
            "type": "image_url",
            "image_url": {
                "url": "https://cdn.example.com/sea.jpg",
            },
            "role": "first_frame",
        },
    ],
    "ratio": "adaptive",
    "duration": -1,
    "generate_audio": True,
}

r = requests.post(BASE + "/v1/video/generations", headers=HEADERS, json=payload, timeout=(10, 300))
r.raise_for_status()
print(r.json())
```

## 6. 错误码

| HTTP | code | 含义 | 处理建议 |
| --- | --- | --- | --- |
| 400 | `invalid_param` | 参数不合法（字段缺失、取值越界、素材地址不可访问）。 | 对照字段表核对参数，错误信息会指出具体字段。 |
| 401 | `invalid_api_key` | KEY 无效、已过期或被禁用。 | 检查 KEY 是否完整、是否已在控制台删除或过期。 |
| 402 | `insufficient_balance` | 额度不足，请充值。 |  |
| 403 | `modality_not_allowed` | 该模态不在 KEY 的白名单里。 |  |
| 403 | `model_not_allowed` | 该 KEY 没有这个模型的权限。 | 确认这个 KEY 的可用模型包含当前模型。 |
| 404 | `model_not_found` | 模型不存在或已下线。 | 核对模型 ID 拼写；模型可能已下线，见模型广场。 |
| 409 | `task_not_cancelable` | 取消时任务已开始生成（仅取消接口会返回）。 | 只有排队中（queued）且尚未开始生成的任务可取消；已开始生成请等它结束。 |
| 413 | `request_too_large` | 请求体超过上限。 |  |
| 429 | `rate_limited` | 请求过于频繁，稍后重试。 | 按退避重试；持续触发请申请更高并发。 |
| 500 | `internal_error` | 服务端异常，本次不计费。 |  |
| 503 | `model_unavailable` | 该模型暂不可用，请更换模型或稍后重试。 |  |
| 503 | `service_unavailable` | 本次调用未能完成，未产生费用。 | 稍后重试，本次未扣费。 |
| 504 | `gateway_timeout` | 等待超时，已全额释放额度。 | 缩短时长或稍后重试；已全额释放额度。 |

## 7. 相关接口

同属「即梦格式」的其它接口：

| 接口 | 方法与路径 | 文档 |
| --- | --- | --- |
| 视频再生成（升档）（同一流程） | `POST /v1/video/regenerations` | `ep-video-regenerations` |
| 任务列表（同一流程） | `GET /v1/tasks` | `ep-tasks-list` |
| 任务详情（同一流程） | `GET /v1/tasks/{id}` | `ep-tasks-get` |
| 下载任务产物（同一流程） | `GET /v1/tasks/{id}/contents/{index}` | `ep-tasks-content-at` |
| 取消任务（同一流程） | `DELETE /v1/tasks/{id}` | `ep-tasks-cancel` |

**支持本接口的模型文档**（计费方式、档位与上限写在那边）：

| 文档 | 模型 |
| --- | --- |
| 视频生成 | `doubao-seedance-2-0-260128` |

本接口不写计费数值：单价与档位属于模型事实，见上表对应文档，避免两处口径慢慢走偏。

## 8. 注意事项

- 本篇是即梦格式原生报文，只适用于按本篇字段提交的模型；其它模型请改看OpenAI 原生报文「创建视频任务」（`ep-video-submit`） —— 本篇的字段它们不认识。
- 这篇是厂商原生报文：平台只替换 `model`，其余字节原样转发 —— 因此这类任务**不支持重试**（重新拼装会丢掉素材），失败请重新提交。
- 素材必须是公网 http(s) 直链、data: 内联或厂商素材 ID；平台只读 mp4 头尾索引取参考视频时长，读不到会直接 400。
- 只说时长不够：content 里写清楚素材用途（role），否则模型按默认规则推断，成片可能和预期不同。
- 厂商对未知字段的处置不可控（400 或忽略），所以平台白名单外的顶层键不会出站，并在调用记录里给出「未识别参数」告警。

---

> 契约版本 2 · 接口 `video/submit` · 视频 / 即梦格式
