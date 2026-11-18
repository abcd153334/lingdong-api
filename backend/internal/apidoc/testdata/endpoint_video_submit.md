# 创建视频任务

> `POST /v1/video/generations` · 提交视频生成任务，立即返回任务号。文字生视频 / 图片生视频 / 配音 / 续写统一入口。
>
> 自动生成 · 契约版本 2 · 接口文档 `ep-video-submit`

## 0. 概览

提交一个视频生成任务，拿到任务号后轮询查询接口取回结果。

- 同一套参数支持多个视频模型，切换只改 model。

- **Base URL**：`https://api.example.com`（`example.com` 是占位域名，换成平台实际地址）
- **接口**：`POST /v1/video/generations`（别名：`/v1/videos`）

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
  "seconds": 5,
  "resolution": "720p",
  "input_reference": [
    {
      "type": "image",
      "image_url": "https://cdn.example.com/cat.png"
    },
    {
      "type": "audio",
      "audio_url": "https://cdn.example.com/piano.mp3"
    }
  ]
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
| `model` | string | 必填 | — | kling-v3 | 模型 ID（须与接口模态一致） |
| `prompt` | string | 条件必填：走厂商原生报文（content[]）时可省，见同板块「即梦格式」那篇。 | — | — | 画面 + 动作 + 运镜描述 |
| `seconds` | any | 选填 | 5 | — | 视频时长（秒），数字或字符串均可；可取值随模型；超上限直接报错，不截断、不自动降档。传 -1 为智能时长（仅声明了该能力的模型支持）。不传则由模型默认时长决定。最终时长以平台实际返回的为准，最多不超过模型时长上限 |
| `resolution` | string | 必填 | — | 720p / 1080p | 清晰度档位；可写档位（如 480p / 1080p），也可写「宽x高」（x 与 × 两种分隔符都认）。写法原样透传给模型；未配置的写法不会拦截，按兜底档处理 |
| `size` | string | 选填 | — | 720p / 1080p | resolution 未填时的兜底别名 |
| `input_reference` | array | 选填 | [] | — | 素材引用数组；type 决定用途，缺省时按哪个链接非空推断。地址须公网 http(s) 直链，推荐先用 /v1/files 上传再取回 |
| `input_reference[].type` | string | 选填 | 按链接推断 | image / audio / video / first_frame / last_frame | 素材用途；写错不会兜底（直接 400）；首尾帧也接受 first-frame / last-frame 写法 |
| `input_reference[].image_url` | string | 选填 | — | — | 参考图 |
| `input_reference[].audio_url` | string | 选填 | — | — | 配音音频 |
| `input_reference[].video_url` | string | 选填 | — | — | 待续写视频 |
| `aspect_ratio` | string | 选填 | — | — | 画幅比例，取值随模型；常见 16:9 / 9:16 / 1:1 / 4:3 / 3:4 / 21:9；部分模型支持 adaptive（跟随参考图）。完整取值见 ratio；仅部分模型支持，以模型页为准 |
| `ratio` | string | 选填 | — | 16:9 / 9:16 / 1:1 / 4:3 / 3:4 / 21:9 / adaptive | 画幅比例（厂商原生写法，与 aspect_ratio 等价）；adaptive = 跟随参考图比例；仅部分模型支持，以模型页为准 |
| `frames` | integer | 选填 | — | — | 按帧数精确指定时长（仅 1.0 pro 系列支持）；取值 [29, 289] 且须为 25+4n（约 1.2 ~ 12 秒）。本平台要求同时给 seconds 作为时长基准：只传 frames 会直接 400（没有秒数就无法确定基准）；最终按服务方返回的帧数折算成秒；仅部分模型支持，以模型页为准 |
| `generate_audio` | boolean | 选填 | true | — | 是否生成音轨；默认就有声；要静音请显式传 false；仅部分模型支持，以模型页为准 |
| `output_format` | string | 选填 | mp4 | mp4 / mov | 成片容器格式；仅部分模型支持，以模型页为准 |
| `draft` | boolean | 选填 | false | — | 样片模式：先出低清预览快速看效果；样片任务的 id 与正式任务不同，拿到样片后需另提正式任务；仅部分模型支持，以模型页为准 |
| `omni_reference_task_type` | string | 选填 | auto | auto / reference / edit / extend | 参考生视频的子任务类型引导；auto 由服务方按素材判定；显式指定可在提交时就校验该类型的参数限制，否则不合规会变成异步报错（任务已建、随后失败）；仅部分模型支持，以模型页为准 |
| `priority` | integer | 选填 | 0 | — | 排队优先级（越大越优先）；0 ~ 9；仅部分模型支持，以模型页为准 |
| `tools` | array | 选填 | — | — | 工具列表（目前只有联网搜索）；写法 [{type:"web_search"}]；实际搜索次数见查询结果的 usage.tool_usage.web_search；仅部分模型支持，以模型页为准 |
| `camera_fixed` | boolean | 选填 | false | — | 固定镜头（不做运镜）；仅部分模型支持，以模型页为准 |
| `return_last_frame` | boolean | 选填 | false | — | 是否返回成片的尾帧图（jpeg）；尾帧可从查询结果取回；常用来把上一段视频的尾帧接成下一段的首帧，连续生成多段；仅部分模型支持，以模型页为准 |
| `service_tier` | string | 选填 | default | default / flex | 服务等级：default 在线推理，flex 离线推理；flex 配额更高、价格约为在线的一半，但时延不保证，且不是所有模型都支持（2.5 / 2.0 系列不支持）。提交后不可修改；仅部分模型支持，以模型页为准 |
| `execution_expires_after` | integer | 选填 | 172800 | — | 任务过期阈值（秒）；从提交时间起算，超过即被服务方终止并标记过期。平台自己的超时判据更短、会先生效，一般不必填；3600 ~ 259200；仅部分模型支持，以模型页为准 |
| `safety_identifier` | string | 选填 | — | — | 终端用户标识，用于厂商侧合规检测；传哈希后的用户标识，不要传原始手机号 / 邮箱；≤ 64 字符；仅部分模型支持，以模型页为准 |
| `camera_control` | object | 选填 | — | — | 运镜控制（推拉摇移）；六个方向只能填一个，各取 -10 ~ 10（正负代表方向）；与尾帧互斥，仅图生视频可用。平台会按厂商要求的结构下发，你只需要填六个轴的数值；仅部分模型支持，以模型页为准 |
| `camera_control.horizontal` | number | 选填 | 0 | — | 水平移动力度；-10 ~ 10；仅部分模型支持，以模型页为准 |
| `camera_control.vertical` | number | 选填 | 0 | — | 垂直移动力度；-10 ~ 10；仅部分模型支持，以模型页为准 |
| `camera_control.pan` | number | 选填 | 0 | — | 左右摇镜力度；-10 ~ 10；仅部分模型支持，以模型页为准 |
| `camera_control.tilt` | number | 选填 | 0 | — | 上下摇镜力度；-10 ~ 10；仅部分模型支持，以模型页为准 |
| `camera_control.roll` | number | 选填 | 0 | — | 旋转力度；-10 ~ 10；仅部分模型支持，以模型页为准 |
| `camera_control.zoom` | number | 选填 | 0 | — | 推近 / 拉远力度；-10 ~ 10；仅部分模型支持，以模型页为准 |
| `seed` | integer | 选填 | 随机 | — | 随机种子；仅部分模型支持，以模型页为准 |
| `metadata` | object | 选填 | — | — | 厂商私有参数容器，原样透传并在任务查询中回显；只下发平台为该模型开放的白名单键；不在白名单里的键不会生效，会在调用记录里给出「未识别参数」告警（不静默丢弃、也不因此拒绝整单）。可灵协议的模型不接受 metadata：传了会明确报错（不会静默忽略，否则你会以为配音 / 水印生效了）；仅部分模型支持，以模型页为准 |
| `watermark` | boolean | 选填 | false | — | 是否在成片上叠加「AI 生成」水印；仅部分模型支持，以模型页为准 |
| `callback_url` | string | 选填 | — | — | 回调地址，本平台不推送回调；传了也不会收到回调，请轮询查询接口 |
| `duration` | number | 选填 | — | — | 时长字段，自动等价映射为 seconds |
| `width` | integer | 选填 | — | — | 宽，与 height 一起自动合成 resolution |
| `height` | integer | 选填 | — | — | 高，与 width 一起自动合成 resolution |
| `image` | string | 选填 | — | — | 首帧图，自动映射为 input_reference 首元素；URL 或 base64 |
| `first_frame` | string | 选填 | — | — | 首帧图的顶层写法，自动折叠进 input_reference |
| `last_frame` | string | 选填 | — | — | 尾帧图的顶层写法；与 first_frame 同时给 = 首尾帧模式 |
| `fps` | integer | 选填 | — | — | 帧率（仅少数模型可调，未声明该参数的模型会被忽略） |

## 4. 响应

| 字段 | 类型 | 取值 | 说明 |
| --- | --- | --- | --- |
| `id` | string | — | 任务 ID（十进制字符串）。查询进度、下载产物都用它 |
| `object` | string | — | 固定为 task |
| `status` | string | queued / running / succeeded / failed / expired / canceled | 任务状态 |
| `model` | string | — | 模型 ID |
| `modality` | string | chat / image / video / audio / music / embedding / rerank / moderation | 模型大类 |
| `kind` | string | — | 任务子类型；timeout 表示轮询网络失败超限；其余与 modality 同值 |
| `model_icon_url` | string | — | 模型图标地址（绝对地址，可能为空） |
| `progress` | integer | — | 进度百分比：排队 0、运行 50、终态 100 |
| `created_at` | integer | — | 创建时间（Unix 秒） |
| `completed_at` | integer | — | 完成时间（Unix 秒）。只有终态才有 |
| `deadline_at` | integer | — | 超时时间（Unix 秒） |
| `error` | object | — | 失败原因；成功时为 null（恒为对象，不是字符串） |
| `error.message` | string | — | 失败分类文案；对用户可读、不含任何内部标识；服务商原文只在管理台可见 |
| `contents` | array | — | 已转存的产物清单；恒为数组（没有产物时是 []）；序号即 /v1/tasks/{id}/contents/{index} 的 index |
| `contents[].index` | integer | — | 产物序号（0 起、稠密编号） |
| `contents[].url` | string | — | 取数地址（绝对地址，带鉴权，不能直接丢给浏览器打开） |
| `contents[].content_type` | string | — | 产物 MIME 类型 |
| `contents[].bytes` | integer | — | 产物字节数 |
| `contents[].kind` | string | image / video / audio / text / file | 产物渲染档 |
| `usage` | object | — | 本次任务的原始用量（数量，不是金额） |
| `usage.images` | integer | — | 出图张数 |
| `usage.seconds` | integer | — | 视频/音频秒数 |
| `usage.ref_images` | integer | — | 参考图张数 |
- 受理即返回，不代表已开始生成；结果要轮询查询接口。
- `status` 的六种取值见「异步任务」一节。

响应示例（字段含义见上表）：

```json
{
  "id": "task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ",
  "object": "task",
  "status": "queued",
  "model": "MiniMax-H3",
  "modality": "video",
  "kind": "video",
  "progress": 0,
  "created_at": 1762318830,
  "deadline_at": 1762322400,
  "error": null,
  "contents": [],
  "usage": {
    "images": 0,
    "seconds": 5,
    "ref_images": 0
  }
}
```


## 5. 示例代码

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
  "seconds": 5,
  "resolution": "720p",
  "input_reference": [
    {
      "type": "image",
      "image_url": "https://cdn.example.com/cat.png"
    },
    {
      "type": "audio",
      "audio_url": "https://cdn.example.com/piano.mp3"
    }
  ]
}'
```

```python
import os
import requests

BASE = "https://api.example.com"
HEADERS = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "<model>",
    "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
    "seconds": 5,
    "resolution": "720p",
    "input_reference": [
        {
            "type": "image",
            "image_url": "https://cdn.example.com/cat.png",
        },
        {
            "type": "audio",
            "audio_url": "https://cdn.example.com/piano.mp3",
        },
    ],
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
  "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
  "seconds": 5,
  "resolution": "720p",
  "input_reference": [
    {
      "type": "image",
      "image_url": "https://cdn.example.com/cat.png"
    },
    {
      "type": "audio",
      "audio_url": "https://cdn.example.com/piano.mp3"
    }
  ]
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
  "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
  "seconds": 5,
  "resolution": "720p",
  "input_reference": [
    {
      "type": "image",
      "image_url": "https://cdn.example.com/cat.png"
    },
    {
      "type": "audio",
      "audio_url": "https://cdn.example.com/piano.mp3"
    }
  ]
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

**兼容写法（duration / width / height / image）**

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "<model>",
  "prompt": "航拍雪山日出",
  "duration": 5,
  "width": 1280,
  "height": 720,
  "image": "https://cdn.example.com/mountain.jpg",
  "metadata": {
    "negative_prompt": "模糊"
  }
}'
```

```python
import os
import requests

BASE = "https://api.example.com"
HEADERS = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "<model>",
    "prompt": "航拍雪山日出",
    "duration": 5,
    "width": 1280,
    "height": 720,
    "image": "https://cdn.example.com/mountain.jpg",
    "metadata": {
        "negative_prompt": "模糊",
    },
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

同属「视频」的其它接口：

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
| 视频生成 | `kling-v3` |

本接口不写计费数值：单价与档位属于模型事实，见上表对应文档，避免两处口径慢慢走偏。

## 8. 注意事项

- 本篇是 OpenAI 原生报文。content[] 素材数组、厂商原生的首尾帧与参考音频写法、运镜与镜头控制字段只在那篇里；参考图数组 image_urls（裸字符串数组，1–8 张）、固定画幅取值 16:9 / 9:16、10 / 20 / 30 秒时长档只在那篇里。按本篇填会漏掉它们，即梦格式原生报文见「创建视频任务（即梦格式）」（`ep-video-submit--jimeng`）、Muse格式原生报文见「创建视频任务（Muse 格式）」（`ep-video-submit--muse`）。
- 模型可配置最大秒数：超出即在提交时 400。上限只在下单时校验，已受理的任务不会因该上限失败。
- 素材地址须为公网 http(s) 直链，不支持 data: 内联素材（内联体读不到时长，也无法重放）；含内联素材的任务不可重试。
- platform 不做回调推送：callback_url 只透传，请轮询查询接口。
- 素材直链按 IP 限流 120 次/分钟：单次提交多张图会并发取图，高并发场景请自行收敛。
- 传 seconds:-1 = 智能时长：模型自行决定长度，以模型上限为界、按实际返回的整数秒（仅声明了该能力的模型支持，其它模型传 -1 直接 400）。
- frames 只有 1.0 pro 系列支持，且必须同时给 seconds（时长以它为准，最终按服务方返回的帧数折算成秒）；两者都传时出站报文以 frames 为准。
- Seedance 系列的秒数下限是 4 秒（2.5 为 4 ~ 30，2.0 为 4 ~ 15）；低于下限平台不拦，但生成会失败。

---

> 契约版本 2 · 接口 `video/submit` · 视频
