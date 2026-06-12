# OneImg-Go

OneImg-Go 是一个轻量级的后端服务程序。本项目集成了基础网页服务、Cloudflare Tunnel 客户端、Nezha (哪吒) 探针监控端，以及基于 Sing-box 的 VLESS/TUIC 代理节点。设计目标是精简体积并在极小内存的 VPS 或受限容器环境中稳定运行。

## 功能介绍
- **静态编译**：使用 `CGO_ENABLED=0` 编译，无系统库依赖，兼容 Alpine 等极简环境。
- **代理协议**：引入 Sing-box 核心库，支持 `VLESS-WS` 与 `TUIC` 协议直连。
- **网络优化**：针对 Cloudflare Tunnel 增加了 `TCP Keep-Alive` 和 `HTTP/2 ReadIdleTimeout`，优化在网络波动环境下的假死重连问题。
- **日志控制**：生产环境中默认丢弃标准输出，降低运行内存开销。

## 环境变量说明
本程序主要通过环境变量或程序目录下的 `.env` 文件进行配置。

### 基础配置
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `UUID` | VLESS 和 TUIC 认证的 UUID | `7bd180e8-1142...` |
| `PORT` | Web 服务与 VLESS-WS 监听的本地端口 | `3000` |
| `DEBUG` | 设为 `true` 输出日志调试信息 | `false` |

### 代理配置 (Sing-box)
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `WSPATH` | VLESS-WS 连接路径 | 取 UUID 前 8 位 |
| `TUIC_PORT` | TUIC 监听端口 (UDP) | `30018` |
| `TUIC_DOMAIN` | TUIC 自签 TLS 证书的域名 | 自动获取IP或 `oneimg.local` |
| `TUIC_PASSWORD` | TUIC 连接密码 | 同 `UUID` |

### 辅助功能 (可选)
| 变量名 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `CF_TUNNEL_TOKEN` | Cloudflare Argo Tunnel Token | 留空则不开启 |
| `NEZHA_SERVER` | 哪吒监控面板接入地址 (例: `nz.abc.com:443`) | 留空则不开启 |
| `NEZHA_KEY` | 哪吒监控节点 Secret | 无 |
| `NEZHA_TLS` | 是否开启哪吒监控 TLS | 自动依据端口识别 |
| `NEZHA_DOH` | 哪吒自定义 DNS over HTTPS (DoH) 地址 | 无 |
| `DOMAIN` / `SUB_PATH` | 自定义域名与获取订阅的隐藏路径 | 无 / `sub` |
| `AUTO_ACCESS` | 定时自身请求以保持服务存活 | `false` |

## 开源声明与致谢
- 本项目主体采用 **[GPL-3.0 License](./LICENSE)** 协议开源。
- 感谢 [sing-box](https://github.com/sagernet/sing-box) 团队提供的强大代理引擎 (GPL-3.0 License)，本项目核心协议能力均构建于其卓越的代码基石之上。
- 感谢 [哪吒监控 (Nezha)](https://github.com/naiba/nezha) 团队提供的优秀探针源码 (Apache-2.0 License)，为本项目提供了轻量可靠的监控参考实现。
- 致敬开源社区的无私奉献，让每一个微小的项目都能站在巨人的肩膀上。
