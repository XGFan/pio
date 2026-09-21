# TODO

## [安全] 明文 HTTP 代理路径在 keep-alive 连接上泄露代理凭证、错投请求

**状态**：未修。2026-09-21 排查 avp 同步 jable 403 时发现（chrome-proxy 经 pio 出口），已由独立评审用 pio 真实 `HTTPProxy` + Chrome 端到端复现。

### 现象

客户端经 pio 的 HTTP 代理访问 `http://` 站点（非 CONNECT）时：

1. **凭证泄露**：同一条 keep-alive 连接上的第 2 个及之后的请求，会带着 `Proxy-Authorization: Basic …` 原样到达目标站点。复现：一个带 3 张图的 `http://` 页面，站点侧收到 7 个携带 `Proxy-Authorization` 的请求。
2. **错投**：后续请求会被送到**第一个请求的目标主机**。复现：同一连接先请求 A、再请求 B，B 被投递给 A 的源站，且带凭证。

CONNECT（https）路径不受影响：TLS 源站从未收到该头。

### 根因

`internal/listener/http_proxy.go`：

- `handleConn`（L127）只 `http.ReadRequest` **一次**（L136），据此认证、选上游。
- `handleAbsoluteForm`（L223）对这**第一个**请求 `StripHopByHop`（L242）、改写成 origin-form 发往上游，随后 `tunnel.Bridge(ctx, clientConn, upConn)`（L263）把连接剩余字节**原样双向透传**。
- Chrome 等客户端在首次认证成功后，会给同一连接上的每个后续请求都主动带 `Proxy-Authorization`。这些请求不再经过解析，既没有剥离代理头，也没有按各自的目标重新拨号。

### 影响面

- 所有经 pio 明文 HTTP 代理访问 `http://` 站点的客户端，包括订阅 `/subscription?type=http` 的 Chrome 扩展。扩展用的是**全局密码**（universal password），泄露后任何拿到它的人都能用 pio 的所有上游。
- 该头会发给任意第三方 http 站点（页面里的广告、跳转中间站等）。

### 已有缓解（不是修复）

chrome-proxy 的出口配置改成 Chrome 代理规则 `EGRESS_PROXY_SERVER=https=pio-proxy.default:8080`，只让 https 经 CONNECT 走 pio，明文 http 不进代理（infra `0fa6d47`）。这只保护 chrome-proxy，其它客户端仍受影响。

### 修复方向（二选一）

1. **逐请求处理**：在 `handleAbsoluteForm` 里循环 `http.ReadRequest`，对每个请求单独 `StripHopByHop`、改写成 origin-form；目标 authority 变化时关掉旧的上游连接、按新目标重新拨号（或每个上游连接只服务一个 authority）。响应也要逐个读取、转发，不能再用裸 `Bridge`。
2. **一问一答后关连接**（最简单）：转发第一个请求时强制 `Connection: close`，读完这一个响应就关闭客户端连接，让客户端为下一个请求新建连接（重新走 `handleConn` 的解析和认证）。代价是明文 http 失去连接复用。

### 回归测试（修复时必须补）

- 在 `test/integration/` 用**同一条 TCP 连接**先后发两个 absolute-form 请求（目标不同、都带 `Proxy-Authorization`），断言：
  - 两个源站都**没**收到 `Proxy-Authorization`；
  - 第二个请求到达的是**它自己的**目标，而不是第一个请求的目标。
- 注意：用 Go 的 `http.Client` + `http.ProxyURL` 不一定能稳定复用连接，最好用原始 `net.Conn` 手写两个请求，确保复现条件成立。
- 修复后可以把 chrome-proxy 的 `EGRESS_PROXY_SERVER` 改回 `http://…` 形式再验证；但 `https=` 形式本身也无害，可以保留。
