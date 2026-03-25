# Weixin API Matrix

这个表用于对照三件事：

- `oenbot11`：OneBot v11 / go-cqhttp 常见公开动作名
- `当前实现`：当前 `go-cqhttp-weixin` 分支已经实现到什么程度
- `openclaw-weixin`：上游 `openclaw-weixin` 源码里已经具备的实际能力，不等同于 OneBot 包装层

## OneBot v11 对照

| oenbot11 | 当前实现 | openclaw-weixin |
| --- | --- | --- |
| `get_login_info` | 已实现 | 有等价能力，未包装成 OneBot |
| `get_status` | 已实现 | 有等价能力，未包装成 OneBot |
| `get_version_info` | 已实现 | 无 OneBot 对等接口 |
| `get_supported_actions` | 已实现 | 无 OneBot 对等接口 |
| `can_send_image` | 已实现，固定 `true` | 能力上支持图片发送 |
| `can_send_record` | 已实现，固定 `false` | 无稳定语音出站实现 |
| `send_private_msg` | 已实现，支持 `text/image/video/file` | 已实现，支持 `text/image/video/file` |
| `send_msg` | 已实现，但仅 `private` | 无 OneBot 包装；底层仅私聊直发 |
| `get_msg` | 已实现，本地消息库回查 | 无对等 API |
| `get_stranger_info` | 已实现，基于本地观测联系人 | 无联系人资料 API |
| `get_friend_list` | 已实现，基于本地观测联系人 | 无好友列表 API |
| `download_file` | 已实现，走 gocq 现有下载逻辑 | 有媒体下载/解密模块，但无 OneBot 接口 |
| `.handle_quick_operation` | 已实现，私聊 reply 走发送链路 | 无 OneBot 接口，但能力上可回复 |
| `send_group_msg` | 未实现，明确关闭 | 无稳定群聊实现 |
| `get_group_info` | 未实现 | 无 |
| `get_group_list` | 未实现 | 无 |
| `get_group_member_info` | 未实现 | 无 |
| `get_group_member_list` | 未实现 | 无 |
| `set_group_ban / set_group_kick / set_group_admin` | 未实现 | 无 |
| `set_friend_add_request / set_group_add_request` | 未实现 | 无 |
| `delete_msg` | 未实现 | 无 |
| `send_forward_msg / 合并转发` | 未实现 | 无 |
| `get_forward_msg` | 未实现 | 无 |
| `get_image / ocr_image` | 未实现 | 无 OneBot 对等接口 |
| `upload_group_file` | 未实现 | 无稳定群聊实现 |

## 非 OneBot v11 但已具备的能力

| oenbot11 | 当前实现 | openclaw-weixin |
| --- | --- | --- |
| `upload_private_file` | 已实现 | 能力上已实现 |
| `getconfig` | runtime 已实现，未对外暴露 | 已实现 |
| `sendtyping` | runtime 已实现，未对外暴露 | 已实现 |
| `QR 登录` | 已实现 | 已实现 |
| `getupdates` 长轮询 | 已实现 | 已实现 |
| `CDN 上传下载 + AES-128-ECB` | 已实现 | 已实现 |

## 结论

- 当前分支已经覆盖 `openclaw-weixin` 中“私聊可用”的主链路。
- 仍未实现的大块能力主要是群聊、管理、请求、撤回、转发和语音出站。
- `openclaw-weixin` 更接近“私聊型插件实现”，不是完整的 Weixin OneBot 内核。
