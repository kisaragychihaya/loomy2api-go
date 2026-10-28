package loomy

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// 对应 Python loomy2api/cli.py —— 命令行界面。
//
// 用法摘要与 cli.py 的模块 docstring 一致（build_parser 的 description，逐行照抄）：
//
//	loomy2api serve                 start the gateway
//	loomy2api accounts              show pool status (quota, sessions, cooldown)
//	loomy2api add <name> ...        add an account
//	loomy2api remove <name>         remove an account
//	loomy2api login [name ...]      log in / refresh sessions
//	loomy2api sms <phone>           send an SMS code (SMS login path)
//	loomy2api verify <name> <phone> <code> <msgid>
//	loomy2api models                list upstream models
//	loomy2api quota                 show quota of every account
//	loomy2api chat "prompt"         one-shot chat through the gateway
//
// 与 Python 的差异：
//
//   - argparse 换成每个子命令一个 flag.FlagSet。argparse 允许位置参数与选项混排
//     （`add b --phone 138…`），而 Go 的 flag 碰到第一个位置参数就停，所以解析前
//     先按 cliSplitFlags 把选项挑出来。
//   - 帮助文本是照抄 argparse 输出的常量（见 cliUsage*），不再由库生成。
//   - Python main 的 `except KeyboardInterrupt: return 130` 没有对应物：Go 里
//     SIGINT 的默认处置就是进程以 130 退出（serve 自己捕获并优雅停机）。
//   - 非 PoolError/AccountError 的异常在 Python 里是 traceback，这里降级成一行错误。
//   - 控制台编码：Python 版要在 _configure_stdio 里把 stdout 强转 UTF-8（Windows
//     遗留代码页），Go 的 os.Stdout 始终按字节输出 UTF-8，不需要这一步。

// CLI 是可测试的命令行门面：输出/输入可注入（对应 Python 里对 sys.stdout 的替换）。
type CLI struct {
	Out io.Writer // 默认 os.Stdout
	Err io.Writer // 默认 os.Stderr
	In  io.Reader // 默认 os.Stdin（remove 确认提示用）

	// serve 是 Serve 的注入点：nil → 包级 Serve(cfg, gw)。
	// Python 的测试靠 monkeypatch cli.serve 免于真的监听端口，这里留同样的接缝。
	serve func(cfg *Config, gw *Gateway) error
	// newPool 是账号池的注入点：nil → cliPool(cfg)。测试用它把 FakeAccountClient
	// 塞进 pool.Client（对应 Python 测试里 pool.client = FakeAccountClient()）。
	newPool func(cfg *Config) (*Pool, error)
}

// Main 是包级便捷入口：用真实的 os 流跑一次命令行（对应 Python 的 cli.main）。
func Main(argv []string) int {
	return (&CLI{Out: os.Stdout, Err: os.Stderr, In: os.Stdin}).Run(argv)
}

// Run 解析 argv（不含程序名）并执行子命令，返回进程退出码。
// 不带子命令时默认执行 serve；只有显式 -h/--help 才打帮助返回 0。
// 参数错误返回 2（argparse 的约定）；业务错误返回 1。
func (c *CLI) Run(argv []string) int {
	c.ensureStreams()

	global, err := cliParseGlobal(argv)
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 2
	}
	if global.help {
		// 显式请求帮助（全局位置的 -h/--help）：打帮助返回 0。
		fmt.Fprint(c.Out, cliHelp())
		return 0
	}
	if global.cmd == "" {
		// 不带子命令（argv 为空，或只给了全局参数）：默认按 serve 处理。
		global.cmd = "serve"
	}

	cmd := cliFind(global.cmd)
	if cmd == nil {
		fmt.Fprintf(c.Err, "loomy2api: 未知子命令 / invalid choice: %s\n\n%s",
			global.cmd, cliHelp())
		return 2
	}

	// 配置文件先落地：不存在就写一份内置默认模板（对所有子命令生效）。
	if created, cerr := EnsureConfigFile(global.config); cerr != nil {
		return c.fail(cerr)
	} else if created {
		fmt.Fprintf(c.Out, "已创建默认配置 %s\n", configPath(global.config))
	}

	cfg, err := LoadConfig(global.config)
	if err != nil {
		return c.fail(err)
	}

	code, runErr := cmd.run(c, cfg, global.rest)
	if runErr == nil {
		return code
	}
	if errors.Is(runErr, errCLIHelp) {
		fmt.Fprint(c.Out, cmd.usage)
		return 0
	}
	var argErr *cliArgError
	if errors.As(runErr, &argErr) {
		fmt.Fprintf(c.Err, "loomy2api %s: %s\n", argErr.name, argErr.msg)
		fmt.Fprint(c.Err, cmd.usage)
		return 2
	}
	return c.fail(runErr)
}

// ensureStreams 补上未注入的流（让 &CLI{} 也可用）。
func (c *CLI) ensureStreams() {
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}
	if c.In == nil {
		c.In = os.Stdin
	}
}

// fail 是 Run 的统一错误出口，对应 Python main 的
// `except (PoolError, AccountError): print(f"错误：{exc}", file=sys.stderr); return 1`。
// 其余错误（网络、UpstreamError…）在 Python 里会以 traceback 退出，这里同样返回 1，
// 只是打成一行错误，不把栈丢给终端用户。
func (c *CLI) fail(err error) int {
	var perr *PoolError
	var aerr *AccountError
	var msg string
	switch {
	case errors.As(err, &perr):
		msg = perr.Msg
	case errors.As(err, &aerr):
		msg = aerr.Msg
	default:
		msg = err.Error()
	}
	fmt.Fprintf(c.Err, "错误：%s\n", msg)
	return 1
}

// ------------------------------------------------------------- 全局参数

// cliGlobal 是全局参数（`-c/--config` 必须写在子命令之前，与 argparse 一致）。
type cliGlobal struct {
	cmd    string
	config string
	help   bool
	rest   []string
}

// cliParseGlobal 拆分「全局选项 + 子命令 + 子命令参数」。
func cliParseGlobal(argv []string) (cliGlobal, error) {
	var out cliGlobal
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-h" || arg == "--help":
			out.help = true
			return out, nil
		case arg == "-c" || arg == "--config":
			if i+1 >= len(argv) {
				return out, fmt.Errorf("loomy2api: %s 需要一个参数 / expected one argument", arg)
			}
			i++
			out.config = argv[i]
			continue
		case strings.HasPrefix(arg, "--config="):
			out.config = strings.TrimPrefix(arg, "--config=")
			continue
		case strings.HasPrefix(arg, "-c="):
			out.config = strings.TrimPrefix(arg, "-c=")
			continue
		case len(arg) > 1 && arg[0] == '-':
			return out, fmt.Errorf("loomy2api: 无法识别的参数 / unrecognized arguments: %s", arg)
		}
		out.cmd = arg
		out.rest = argv[i+1:]
		return out, nil
	}
	return out, nil
}

// ------------------------------------------------------------- 子命令参数

// errCLIHelp 表示子命令行要求帮助（-h/--help）；Run 会打该子命令的 usage 并返回 0。
var errCLIHelp = errors.New("cli: help requested")

// cliArgError 是子命令的参数错误：Run 打 usage 到 stderr 并返回 2（等价 argparse）。
type cliArgError struct {
	name string
	msg  string
}

func (e *cliArgError) Error() string { return e.name + ": " + e.msg }

// cliBadArgs 造一个参数错误。name 用 FlagSet 的名字，便于 Run 找到对应 usage。
func cliBadArgs(name, format string, args ...any) error {
	return &cliArgError{name: name, msg: fmt.Sprintf(format, args...)}
}

// cliParse 解析子命令参数，返回位置参数。
// 选项与位置参数可以混排（argparse 的行为）；-h/--help 返回 errCLIHelp。
func cliParse(fs *flag.FlagSet, argv []string) ([]string, error) {
	flags, positional := cliSplitFlags(fs, argv)
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errCLIHelp
		}
		return nil, cliBadArgs(fs.Name(), "%v", err)
	}
	return positional, nil
}

// cliSplitFlags 把 argv 拆成「选项段」与「位置参数段」。
// Go 的 flag 遇到第一个位置参数就停止解析，而 cli.py 的用法是混排的
// （`add b --phone 138…`），所以先归类再交给 FlagSet。
func cliSplitFlags(fs *flag.FlagSet, argv []string) (flags, positional []string) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.ContainsRune(name, '=') {
			continue // --flag=value 自带值
		}
		if cliFlagTakesValue(fs, name) && i+1 < len(argv) {
			i++
			flags = append(flags, argv[i])
		}
	}
	return flags, positional
}

// cliFlagTakesValue 判断已注册的 flag 是否要跟一个值（bool flag 除外）。
// 未注册的名字返回 false，交给 FlagSet 自己报「flag provided but not defined」。
func cliFlagTakesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if bf, isBool := f.Value.(interface{ IsBoolFlag() bool }); isBool && bf.IsBoolFlag() {
		return false
	}
	return true
}

// cliPositional 取第 i 个位置参数，缺失时报参数错误。
func cliPositional(name string, pos []string, i int, label string) (string, error) {
	if i >= len(pos) {
		return "", cliBadArgs(name, "缺少参数 %s / missing argument: %s", label, label)
	}
	return pos[i], nil
}

// cliNoExtra 检查没有多余的位置参数（argparse 会报 unrecognized arguments）。
func cliNoExtra(name string, pos []string, accepted int) error {
	if len(pos) <= accepted {
		return nil
	}
	return cliBadArgs(name, "多余的参数 / unrecognized arguments: %s",
		strings.Join(pos[accepted:], " "))
}

// ------------------------------------------------------------- 命令表

// cliCommand 是一个子命令：名字、帮助摘要、usage 文本与实现。
type cliCommand struct {
	name    string
	summary string
	usage   string
	run     func(c *CLI, cfg *Config, argv []string) (int, error)
}

// cliCommandList 按 cli.py build_parser 的顺序排列（共 11 个子命令）。
var cliCommandList = []cliCommand{
	{name: "serve", summary: "启动网关", usage: cliUsageServe, run: cliCmdServe},
	{name: "accounts", summary: "查看账号池状态", usage: cliUsageAccounts, run: cliCmdAccounts},
	{name: "add", summary: "添加账号", usage: cliUsageAdd, run: cliCmdAdd},
	{name: "remove", summary: "删除账号", usage: cliUsageRemove, run: cliCmdRemove},
	{name: "login", summary: "登录/续期账号", usage: cliUsageLogin, run: cliCmdLogin},
	{name: "sms", summary: "发短信验证码（短信登录第一步）", usage: cliUsageSms, run: cliCmdSms},
	{name: "verify", summary: "用验证码换 session", usage: cliUsageVerify, run: cliCmdVerify},
	{name: "identity", summary: "查看/重绑账号的设备标识", usage: cliUsageIdentity, run: cliCmdIdentity},
	{name: "models", summary: "列出上游模型", usage: cliUsageModels, run: cliCmdModels},
	{name: "quota", summary: "查看各账号积分", usage: cliUsageQuota, run: cliCmdQuota},
	{name: "chat", summary: "单次对话自检", usage: cliUsageChat, run: cliCmdChat},
}

// cliFind 按名字找子命令，找不到返回 nil。
func cliFind(name string) *cliCommand {
	for i := range cliCommandList {
		if cliCommandList[i].name == name {
			return &cliCommandList[i]
		}
	}
	return nil
}

// cliNames 返回逗号分隔的子命令名（帮助里的 {serve,accounts,…}）。
func cliNames() string {
	names := make([]string, 0, len(cliCommandList))
	for _, cmd := range cliCommandList {
		names = append(names, cmd.name)
	}
	return strings.Join(names, ",")
}

// cliHelpTemplate 是 argparse 的 parser.print_help() 输出，{cmds} 填子命令列表。
var cliHelpTemplate = `usage: loomy2api [-h] [-c CONFIG]
                 [命令 COMMAND] ...

Command line interface.

    不带命令时默认启动 serve / no command starts serve（等价于 loomy2api serve）

    loomy2api serve                 start the gateway
    loomy2api accounts              show pool status (quota, sessions, cooldown)
    loomy2api add <name> ...        add an account
    loomy2api remove <name>         remove an account
    loomy2api login [name ...]      log in / refresh sessions
    loomy2api sms <phone>           send an SMS code (SMS login path)
    loomy2api verify <name> <phone> <code> <msgid>
    loomy2api models                list upstream models
    loomy2api quota                 show quota of every account
    loomy2api chat "prompt"         one-shot chat through the gateway

positional arguments:
  {cmds}
    serve               启动网关
    accounts            查看账号池状态
    add                 添加账号
    remove              删除账号
    login               登录/续期账号
    sms                 发短信验证码（短信登录第一步）
    verify              用验证码换 session
    identity            查看/重绑账号的设备标识
    models              列出上游模型
    quota               查看各账号积分
    chat                单次对话自检

options:
  -h, --help            show this help message and exit
  -c CONFIG, --config CONFIG
                        config.json 路径
`

// cliHelp 生成全局帮助（对应 Python parser.print_help()）。
// 占位符 {cmds} 会展开成 {serve,accounts,…}（含花括号，与 argparse 一致）。
func cliHelp() string {
	return strings.ReplaceAll(cliHelpTemplate, "{cmds}", "{"+cliNames()+"}")
}

// 各子命令的 usage（与 argparse 打印的内容逐行一致）。
const (
	cliUsageServe = `usage: loomy2api serve [-h] [--port PORT]

options:
  -h, --help   show this help message and exit
  --port PORT  覆盖配置里的端口
`

	cliUsageAccounts = `usage: loomy2api accounts [-h]

options:
  -h, --help  show this help message and exit
`

	cliUsageAdd = `usage: loomy2api add [-h] [--phone PHONE] [--password PASSWORD]
                     [--session SESSION] [--no-login]
                     name

positional arguments:
  name

options:
  -h, --help           show this help message and exit
  --phone PHONE        手机号（登录用）
  --password PASSWORD  密码（可自动续期）
  --session SESSION    已有的 session（不想给密码时）
  --no-login           只保存不立即登录
`

	cliUsageRemove = `usage: loomy2api remove [-h] [-y] name

positional arguments:
  name

options:
  -h, --help  show this help message and exit
  -y, --yes   不确认直接删
`

	cliUsageLogin = `usage: loomy2api login [-h] [names ...]

positional arguments:
  names       账号名（缺省=全部）

options:
  -h, --help  show this help message and exit
`

	cliUsageSms = `usage: loomy2api sms [-h] phone

positional arguments:
  phone

options:
  -h, --help  show this help message and exit
`

	cliUsageVerify = `usage: loomy2api verify [-h] name phone code msgid

positional arguments:
  name
  phone
  code
  msgid

options:
  -h, --help  show this help message and exit
`

	cliUsageIdentity = `usage: loomy2api identity [-h] [--rebind] name

positional arguments:
  name

options:
  -h, --help  show this help message and exit
  --rebind    生成新标识并重新登录
`

	cliUsageModels = `usage: loomy2api models [-h]

options:
  -h, --help  show this help message and exit
`

	cliUsageQuota = `usage: loomy2api quota [-h]

options:
  -h, --help  show this help message and exit
`

	cliUsageChat = `usage: loomy2api chat [-h] [--model MODEL] prompt

positional arguments:
  prompt

options:
  -h, --help     show this help message and exit
  --model MODEL
`
)

// ------------------------------------------------------------- 账号池

// pool 对应 Python _pool：构造账号池，可被测试注入。
func (c *CLI) pool(cfg *Config) (*Pool, error) {
	if c.newPool != nil {
		return c.newPool(cfg)
	}
	return cliPool(cfg)
}

// cliPool 对应 Python _pool(cfg)：日志写 log_dir，并同时打控制台。
// Python 的 Logger 默认 console=True，所以这里与 log_console 配置无关。
func cliPool(cfg *Config) (*Pool, error) {
	logDir := cfg.LogDir
	if logDir == "" {
		logDir = "logs"
	}
	return NewPool(cfg, NewLogger(cfg.Path(logDir), "", true).Log)
}

// humanQuota 对应 Python _human_quota。
func humanQuota(acc *Account) string {
	if acc.Available == nil {
		return "额度未知"
	}
	return fmt.Sprintf("可用 %d = 余额 %s + 每日 %s",
		*acc.Available, cliOptInt(acc.Balance), cliOptInt(acc.DailyBalance))
}

// cliOptInt 把 *int64 打成 Python 的 str(v)：nil → "None"。
func cliOptInt(p *int64) string {
	if p == nil {
		return "None"
	}
	return strconv.FormatInt(*p, 10)
}

// cliTruncate 对应 Python 的 s[:n]（按码点切）。
func cliTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// cliReadLine 读一行输入（对应 Python 的 input()，EOF 视为空行）。
func cliReadLine(r io.Reader) string {
	if r == nil {
		return ""
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// cliIsAccountError 判断 err 是否是账号服务错误（Python 的 except AccountError）。
func cliIsAccountError(err error) bool {
	var aerr *AccountError
	return errors.As(err, &aerr)
}

// cliIsBusinessError 判断 err 是否是账号池/账号服务的可预期错误
// （Python 的 except (PoolError, AccountError)）。
func cliIsBusinessError(err error) bool {
	var perr *PoolError
	var aerr *AccountError
	return errors.As(err, &perr) || errors.As(err, &aerr)
}

// ---------------------------------------------------------------- serve

// cliCmdServe 对应 Python cmd_serve：--port 非 0 时覆盖配置，然后交给 Serve。
// Serve 内部负责建网关、挂信号、停止账号池，返回值即退出码的来源。
func cliCmdServe(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", 0, "覆盖配置里的端口")
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("serve", pos, 0); err != nil {
		return 0, err
	}
	if *port != 0 {
		cfg.Port = *port
	}
	serve := c.serve
	if serve == nil {
		serve = Serve
	}
	return 0, serve(cfg, nil)
}

// -------------------------------------------------------------- accounts

// cliCmdAccounts 对应 Python cmd_accounts：先刷新有登录态账号的额度，再打状态表。
func cliCmdAccounts(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("accounts", pos, 0); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	for _, acc := range pool.Accounts() {
		if acc.SessionValid() {
			pool.RefreshQuota(acc) // 天然容错：离线时降级成「额度未知」
		}
	}

	snap := pool.Snapshot()
	fmt.Fprintf(c.Out, "策略 %s · 账号 %d 个 · 可用 %d 个\n",
		jstr(snap, "strategy"), jint(snap, "count"), jint(snap, "usable"))
	fmt.Fprintln(c.Out, strings.Repeat("-", 78))
	fmt.Fprintf(c.Out, "%-18s%10s%10s%6s%7s  %s\n",
		"名称", "可用积分", "剩余天数", "请求", "扣分", "状态")

	for _, acc := range pool.Accounts() {
		days := "-"
		if d, known := acc.DaysLeft(); known {
			days = fmt.Sprintf("%.1f", d)
		}
		state := "正常"
		switch {
		case !acc.Enabled:
			state = "已禁用"
		case acc.InCooldown():
			state = fmt.Sprintf("冷却 %.0fs", acc.CooldownUntil-accountNow())
		case !acc.SessionValid():
			state = "无登录态"
		}
		available := "-"
		if acc.Available != nil {
			available = strconv.FormatInt(*acc.Available, 10)
		}
		fmt.Fprintf(c.Out, "%-18s%10s%10s%6d%7d  %s\n",
			acc.Name, available, days, acc.Requests, acc.PointsUsed, state)
		if acc.LastError != "" && state != "正常" {
			fmt.Fprintf(c.Out, "%-18s└─ %s\n", "", cliTruncate(acc.LastError, 60))
		}
	}
	return 0, nil
}

// ------------------------------------------------------------------ add

// cliCmdAdd 对应 Python cmd_add：加账号，有手机号+密码时顺手登录并查额度。
func cliCmdAdd(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	phone := fs.String("phone", "", "手机号（登录用）")
	password := fs.String("password", "", "密码（可自动续期）")
	session := fs.String("session", "", "已有的 session（不想给密码时）")
	noLogin := fs.Bool("no-login", false, "只保存不立即登录")
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	name, err := cliPositional("add", pos, 0, "name")
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("add", pos, 1); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	acc, err := pool.AddAccount(name, *phone, *password, *session, "", nil)
	if err != nil {
		return 0, err
	}
	if *phone != "" && *password != "" && !*noLogin {
		if serr := pool.EnsureSession(acc, true); serr != nil {
			fmt.Fprintf(c.Out, "账号已保存，但登录失败：%v\n", serr)
		} else {
			pool.RefreshQuota(acc)
			fmt.Fprintf(c.Out, "已添加并登录 %s：%s\n", acc.Name, humanQuota(acc))
		}
	} else {
		fmt.Fprintf(c.Out, "已添加账号 %s\n", acc.Name)
	}
	pool.Flush()
	return 0, nil
}

// --------------------------------------------------------------- remove

// cliCmdRemove 对应 Python cmd_remove：没有 -y 时从 In 读一次确认。
func cliCmdRemove(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yesShort := fs.Bool("y", false, "不确认直接删")
	yesLong := fs.Bool("yes", false, "不确认直接删")
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	name, err := cliPositional("remove", pos, 0, "name")
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("remove", pos, 1); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	if pool.Get(name) == nil {
		fmt.Fprintf(c.Out, "没有名为 %s 的账号\n", name)
		return 1, nil
	}
	if !*yesShort && !*yesLong {
		fmt.Fprintf(c.Out, "确认删除账号 %s？[y/N] ", name)
		answer := strings.ToLower(strings.TrimSpace(cliReadLine(c.In)))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(c.Out, "已取消")
			return 0, nil
		}
	}
	pool.RemoveAccount(name)
	fmt.Fprintf(c.Out, "已删除 %s\n", name)
	return 0, nil
}

// ---------------------------------------------------------------- login

// cliCmdLogin 对应 Python cmd_login：逐个强制重登并刷新额度，有失败返回 1。
func cliCmdLogin(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	targets := []*Account{}
	if len(pos) > 0 {
		for _, name := range pos {
			if acc := pool.Get(name); acc != nil {
				targets = append(targets, acc)
			}
		}
	} else {
		targets = pool.Accounts()
	}
	if len(targets) == 0 {
		fmt.Fprintln(c.Out, "没有要登录的账号（先用 loomy2api add 添加）")
		return 1, nil
	}

	failed := 0
	for _, acc := range targets {
		if serr := pool.EnsureSession(acc, true); serr != nil {
			failed++
			fmt.Fprintf(c.Out, "✗ %s: %v\n", acc.Name, serr)
			continue
		}
		pool.RefreshQuota(acc)
		days, known := acc.DaysLeft()
		if !known {
			days = 0
		}
		fmt.Fprintf(c.Out, "✓ %s: userid=%s session=%s… 剩余 %.1f 天 · %s\n",
			acc.Name, acc.Userid, accountPrefix(acc.Session, 8), days, humanQuota(acc))
	}
	pool.Flush()
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

// ------------------------------------------------------------------ sms

// cliCmdSms 对应 Python cmd_sms：发验证码并提示下一步。
func cliCmdSms(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("sms", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	phone, err := cliPositional("sms", pos, 0, "phone")
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("sms", pos, 1); err != nil {
		return 0, err
	}

	client := NewAccountClient(cfg)
	payload, err := client.SendSMSCode(phone, nil)
	if err != nil {
		if cliIsAccountError(err) {
			fmt.Fprintf(c.Out, "发送失败：%v\n", err)
			return 1, nil
		}
		return 0, err
	}
	msgid := ""
	if data := jmap(payload, "data"); data != nil {
		msgid = accountVal(data, "msgid")
	}
	if msgid == "" {
		msgid = accountVal(payload, "msgid")
	}
	fmt.Fprintf(c.Out, "验证码已下发到 %s，msgid=%s\n", phone, msgid)
	fmt.Fprintf(c.Out, "下一步：loomy2api verify <账号名> %s <验证码> %s\n", phone, msgid)
	return 0, nil
}

// --------------------------------------------------------------- verify

// cliCmdVerify 对应 Python cmd_verify：用验证码换 session，写回账号并落盘。
func cliCmdVerify(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	name, err := cliPositional("verify", pos, 0, "name")
	if err != nil {
		return 0, err
	}
	phone, err := cliPositional("verify", pos, 1, "phone")
	if err != nil {
		return 0, err
	}
	code, err := cliPositional("verify", pos, 2, "code")
	if err != nil {
		return 0, err
	}
	msgid, err := cliPositional("verify", pos, 3, "msgid")
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("verify", pos, 4); err != nil {
		return 0, err
	}

	client := NewAccountClient(cfg)
	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	got, err := client.LoginBySMS(phone, code, msgid, nil)
	if err != nil {
		if cliIsAccountError(err) {
			fmt.Fprintf(c.Out, "登录失败：%v\n", err)
			return 1, nil
		}
		return 0, err
	}
	acc := pool.Get(name)
	if acc == nil {
		// 新账号：loginid 就是手机号（对应 Python add_account(name, loginid=phone)）。
		acc, err = pool.AddAccount(name, phone, "", "", "", nil)
		if err != nil {
			return 0, err
		}
	}
	acc.Session = got["session"]
	acc.Userid = got["userid"]
	acc.ObtainedAt = time.Now().Unix()
	acc.ExpireAt = acc.ObtainedAt + SessionExpireSeconds
	pool.RefreshQuota(acc)
	pool.Save(true)
	fmt.Fprintf(c.Out, "✓ %s 登录成功 userid=%s session=%s…\n",
		acc.Name, acc.Userid, accountPrefix(acc.Session, 8))
	return 0, nil
}

// ------------------------------------------------------------- identity

// cliCmdIdentity 对应 Python cmd_identity：查看或重绑账号的设备标识。
func cliCmdIdentity(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("identity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rebind := fs.Bool("rebind", false, "生成新标识并重新登录")
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	name, err := cliPositional("identity", pos, 0, "name")
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("identity", pos, 1); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	acc := pool.Get(name)
	if acc == nil {
		fmt.Fprintf(c.Out, "没有名为 %s 的账号\n", name)
		return 1, nil
	}
	if *rebind {
		renewed, rerr := pool.RebindIdentity(name)
		if rerr != nil {
			if cliIsBusinessError(rerr) {
				fmt.Fprintf(c.Out, "✗ 重绑失败：%v\n", rerr)
				return 1, nil
			}
			return 0, rerr
		}
		if renewed != nil {
			acc = renewed
		}
		fmt.Fprintf(c.Out, "✓ %s 已绑定新设备标识 devid=%s\n",
			name, upStr(acc.Identity["devid"]))
	}

	view := acc.IdentityView()
	fmt.Fprintf(c.Out, "%s 设备标识：\n", name)
	for _, key := range []string{"devid", "ua", "modelid", "version", "campus_device_id"} {
		value := jstr(view, key)
		if value == "" {
			value = "—"
		}
		fmt.Fprintf(c.Out, "  %-18s%s\n", key, value)
	}
	if created, ok := view["created_at"]; ok && anthTruthy(created) {
		stamp := time.Unix(pyInt64(created), 0).Format("2006-01-02 15:04")
		fmt.Fprintf(c.Out, "  %-18s%s\n", "created_at", stamp)
	}
	if !anthTruthy(view["bound"]) {
		fmt.Fprintln(c.Out, "  （尚未绑定，下次登录或 --rebind 时生成）")
	}
	return 0, nil
}

// --------------------------------------------------------------- models

// cliCmdModels 对应 Python cmd_models：列出上游模型与能力标签。
func cliCmdModels(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("models", pos, 0); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	client := NewModelGateway(cfg)
	acc, err := pool.Acquire()
	if err != nil {
		return 0, err
	}
	payload, err := client.Models(acc.Session)
	if err != nil {
		return 0, err
	}

	fmt.Fprintf(c.Out, "%-26s%-10s%10s  %s\n", "id", "倍率", "上下文", "能力")
	fmt.Fprintln(c.Out, strings.Repeat("-", 78))
	for _, item := range jlist(payload, "data") {
		model, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		caps := jmap(model, "capabilities")
		tags := []string{}
		for _, tag := range []struct {
			label string
			on    bool
		}{
			{"reasoning", anthTruthy(caps["reasoning"])},
			{"vision", anthTruthy(caps["vision"])},
			{"tools", anthTruthy(caps["function_calling"])},
			{"audio", cliHasAudio(caps)},
		} {
			if tag.on {
				tags = append(tags, tag.label)
			}
		}

		context := "-"
		if value := model["context_length"]; anthTruthy(value) {
			context = upStr(value)
		}
		fmt.Fprintf(c.Out, "%-26s%-10s%10s  %s\n",
			jstr(model, "id"), cliMultiplier(anthStr(model["name"])), context,
			strings.Join(tags, ","))
	}
	return 0, nil
}

// cliMultiplier 对应 Python 的 `name[name.rfind("x") : -1] if "x" in name else "-"`：
// 从最后一个 "x" 截到倒数第二个字符（"(x1.0)" → "x1.0"）。
func cliMultiplier(name string) string {
	runes := []rune(name)
	idx := -1
	for i, r := range runes {
		if r == 'x' {
			idx = i
		}
	}
	if idx < 0 {
		return "-"
	}
	if idx >= len(runes)-1 {
		return "" // Python: name[-1:-1] == ""
	}
	return string(runes[idx : len(runes)-1])
}

// cliHasAudio 对应 Python 的 `"audio" in (caps.get("input_modalities") or [])`。
func cliHasAudio(caps map[string]any) bool {
	for _, item := range jlist(caps, "input_modalities") {
		if label, isStr := item.(string); isStr && label == "audio" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- quota

// cliCmdQuota 对应 Python cmd_quota：逐账号刷新额度并合计。
func cliCmdQuota(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("quota", pos, 0); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	total := int64(0)
	for _, acc := range pool.Accounts() {
		pool.RefreshQuota(acc)
		if acc.Available != nil {
			total += *acc.Available
		}
		fmt.Fprintf(c.Out, "%-18s%s\n", acc.Name, humanQuota(acc))
	}
	pool.Save(true)
	fmt.Fprintln(c.Out, strings.Repeat("-", 50))
	fmt.Fprintf(c.Out, "合计可用积分：%d\n", total)
	return 0, nil
}

// ----------------------------------------------------------------- chat

// cliCmdChat 对应 Python cmd_chat：单次非流式对话，打印内容、用量并记账。
func cliCmdChat(c *CLI, cfg *Config, argv []string) (int, error) {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	modelFlag := fs.String("model", "", "")
	pos, err := cliParse(fs, argv)
	if err != nil {
		return 0, err
	}
	prompt, err := cliPositional("chat", pos, 0, "prompt")
	if err != nil {
		return 0, err
	}
	if err := cliNoExtra("chat", pos, 1); err != nil {
		return 0, err
	}

	pool, err := c.pool(cfg)
	if err != nil {
		return 0, err
	}
	client := NewModelGateway(cfg)
	acc, err := pool.Acquire()
	if err != nil {
		return 0, err
	}

	model := *modelFlag
	if model == "" {
		model = cfg.DefaultModel
	}
	if model == "" {
		model = DefaultModel
	}
	payload := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
		"stream":   false,
	}

	started := accountNow()
	status, _, data, err := client.Call(acc.Session, "chat/completions", payload, nil, "POST")
	if err != nil {
		return 0, err
	}
	if status != 200 {
		fmt.Fprintf(c.Out, "HTTP %d: %s\n", status, httpcText(data, 400))
		return 1, nil
	}

	var obj map[string]any
	if jerr := json.Unmarshal(data, &obj); jerr != nil {
		return 0, jerr
	}
	message := map[string]any{}
	if choices := jlist(obj, "choices"); len(choices) > 0 {
		if first, isMap := choices[0].(map[string]any); isMap {
			message = jmap(first, "message")
		}
	}
	content := "None"
	if value, ok := message["content"]; ok && value != nil {
		content = anthStr(value)
	}
	fmt.Fprintf(c.Out, "[%s %.1fs] %s\n", acc.Name, accountNow()-started, content)

	// Python 是 json.dumps(usage, ensure_ascii=False)，键序即上游顺序；Go 的
	// encoding/json 会按键名排序、且不加 Python 默认的空格，内容等价。
	usage, uerr := jsonDumps(obj["usage"])
	if uerr != nil {
		usage = "null"
	}
	fmt.Fprintf(c.Out, "usage: %s\n", usage)

	pool.ReportSuccess(acc, ExtractUsage(obj))
	pool.Flush()
	return 0, nil
}
