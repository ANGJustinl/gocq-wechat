# 配置

这个分支已经改成 `Weixin/iLink -> OneBot v11`，不再负责 QQ 登录、QQ 协议和群能力。`config.yml` 仍然是主配置文件，但真正新增的入口是 `weixin` 段。

## 最小配置

```yaml
account:
  uin: 0
  password: ''
  encrypt: false
  status: 0

weixin:
  api-base-url: https://ilinkai.weixin.qq.com
  cdn-base-url: https://novac2c.cdn.weixin.qq.com/c2c
  poll-timeout: 35
  qr-timeout: 480
  state-dir: data/weixin

heartbeat:
  interval: 5

message:
  post-format: string
  ignore-invalid-cqcode: false
  force-fragment: false
  fix-url: false
  proxy-rewrite: ''
  report-self-message: false
  remove-reply-at: false
  extra-reply-data: false
  skip-mime-scan: false
  convert-webp-image: false
  http-timeout: 15

output:
  log-level: info
  log-aging: 15
  log-force-new: true
  log-colorful: true
  debug: false

default-middlewares: &default
  access-token: ''
  filter: ''
  rate-limit:
    enabled: false
    frequency: 1
    bucket: 1

database:
  leveldb:
    enable: true
  sqlite3:
    enable: false
    cachettl: 3600000000000

servers:
  - http:
      address: 0.0.0.0:5700
      version: 11
      timeout: 5
      middlewares:
        <<: *default

  - ws:
      address: 0.0.0.0:6700
      middlewares:
        <<: *default

  - ws-reverse:
      universal: ws://127.0.0.1:8080/
      reconnect-interval: 3000
      middlewares:
        <<: *default
```

## Weixin 段说明

| 字段 | 说明 |
| --- | --- |
| `api-base-url` | iLink API 根地址，默认官方地址 |
| `cdn-base-url` | iLink CDN 根地址，默认官方地址 |
| `poll-timeout` | Go 主进程的长轮询秒数 |
| `qr-timeout` | 扫码等待时长，单位秒 |
| `state-dir` | Weixin 运行时状态根目录，固定保存 session/sync/contacts/messages/media |

## 运行约束

- 这个分支只支持 `servers.*.version: 11`。如果 HTTP server 配了 `12`，程序会忽略并强制回到 `11`。
- `account` 段保留只是为了兼容旧结构；不会再触发 QQ 登录链路。
- `device.json`、`session.token`、qsign、sign-server 在默认 Weixin 启动路径中都不会再被使用。
- 程序是单二进制运行，不再要求本地 sidecar 或 Node.js
- 首版只支持私聊，不支持微信群、频道、好友请求、群管理、撤回等动作。

## OneBot 支持面

当前公开动作固定为：

- `get_login_info`
- `get_status`
- `get_version_info`
- `get_supported_actions`
- `can_send_image`
- `can_send_record`
- `send_private_msg`
- `send_msg`
- `get_msg`
- `get_stranger_info`
- `get_friend_list`
- `download_file`
- `upload_private_file`
- `.handle_quick_operation`

其中：

- `send_msg` 只接受私聊语义；传 `group_id` 会直接失败。
- `can_send_image = true`
- `can_send_record = false`
- 所有群、频道、管理类动作都会返回 `UNSUPPORTED_ACTION`。

## 消息映射

- `self_id` / `user_id` 使用原始微信 ID 的正 63 位 FNV-1a 哈希。
- `message_id` 使用 `crc32("wx:<self_raw_id>:<peer_raw_id>:<raw_message_id_or_client_id>")`。
- 入站消息段：
  - 文本 -> `text`
  - 图片 -> `image`
  - 语音 -> `record`
  - 视频 -> `video`
  - 文件 -> 文本占位 `[文件] <name>`，并在事件中附带 `_wx_file_path`
- 出站消息段：
  - 支持 `text`
  - 支持 `image`
  - 支持 `video`
  - 支持 `file`
  - 不支持 `record`
  - `reply` 会降级成普通文本引用前缀
