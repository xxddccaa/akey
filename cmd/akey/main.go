// Command akey: one encrypted master config -> native configs for codex,
// xdcode, claude, opencode and pi. Same binary is server and client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/xxddccaa/akey/internal/client"
	"github.com/xxddccaa/akey/internal/importer"
	"github.com/xxddccaa/akey/internal/server"
	"github.com/xxddccaa/akey/internal/spec"
)

// version is set with -ldflags "-X main.version=..."
var version = "dev"

const usageText = `akey — 一份加密主配置，一条命令配好 codex / xdcode / claude / opencode / pi
(one encrypted master config → native configs for every agent tool)

用法 / usage:
  akey serve  --listen :8787 --data DIR --dist DIR --host IP[,IP] [--insecure-http]
  akey info   --data DIR [--host IP[,IP]] [--listen :8787] [--insecure-http]
  akey pull   [--server URL] [--pin PIN] [--only codex,claude] [--dry-run] [--yes] [--install-missing]
  akey push   FILE [--server URL] [--pin PIN] [--admin-token T]
  akey edit   [--server URL] [--pin PIN] [--admin-token T]
  akey import [-o FILE] [-f]
  akey render FILE [--only ...] --out DIR
  akey version

命令 / commands:
  serve    在 VPS 上运行服务端（只保存密文，含网页面板）/ run the server + web panel (stores ciphertext only)
  info     重新打印服务端信息：面板地址、admin token、客户端一键命令 / reprint panel URL, admin token, client one-liners
  pull     输入口令 → 拉取解密 → 写入各工具配置（旧文件自动备份）/ fetch, decrypt, write configs (with backups)
  push     加密并上传本地 master.yaml / encrypt and upload a local master.yaml
  edit     拉取 → $EDITOR 编辑 → 校验 → 加密上传 / edit the stored master config in $EDITOR
  import   从本机现有配置生成 master.yaml 草稿（含密钥，0600）/ draft master.yaml from existing configs
  render   不经服务器，本地渲染到 DIR/<tool>/ 用于检查 / render locally for inspection
  version  显示版本 / print version

环境变量 / environment:
  AKEY_PASSPHRASE   非交互口令 / passphrase for non-interactive use
  AKEY_ADMIN_TOKEN  服务端 admin token（首次创建配置时需要）/ needed to create a new config

运行 "akey <command> -h" 查看各命令的参数。/ Run "akey <command> -h" for flags.
`

func main() {
	client.Version = version
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "info":
		err = cmdInfo(args)
	case "pull":
		err = cmdPull(ctx, args)
	case "push":
		err = cmdPush(ctx, args)
	case "edit":
		err = cmdEdit(ctx, args)
	case "import":
		err = cmdImport(args)
	case "render":
		err = cmdRender(args)
	case "version", "-v", "--version":
		fmt.Println("akey", version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "akey: unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "akey:", err)
		os.Exit(1)
	}
}

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: akey %s\n\nflags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse allows flags before and after positional arguments
// (e.g. `akey push master.yaml --server ...`).
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func isSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

type connFlags struct {
	server, pin, admin *string
}

func addConn(fs *flag.FlagSet, admin bool) connFlags {
	c := connFlags{
		server: fs.String("server", "", "server URL, e.g. https://1.2.3.4:8787 (default: saved in client.json)"),
		pin:    fs.String("pin", "", "server public key pin sha256//... (default: saved in client.json)"),
	}
	if admin {
		c.admin = fs.String("admin-token", "", "server admin token, only needed to create a new config (or AKEY_ADMIN_TOKEN)")
	}
	return c
}

func (c connFlags) opts(fs *flag.FlagSet) client.Options {
	o := client.Options{Server: *c.server, Pin: *c.pin, PinSet: isSet(fs, "pin")}
	if c.admin != nil {
		o.AdminToken = *c.admin
	}
	return o
}

func noArgs(fs *flag.FlagSet, pos []string) error {
	if len(pos) > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", pos[0])
	}
	return nil
}

func cmdServe(args []string) error {
	fs := newFlags("serve", "serve --listen :8787 --data DIR --dist DIR --host IP[,IP] [--insecure-http]")
	listen := fs.String("listen", ":8787", "listen address")
	data := fs.String("data", "", "data directory (params, admin token, TLS key, ciphertext blobs)")
	dist := fs.String("dist", "", "directory with akey-<os>-<arch>[.exe] binaries and SHA256SUMS, served under /dl/")
	hosts := fs.String("host", "", "public IP(s)/hostname(s), comma-separated; used in the TLS cert and printed install commands")
	insecure := fs.Bool("insecure-http", false, "plain HTTP without TLS (trusted LAN only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(fs, pos); err != nil {
		return err
	}
	if *data == "" {
		fs.Usage()
		return errors.New("--data is required")
	}
	return server.Run(server.Config{Listen: *listen, DataDir: *data, DistDir: *dist, Hosts: splitHosts(*hosts),
		InsecureHTTP: *insecure, Version: version})
}

func splitHosts(s string) []string {
	var hs []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hs = append(hs, h)
		}
	}
	return hs
}

func cmdInfo(args []string) error {
	fs := newFlags("info", "info --data DIR [--host IP[,IP]] [--listen :8787] [--insecure-http]")
	listen := fs.String("listen", ":8787", "the server's listen address (only the port is used)")
	data := fs.String("data", "", "the server's data directory (e.g. /var/lib/akey)")
	hosts := fs.String("host", "", "public IP(s)/hostname(s), comma-separated, for the printed URLs")
	insecure := fs.Bool("insecure-http", false, "the server runs with --insecure-http")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(fs, pos); err != nil {
		return err
	}
	if *data == "" {
		fs.Usage()
		return errors.New("--data is required")
	}
	return server.PrintInfo(server.Config{Listen: *listen, DataDir: *data, Hosts: splitHosts(*hosts),
		InsecureHTTP: *insecure, Version: version}, os.Stdout)
}

func cmdPull(ctx context.Context, args []string) error {
	fs := newFlags("pull", "pull [--server URL] [--pin PIN] [--only codex,claude] [--dry-run] [--yes] [--install-missing]")
	c := addConn(fs, false)
	only := fs.String("only", "", "comma-separated tools to write (default: all enabled): "+strings.Join(spec.ToolNames, ","))
	dry := fs.Bool("dry-run", false, "show what would change (secrets redacted), write nothing")
	yes := fs.Bool("yes", false, "accepted for scripts; pull never asks for confirmation (old files are backed up)")
	inst := fs.Bool("install-missing", false, "run tools.<name>.install for enabled tools not found on PATH")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(fs, pos); err != nil {
		return err
	}
	o := c.opts(fs)
	if o.Only, err = client.ParseOnly(*only); err != nil {
		return err
	}
	o.DryRun, o.Yes, o.InstallMissing = *dry, *yes, *inst
	return client.Pull(ctx, o)
}

func cmdPush(ctx context.Context, args []string) error {
	fs := newFlags("push", "push FILE [--server URL] [--pin PIN] [--admin-token T]")
	c := addConn(fs, true)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("push needs exactly one FILE")
	}
	return client.Push(ctx, pos[0], c.opts(fs))
}

func cmdEdit(ctx context.Context, args []string) error {
	fs := newFlags("edit", "edit [--server URL] [--pin PIN] [--admin-token T]")
	c := addConn(fs, true)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(fs, pos); err != nil {
		return err
	}
	return client.Edit(ctx, c.opts(fs))
}

func cmdRender(args []string) error {
	fs := newFlags("render", "render FILE [--only ...] --out DIR")
	only := fs.String("only", "", "comma-separated tools (default: all enabled)")
	out := fs.String("out", "", "output directory; files go to DIR/<tool>/...")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		fs.Usage()
		return errors.New("render needs FILE and --out DIR")
	}
	tools, err := client.ParseOnly(*only)
	if err != nil {
		return err
	}
	return client.RenderLocal(pos[0], tools, *out)
}

func cmdImport(args []string) error {
	fs := newFlags("import", "import [-o FILE] [-f]")
	outFile := fs.String("o", "master.yaml", "output file (written with mode 0600)")
	force := fs.Bool("f", false, "overwrite the output file if it exists")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(fs, pos); err != nil {
		return err
	}
	m, warns, err := importer.Import()
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "note:", w)
	}
	if err != nil {
		return err
	}
	body, err := importer.Marshal(m)
	if err != nil {
		return err
	}
	header := "# akey master config draft, generated by `akey import`.\n" +
		"# CONTAINS API KEYS — keep it private (mode 0600), delete it after `akey push`.\n" +
		"# Review providers/models/tools, then: akey push " + *outFile + "\n"
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(*outFile, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists; use -f to overwrite or -o to choose another file", *outFile)
	}
	if err != nil {
		return err
	}
	_ = f.Chmod(0o600)
	if _, err := io.WriteString(f, header); err == nil {
		_, err = f.Write(body)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d providers, %d models, tools: %s\n", *outFile, len(m.Providers), len(m.Models), strings.Join(m.EnabledTools(), ", "))
	fmt.Fprintf(os.Stderr, "WARNING: %s contains your API keys in plain text. Review it, run `akey push %s`, then delete it.\n", *outFile, *outFile)
	return nil
}
