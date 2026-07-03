# FastClaw 钉钉消息通道 MVP 需求开发文档

## 1. 背景

FastClaw 已支持多种即时通讯渠道，包括 Telegram、Discord、Slack、LINE、WeChat、Feishu 等。当前客户需要在钉钉中使用 FastClaw Agent 机器人，核心诉求是“消息收发”：用户在钉钉私聊或群聊中向机器人发消息，FastClaw Agent 处理后回复到同一个钉钉会话。

钉钉官方提供了 `DingTalk-Real-AI/dingtalk-openclaw-connector`，这是 OpenClaw 的 TypeScript channel 插件，能力覆盖消息收发、AI Card、文档、日程、待办、DING、AI 表格等。客户当前不需要完整能力，只需要稳定的文本消息收发。因此本需求聚焦钉钉消息通道 MVP，不引入大面积自动化权限。

## 2. 目标

新增 FastClaw 原生钉钉 channel，使一个 FastClaw Agent 可以绑定一个钉钉企业内部机器人，实现：

- 接收钉钉私聊文本消息。
- 接收钉钉群聊文本消息。
- 群聊中默认仅在 @ 机器人时触发 Agent。
- Agent 回复文本或 Markdown 到原钉钉会话。
- 会话、用户、账号按 FastClaw 现有 IM 规则隔离。
- 支持进程内热启动和多实例 singleton lease。
- 后台 UI 可以配置钉钉机器人凭据。

## 3. 非目标

第一版不做以下能力：

- 钉钉文档、日程、待办、日志、DING、AI 表格等业务 API。
- AI Card 流式卡片和交互式卡片。
- 图片、语音、视频、文件收发。
- 主动向任意钉钉用户或群推送消息。
- 多 Agent 路由配置中的复杂协作模式。
- 钉钉扫码授权一键创建机器人流程。

这些能力可以在消息通道稳定后分阶段扩展。

## 4. 推荐技术方案

优先实现 Go 原生 adapter，形态与现有 WeChat iLink adapter 保持一致：

```text
DingTalk Stream
  -> internal/channels.DingTalk
  -> bus.InboundMessage
  -> Gateway routing
  -> Agent
  -> bus.OutboundMessage
  -> internal/channels.DingTalk.SendMessage
  -> DingTalk
```

优先 Go 原生的原因：

- FastClaw 当前 channel 生命周期、热启动、租户隔离、日志、lease 均在 Go 内统一管理。
- WeChat 也是 Go 原生 iLink adapter，不依赖 Node/TypeScript 子进程。
- 部署依赖更少，后续维护和排障更直接。

保留备用方案：

- 如果钉钉 Stream 协议在 Go 中无法稳定实现，或官方 npm 包 `dingtalk-stream` 封装了关键不可见细节，则实现一个很薄的 Node transport 子进程。
- Node 子进程只负责钉钉 Stream 收发，不桥接完整 OpenClaw connector。
- Go 与 Node 间传输标准化消息，Go 侧仍保持 FastClaw channel 语义。

## 5. 前置技术验证

正式开发前先做一个最小 spike，验证 Go 原生方案可行性：

1. 使用 Go 连接钉钉 Stream。
2. 收到一条私聊文本消息。
3. 正确 ack，避免重复投递。
4. 解析出 `conversationId`、`msgId`、`senderId`、`senderNick`、`sessionWebhook`。
5. 使用 `sessionWebhook` 回复一条文本。
6. 模拟断线后可以自动重连并继续收消息。

Go spike 成功后进入完整 MVP 开发。若失败，切换到 Node transport 子进程方案。

## 6. 数据映射

钉钉入站事件映射到 `bus.InboundMessage`：

| DingTalk 字段 | FastClaw 字段 | 说明 |
| --- | --- | --- |
| `conversationId` | `ChatID` | 会话键，私聊和群聊均使用 |
| `senderStaffId` / `senderId` | `UserID` | 平台侧用户 ID |
| `msgId` | `MessageID` | 消息去重主键之一 |
| `senderNick` | `SenderName` | 发送者展示名 |
| `conversationType == "2"` | `PeerKind = "group"` | 群聊 |
| `conversationType == "1"` | `PeerKind = "dm"` | 私聊 |
| at 列表 | `Mentions` | 用于群聊 @ 触发 |
| 文本内容 | `Text` | 首版仅处理文本/Markdown 可降级文本 |

FastClaw `AccountID` 建议使用钉钉 `clientId` 或 `robotCode`。若运行时能稳定获得 `robotCode`，优先使用 `robotCode`；否则使用 `clientId`，并确保与配置表 `credential_key` 保持一致。

## 7. 配置设计

新增 channel type：`dingtalk`。

最小配置字段：

```json
{
  "enabled": true,
  "accounts": {
    "<accountId>": {
      "clientId": "<DingTalk AppKey / Client ID>",
      "clientSecret": "<DingTalk AppSecret / Client Secret>",
      "requireMention": true
    }
  }
}
```

当前 `config.AccountConfig` 没有 `clientId/clientSecret/requireMention` 字段。实现时有两种选择：

- 扩展 `AccountConfig`，加入钉钉需要的字段。
- 临时复用现有字段：`accountID = clientId`，`BotToken = clientSecret`，`UserID` 或新字段承载机器人身份。

推荐扩展结构，避免字段语义继续混用。

## 8. 后端开发任务

1. 新增 `internal/channels/dingtalk.go`。
   - 实现 `channels.Channel` 接口。
   - 实现 Start、Send、SendMessage、SendTyping。
   - 首版 SendTyping 可以 no-op。

2. 新增钉钉 Stream 客户端。
   - 建立连接。
   - 订阅机器人消息 topic。
   - 收到消息后立即 ack。
   - 处理心跳、断线重连、指数退避。
   - 记录连接状态日志。

3. 新增消息解析。
   - 支持 text。
   - 支持 markdown 降级为文本。
   - 可选支持 richText 中的文本拼接。
   - 不支持的类型记录 debug 日志并跳过。

4. 新增出站回复。
   - 优先使用入站消息携带的 `sessionWebhook` 回复当前会话。
   - 如 `sessionWebhook` 需要按会话缓存，按 `conversationId` 保存短期缓存。
   - 文本过长时按平台限制切分或返回错误日志。

5. 接入 channel 注册。
   - 在 `internal/gateway/channels.go` 增加 `dingtalk` 分支。
   - 使用 `registerSingleton`，避免多实例重复收消息。

6. 接入配置保存接口。
   - 在 `internal/setup/handlers_agent_channels.go` 增加 connect handler。
   - 校验 `clientId/clientSecret` 必填。
   - 尽量做轻量验证；如果钉钉没有简单 validation API，可在保存后启动连接并通过状态反馈。
   - 保存 kind=channel row，`credential_key` 与 adapter 发出的 `AccountID` 一致。

7. 接入路由和去重。
   - 确保 `LookupChannelByCredential("dingtalk", accountID)` 能找到 owner。
   - 利用现有 gateway dedup。
   - 钉钉 adapter 内部也应对 `msgId` 做短期去重，防止上游重发穿透。

## 9. 前端开发任务

1. 在 Agent Channels 页面新增 DingTalk 入口。
2. 新增钉钉图标资源。
3. 新增 Connect DingTalk dialog。
   - `Client ID / AppKey`
   - `Client Secret / AppSecret`
   - `Require @mention in groups`
4. 调用新后端接口。
5. 连接成功后展示账号 ID 和测试提示。

## 10. 验证清单

本地和测试环境至少验证：

- 可以保存钉钉机器人配置。
- 保存后 channel 热启动，无需重启 FastClaw。
- 钉钉私聊发送“你好”，Agent 能回复。
- 钉钉群聊不 @ 机器人时不触发 Agent。
- 钉钉群聊 @ 机器人时触发 Agent 并回复。
- 同一钉钉用户多轮对话保留上下文。
- 不同钉钉用户映射到不同 FastClaw app_user。
- 断开网络或重启进程后，恢复连接可继续收消息。
- 多实例部署下只有 lease holder 接收 Stream 消息。
- 删除 channel 后停止 outbound routing，重启后不再注册。

## 11. 风险与待确认

- Go 是否有稳定可用的钉钉 Stream 协议实现，或是否需要自行实现 WebSocket 握手和 topic 处理。
- `sessionWebhook` 的有效期和使用限制，需要实测确认。
- 钉钉群聊 @ 机器人的字段结构需要用真实事件样本确认。
- 账号 ID 应使用 `clientId` 还是 `robotCode`，需要保证配置保存、入站 lookup、出站发送三者一致。
- 钉钉消息格式可能存在多版本差异，第一版应尽量保守解析。
- 如果客户后续要求 AI Card 或主动推送，当前 MVP 设计需要扩展发送层。

## 12. 里程碑

1. Spike：Go 连接钉钉 Stream，完成收一条消息和回复一条消息。
2. Backend MVP：Go adapter + 注册 + 配置保存 + 基础路由。
3. Frontend MVP：Channels 页面可配置钉钉。
4. E2E 验证：私聊、群聊、@、重连、去重。
5. 整理部署说明和客户配置步骤。


## 13. 钉钉应用配置占位

实际部署时从钉钉开放平台获取以下配置，并通过后台配置或安全的环境变量/密钥管理系统注入：

- Client ID：`<DINGTALK_CLIENT_ID>`
- Client Secret：`<DINGTALK_CLIENT_SECRET>`
- RobotCode：`<DINGTALK_ROBOT_CODE>`
