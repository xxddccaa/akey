# akey

一份主配置（`master.yaml`），一条命令配好 **codex / xdcode / claude / opencode / pi**。

主配置在本机加密后存到你自己的 VPS（没有域名也行，用 `IP:端口` + 自签证书公钥 PIN）。服务端只保存密文。到任何一台新机器上执行一行 `curl … | sh` 并输入口令，就会装好 `akey`，并为各工具写入原生配置（旧文件会自动备份）。

主配置格式见 [`testdata/master.example.yaml`](testdata/master.example.yaml)。

## 1. 部署服务端（VPS）

```sh
make dist                      # 交叉编译 dist/akey-{linux,darwin,windows}-{amd64,arm64}[.exe] + SHA256SUMS
scp -r dist deploy/akey.service root@VPS:/tmp/

# 在 VPS 上：
sudo install -m 0755 /tmp/dist/akey-linux-amd64 /usr/local/bin/akey
sudo mkdir -p /usr/local/share/akey/dist && sudo cp /tmp/dist/* /usr/local/share/akey/dist/
sudo cp /tmp/akey.service /etc/systemd/system/
sudo sed -i 's/YOUR_IP/1.2.3.4/' /etc/systemd/system/akey.service   # 换成 VPS 公网 IP
sudo systemctl daemon-reload && sudo systemctl enable --now akey
sudo ufw allow 8787/tcp        # 或在云厂商安全组放行 8787
sudo journalctl -u akey -n 60  # 查看 banner
```

banner 里有：TLS 公钥 PIN、Linux/macOS 和 Windows 的一键安装命令，以及**只显示一次**的 admin token（首次创建配置时需要；之后也能在 `/var/lib/akey/admin_token` 找到）。

## 2. 第一次推送主配置

在已经配好各工具的机器上：

```sh
akey import -o master.yaml     # 从 ~/.xdcode ~/.codex ~/.claude 等现有配置生成草稿（0600，含密钥）
$EDITOR master.yaml            # 检查 providers / models / tools
akey push master.yaml --server https://1.2.3.4:8787 --pin 'sha256//…' --admin-token …
rm master.yaml                 # 推送后删除明文
```

不想用 import 的话，直接 `akey edit --server … --pin …` 会从模板开始编辑。

## 3. 新机器一键安装

把 banner 里打印的命令原样复制过去执行，输入口令即可：

```sh
# Linux / macOS
curl -fsSk --pinnedpubkey 'sha256//…' https://1.2.3.4:8787/i | sh
# 只配部分工具：… | sh -s -- --only codex,claude
```

```powershell
# Windows PowerShell（banner 中那一行，带证书指纹校验）
[Net.ServicePointManager]::SecurityProtocol=…; iex (New-Object Net.WebClient).DownloadString('https://1.2.3.4:8787/i.ps1')
```

`akey` 装在 `~/.local/bin`（Windows：`%LOCALAPPDATA%\akey`），服务器地址和 PIN 存在 `~/.config/akey/client.json`（macOS：`~/Library/Application Support/akey/`，Windows：`%AppData%\akey\`）。

## 4. 日常使用

```sh
akey edit                # 任意机器：拉取 → $EDITOR 编辑 → 校验 → 加密上传（只需口令）
akey pull                # 每台机器：拉取最新配置并写入各工具
akey pull --dry-run      # 只看会改什么（diff，Key 已打码），不写文件
akey pull --only claude  # 只写指定工具
akey pull --install-missing   # 缺少的工具按 tools.<name>.install 自动安装
akey render master.yaml --out /tmp/out   # 不经服务器，本地渲染检查
```

每个被覆盖的文件先备份为 `<文件>.akey-bak-YYYYMMDD-HHMMSS`（每个文件保留最近 5 份），然后原子写入。非交互场景用 `AKEY_PASSPHRASE` / `AKEY_ADMIN_TOKEN` 环境变量。

## 多套配置

一个口令对应一份配置。想给另一组机器（比如内网机器）用不同的配置，换一个口令 `akey push` / `akey edit` 即可（新建时需要 admin token），不需要账号或 profile。

## 安全模型

- **端到端加密**：口令经 Argon2id（64 MiB, t=3）派生出存储 id、XChaCha20-Poly1305 加密密钥和写入凭据；加密密钥和明文永远不离开客户端。
- **服务端只见密文**：服务器（以及偷到磁盘的人）拿不到任何 Key；只存写入凭据的 sha256，每个 id 保留最近 10 个版本。
- **无域名也防中间人**：自签证书 + 公钥 PIN，安装脚本、二进制下载（再校验 SHA256SUMS）和所有 API 请求都校验 PIN。
- **写权限**：新建配置需要服务端 admin token；之后凭口令派生的写入凭据即可更新。
- **防暴力猜测**：错误口令得到 404 并按 IP 限速；口令就是唯一凭据，请用 12 位以上的长口令。
