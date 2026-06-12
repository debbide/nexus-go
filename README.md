# OneImg-Go: 终极无痕探针与私有代理瑞士军刀

OneImg-Go 是一个专为受限环境（如小内存 VPS、PaaS 容器）深度定制的极致轻量化全能后端。它将 网页假装服务、Cloudflare 隧道穿透、哪吒探针 (Nezha Agent)，以及基于 Sing-box 的高性能 VLESS/TUIC 私有代理**完美融为一体**。

## 🌟 核心特性

- **纯净单体架构**：纯 Go 语言编写，`CGO_ENABLED=0` 静态编译，零系统底层依赖，完美适配 Alpine 等极简系统。
- **顶级代理引擎**：内置 Sing-box 核心，提供 `VLESS-WS` (适合过 CDN) 和 `TUIC` (UDP直连王者) 双协议支持。
- **免疫隧道假死**：深度重构 Cloudflare 隧道底层拨号代码，强制加入 `TCP Keep-Alive` 与 `HTTP/2 ReadIdleTimeout`，彻底免疫国内运营商掐断底层连接导致的假死死锁。
- **绝对无痕潜行**：生产环境下默认关闭所有标准输出与日志记录，内存占用极低。

---

## 🛠️ 环境变量配置指南 (Environment Variables)

您可以完全通过环境变量（或目录下的 `.env` 文件）来控制开启哪些功能。如果不填，大部分参数拥有智能默认值。

### 1. 全局与基础设置
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `UUID` | **核心凭证**，用于 VLESS 和 TUIC 认证 | `7bd180e8-1142...` |
| `PORT` 或 `SERVER_PORT` | 伪装 Web 服务与 VLESS-WS 监听的本地端口 | `3000` |
| `DEBUG` | 设为 `true` 开启终端日志输出（生产环境务必关闭）| `false` |

### 2. 代理服务设置 (Sing-box)
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `WSPATH` | VLESS-WS 客户端连接时需要填写的伪装路径 | 取 UUID 的前 8 位 |
| `TUIC_PORT` | TUIC 协议监听的独立 UDP 端口 | `30018` |
| `TUIC_DOMAIN` | TUIC 自动生成 TLS 自签证书使用的域名 | 自动获取公网IP或 `oneimg.local` |
| `TUIC_PASSWORD` | TUIC 用户的连接密码 | 同 `UUID` |

### 3. Cloudflare 隧道穿透 (可选)
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `CF_TUNNEL_TOKEN` | 您的 Cloudflare Argo Tunnel Token | 留空则不开启隧道 |

### 4. 哪吒探针监控 (Nezha Agent) (可选)
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `NEZHA_SERVER` | 哪吒面板的接入地址 (例如 `nz.abc.com:443`) | 留空则不开启探针 |
| `NEZHA_KEY` | 该节点在哪吒面板对应的 Secret Key | 无 |
| `NEZHA_TLS` | 是否开启 TLS (通常填 443 端口会自动开启) | 自动探测 |
| `NEZHA_DOH` | 自定义 DNS over HTTPS 地址 | 无 |

### 5. 自动唤醒与订阅
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `DOMAIN` | 您绑定的公网域名 | 无 |
| `SUB_PATH` | 获取节点订阅信息的隐藏路径 | `sub` |
| `AUTO_ACCESS` | 设为 `true` 则每隔一段时间自动请求一下域名以防容器休眠 | `false` |

---

## 🚀 部署与使用

**客户端配置提醒**：
- **VLESS 节点**：请在客户端务必将传输协议（Network）设置为 `ws` (WebSocket)，并在路径处填写 `WSPATH`。
- **TUIC 节点**：TUIC 使用的是自动生成的自签证书，请在客户端设置中务必勾选 **“跳过证书验证 (Allow Insecure / skip-cert-verify)”**，否则将无法连接。

---

## 📜 鸣谢与开源协议声明

- 本项目完全开源，并采用 **[GPL-3.0 License](./LICENSE)** 协议。
- 本项目代理模块核心引擎强力驱动自 [sing-box](https://github.com/sagernet/sing-box) (GPL-3.0 License)，特此致敬原作者。
- 本项目系统探针模块参考并使用了 [Nezha-Agent](https://github.com/naiba/nezha) 的优秀源码，特此感谢哪吒面板开发者团队（Apache-2.0 License）。
