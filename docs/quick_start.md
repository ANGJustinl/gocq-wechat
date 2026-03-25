# 开始

这个分支已经改成单二进制启动，Go 主进程会直接完成二维码登录、收发消息和 OneBot 对外服务。

## 环境要求

- Go 版本按仓库原要求
- 可访问 `https://ilinkai.weixin.qq.com` 和微信 CDN

## 1. 配置 `config.yml`

生成配置后，至少确认以下几项：

```yaml
weixin:
  api-base-url: https://ilinkai.weixin.qq.com
  cdn-base-url: https://novac2c.cdn.weixin.qq.com/c2c
  state-dir: data/weixin

servers:
  - http:
      address: 0.0.0.0:5700
      version: 11
```

这个分支只支持 OneBot v11。不要把 `servers.*.version` 配成 `12`。

## 2. 启动主进程

```bash
go run .
```

或者构建后运行：

```bash
go build -o go-cqhttp .
./go-cqhttp
```

如果当前还没有登录态，程序会在终端打印二维码；用微信扫码后，程序会自动进入长轮询并开始对外提供 OneBot HTTP/WS 服务。

## 3. 验证接口

发送一条私聊消息的最小示例：

```bash
curl "http://127.0.0.1:5700/send_private_msg?user_id=<哈希后的用户ID>&message=hello"
```

可用的 `user_id` 可以从收到的私聊事件里取。这个分支对外暴露的是哈希后的 `int64`，原始微信 ID 会作为扩展字段 `_wx_raw_user_id` 附带在事件中。

## 当前限制

- 只支持私聊
- 不支持微信群、频道、好友请求、群管理类动作
- 不支持发送 `record`
- `reply` 会降级成普通文本引用前缀
