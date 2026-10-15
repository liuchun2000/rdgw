# Remotedialer 测试用例与场景汇总

## 文档信息

| 字段 | 内容 |
| --- | --- |
| 文档版本 | 1.0.0 |
| 更新时间 | 2026-10-12 |
| 对应方案 | implementation.md 1.5.1-port-host-disposition |
| 当前结论 | 基础单元测试通过；端到端网络和部分安全边界测试仍待实施 |

## 1. 已自动验证

### 1.1 核心隧道与 session

| 用例 | 预期 | 测试位置 |
| --- | --- | --- |
| 建立和关闭 session | session 状态、连接和关闭流程正确 | `session_test.go`、`session_serve_test.go` |
| 远端连接增删 | 正确维护 client/session key 映射 | `session_serve_test.go`、`session_test.go` |
| 连接数据转发 | 数据进入正确 connection，异常可关闭连接 | `session_serve_test.go` |
| pause/resume | 背压状态可以切换 | `session_serve_test.go` |
| connection ID 同步 | 编码、解码、差异计算和过期连接清理正确 | `session_sync_test.go` |
| session 关闭并发 | 关闭过程不发生死锁 | `session_test.go` |

### 1.2 缓冲区与连接并发

| 用例 | 预期 | 测试位置 |
| --- | --- | --- |
| 缓冲区超限 | 超过限制时返回错误 | `buffer_test.go` |
| 并发写入与 deadline | 并发写入时 deadline 处理稳定 | `connection_test.go` |
| WebSocket connection 基础行为 | 读写和关闭符合 connection 接口 | `wsconn_test.go` |

### 1.3 client 鉴权与 Origin

| 用例 | 预期 | 测试位置 |
| --- | --- | --- |
| 正确 client ID + token | 认证成功 | `server/main_test.go` |
| 错误 token | 认证失败 | `server/main_test.go` |
| 未允许 Origin | 认证失败 | `server/main_test.go` |
| token 快照读取 | token 和 Origin 从同一快照读取 | 当前单元测试覆盖结构；reload 并发边界仍待补充 |
| 常量时间 token 比较 | token 比较使用固定时间比较 API | `server/main.go` 静态实现，暂无基准测试 |

### 1.4 代理目标校验

| 用例 | 预期 | 测试位置 |
| --- | --- | --- |
| HTTP 默认端口 | `http` 使用 80 | `server/main_test.go` |
| HTTPS/WSS 默认端口 | `https/wss` 使用 443 | `server/main_test.go` |
| 显式端口 | 拨号地址和 HTTP Host 保留显式端口，如 `example.test:8443` | `server/main_test.go` |
| 非法 scheme | 拒绝 `tcp` 等不支持 scheme | `server/main_test.go` |
| 非法端口 | 拒绝 0、超范围和非数字端口 | `server/main_test.go` |
| 非法 authority | 拒绝多冒号和控制字符 | `server/main_test.go` |
| IPv6 authority | 当前仅保留基础兼容路径，完整 Host 方括号语义暂不作为本轮验收项 | 后置 |

### 1.5 peer TLS 配置

| 用例 | 预期 | 测试位置 |
| --- | --- | --- |
| 默认系统 CA | 未提供 RootCAs 时保留系统证书池行为 | `server_tls_test.go` |
| TLS 配置复制 | setter 不受调用方后续修改影响，最低 TLS 版本为 1.2 | `server_tls_test.go` |
| 拒绝弱配置 | nil、`InsecureSkipVerify`、TLS 1.1 配置被拒绝 | `server_tls_test.go` |
| setter 时序 | 已启动 peer 后不能修改 TLS 配置 | `server_tls_test.go` |
| peer URL 校验 | malformed URL、错误端口和错误 scheme 被拒绝 | `server_tls_test.go` |
| ServerName | 从 peer authority 提取不带端口的名称 | `server_tls_test.go` |

## 2. 已实现但尚未自动化覆盖的场景

| 场景 | 当前状态 | 风险/说明 |
| --- | --- | --- |
| client `-token-file` 读取和 `X-Tunnel-Token` 发送 | 已实现 | client 包暂无专门 CLI 集成测试 |
| single 模式 `/connect` GET + WebSocket Upgrade | 已实现 | 缺少 httptest 路由矩阵测试 |
| split 模式 connect/proxy 路由隔离 | 已实现 | 缺少双 listener 集成测试 |
| SOCKS5 未知 client 拒绝 | 已实现 | 缺少完整协议回归测试 |
| SOCKS5 listener Close/error channel | 已实现基础句柄 | 临时 Accept 错误分类、活动连接等待和全局生命周期仍待完善 |
| HTTPS/WSS 上游 TLS 开关 | 已实现 | 缺少真实证书握手测试 |
| HTTP Host 显式端口转发 | 已实现 | 当前仅验证目标值解析，缺少真实上游 Host 断言 |
| SOCKS5 域名 CONNECT | 代码路径存在，但复核发现域名内容在读取前校验，当前实现可能拒绝所有域名目标 | 必须后续修正并补协议回归测试 |
| 旧 client 无 token 启动 | 当前 `-token-file` 为空会直接退出 | 与旧命令兼容性存在冲突，需后续明确 auth 模式和兼容模式 |

## 3. 后置测试用例

| 分类 | 用例 |
| --- | --- |
| client 出站策略 | allowlist 命中/拒绝、CIDR、localhost、链路本地、DNS 重绑定和最终 IP 复核 |
| Origin 核心边界 | `Server.ServeHTTP` 直接入口、peer header 路径、自定义 authorizer 的 Origin 行为 |
| listener 生命周期 | bind 回滚、临时/永久 Accept 错误、SIGTERM 优雅关闭、活动连接回收、地址冲突规范化 |
| WS 路由 | `ws/wss` 非 GET、缺失 Upgrade、错误 Version/Key、普通 HTTP 请求误入 WS 路径 |
| SOCKS5 | 域名控制字符读取后校验、端口 0、IPv4/IPv6/域名 CONNECT、非法命令和超长字段 |
| TLS 集成 | 私有 CA 握手、hostname/IP SAN、证书失败重试且不降级 |
| client/peer 私有 CA | `-client-ca-file`、`-peer-ca-file` CLI 注入和实际 WSS 握手 |
| 可用性 | HTTP/WS/SOCKS5 超时、并发、空闲连接回收和每 client 限制 |
| 日志与 header | 动态 hop-by-hop 清理、完整 query/token/Cookie 脱敏、debug payload 告警 |

## 4. 最新 reviewer/architect 复核结论

### 4.1 已通过或按用户裁决接受

- 显式端口会保留在上游 HTTP/WebSocket `Host`，拨号地址和 TLS `ServerName` 分离。
- single/split 模式 `/connect` 的 GET + Upgrade 检查已存在。
- token 常量时间比较、token 文件 header 传递、peer TLS 最低版本和系统 CA 默认行为已通过静态复核。
- IPv6 authority 暂不纳入本轮阻断。
- SOCKS5 密码不与 token/ACL 绑定，按用户裁决不作为本轮阻断。
- allowlist、完整 timeout/concurrency、响应 header 清理、日志脱敏、upstreamTLS 并发和真实 CA/SAN/retry 测试后置。

### 4.2 复核发现、尚未处理

| 严重度 | 问题 | 影响 | 当前决定 |
| --- | --- | --- | --- |
| 高 | SOCKS5 域名字段在 `io.ReadFull` 前校验 | 域名目标可能全部失败，SSH/代理使用域名时不可用 | 后续修正 |
| 高/兼容性 | client 无 token 时强制要求 `-token-file` | 旧 client 启动命令可能退出；server 无 auth 文件时语义不一致 | 后续明确兼容策略 |
| 中 | 未配置 `-client-auth-file` 时非空 `X-Tunnel-ID` 可通过 | 公网 `/connect` 可伪造 client ID 建立 session | 后续处理，当前不得视为完整安全验收通过 |
| 高（部署条件） | single 模式 `/client/...` 与 `/connect` 共用 listener，代理路由不检查 client token | 若 single listener 可从公网直达，调用方可绕过 `/connect` 鉴权直接使用代理入口；生产必须确保该端口仅受控内网可达，或采用 split 并隔离 proxy listener | 后续收紧/验证网络边界 |
| 中 | 核心 `Server.ServeHTTP` 的 `CheckOrigin` 仍无条件放行 | peer 或直接核心 API 路径可绕过示例入口 Origin 策略 | 后续明确边界或下沉核心策略 |
| 中 | SOCKS5 listener 临时错误分类、活动连接等待不完整 | 临时错误可能触发整体停服，优雅关闭不完整 | 后续处理 |
| 中 | `-client-ca-file` / `-peer-ca-file` 尚未接入入口 | 当前 peer 使用系统 CA 默认校验；无法通过示例 CLI 配置私有 CA | 将 V4 标为未完成并后续实现 |

本轮复核结论：**端口修正通过，但整体方案只能判定为部分通过，不能称为完整验收通过。**

## 5. 验证命令

当前代码变更已执行并通过：

```text
gofmt -w server/main.go server/main_test.go
go test ./...
go vet ./...
git diff --check
```

真实私有 CA、hostname/IP SAN 和证书失败重试测试按用户决定暂不执行。

## 6. 手工联调清单

1. 使用 `-listener-mode=single` 启动旧命令，确认合法 client 可以连接。
2. 使用 `-listener-mode=split` 启动 connect/proxy 两个 HTTP listener，确认端口互相不能访问对方路由。
3. 通过 `/client/{id}/http/...` 和 `/client/{id}/https/...` 验证 HTTP/HTTPS 请求转发。
4. 通过 `/client/{id}/ws/...` 和 `/client/{id}/wss/...` 验证 WebSocket Upgrade 和双向数据。
5. 使用 SOCKS5 ProxyCommand 验证 SSH/TCP CONNECT。
6. 使用错误 token、未知 client ID 和未允许 Origin 验证 `/connect` 拒绝行为。
8. 使用显式端口目标验证上游收到的 `Host` 仍包含端口。
