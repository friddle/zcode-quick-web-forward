# Web Remote 参照客户端（官方 bundle 镜像）

把官方 web remote 前端**原样**镜像到本地跑起来，作为对接 Go 网桥的验收客户端。
不修改任何官方代码——它连的仍是 zcode.z.ai 的 relay 和我们的设备，所以它看到/发出
的每一帧都是真实协议。页面表现与官方桌面宿主不一致的地方，就是网桥的缺口。

## 搭建

```bash
# 1. 镜像官方页面（首次）
mkdir -p /tmp/zcode-web-mirror/remote/v4
curl -s 'https://zcode.z.ai/remote/v4?sid=dummy' -o /tmp/zcode-web-mirror/remote/v4/index.html
cd /tmp/zcode-web-mirror/remote/v4
grep -oE '"/remote/v4/assets/[^"]+"' index.html | tr -d '"' | sort -u | \
  xargs -P 8 -I{} sh -c 'curl -s -o "assets/$(basename {})" "https://zcode.z.ai{}"' --create-dirs

# 2. 起本地服务（懒加载缓存代理：本地没有的资源会自动从官方源拉取并缓存）
python3 serve.py 8899
```

## 使用

```bash
# 从远程 daemon 日志取最新配对 URL，把域名换成 localhost：
ssh friddle@100.124.32.41 'grep -oE "https://zcode.z.ai/remote/v4\?[^\ ]*" r.log | tail -1'
# → http://localhost:8899/remote/v4?sid=...&hash=...&t=...
```

用浏览器（或 Playwright）打开该 localhost 地址即可。注意 relay 每个设备同一时间
只允许一个终端页——测完把标签页关掉或导航走，别把用户自己的连接踢下线。

## 解码抓包

`decode_capture.py` 解码 WS 二进制帧（head 数组 + data 值，VSBuffer 变体）：

```bash
python3 decode_capture.py official-ws-frames.json   # 输出 /tmp/official-decoded.json
```

抓包方式：在页面上注入 WebSocket hook，把 binary frame 存成 `{seq,size,b64}` 数组。

## 为什么不直接改 index.js

官方 bundle 只含「客户端侧」：zod schema（校验/解析我们推的帧）、投影应用器、UI。
我们要实现的「宿主侧」（构建 conversation/sessions-index/tasks-index 帧）在官方代码里
没有可复用的实现——桌面宿主是闭源 Electron 应用。所以正确姿势是：官方客户端当
验收标准 + 本目录的夹具/文档当参照，宿主侧用 Go（或任何语言）对着夹具实现。

## 回车提交版（手机用）

官方 bundle 在「web 远控 + 手机视口」（`(max-width:767px) and (hover:none) and
(pointer:coarse)`）下把 composer 的 enterSubmits 硬编码为 false——手机上回车永远
只插换行，任务只能点发送箭头；而消息编辑条（rewind）的回车提交却没关，同一页面
两种回车习惯。daemon 是协议管道改不到官方页面 JS，所以通过本镜像出一个注入了
`enter_patch.js` 的版本：回车=提交任务（与点发送按钮同一条链路），输入法组词中
第一下回车只上屏确认、第二下才提交，Shift+Enter 仍为换行，@、/ 菜单打开时回车
仍是选词；桌面宽度不受影响（媒体查询不匹配时补丁直接放行）。

- 注入发生在每次响应（磁盘 index.html 保持原样），官方更新后懒拉取的新 HTML
  照样被注入；URL 加 `&zqfp=0` 可临时关掉补丁做 A/B 对照。
- 服务器部署：`/root/zcode-web-mirror/{serve.py,enter_patch.js}` +
  `zqf-webpage.service`（0.0.0.0:8899）。手机把配对链接的域名换成
  `http://<服务器IP>:8899` 即可，其余参数原样保留。
- 注意 relay 同一设备同一时间只允许一个终端页：用回车提交版时原版书签页会被
  顶下线，反之亦然。
