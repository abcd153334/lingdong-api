# 快速开始

> 自动生成 · 契约版本 2

所有接口都是标准 HTTP，地址为 `https://api.example.com/v1`。

**三步跑通**

1. 在控制台创建 API Key
2. 把 Key 放进请求头
3. 按模型文档发送请求

- Key 只显示一次
- 不要写进前端代码

**第一个请求**

```bash
curl https://api.example.com/v1/models \
  -H "Authorization: Bearer $LD_API_KEY"
```

> 图像与视频模型是异步的，提交后要轮询。

