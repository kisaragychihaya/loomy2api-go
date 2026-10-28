package loomy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 移植自 tests/test_cli.py —— CLI 接线层的回归网。
//
// Python 的 test_cli_survives_legacy_console_encoding 没有移植：它验证的是 Windows
// 遗留代码页下 Python 的 stdout 编码问题（对应 cli.py 的 _configure_stdio），Go 的
// os.Stdout 始终按字节写 UTF-8，没有对应的失败模式。

// cliTestEnv 对应 Python CliTestCase.setUp：临时目录 + 指向死端口的 config.json +
// 一个 14 天后到期的账号。
type cliTestEnv struct {
	dir     string
	cfgFile string
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	cli     *CLI
}

// cliTestSetup 造一份与 CliTestCase.setUp 等价的夹具。
func cliTestSetup(t *testing.T) *cliTestEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := makeConfig(dir, "http://127.0.0.1:1/api/v1")
	cfgFile := filepath.Join(dir, "config.json")
	// 死端口：任何意外的上游调用都会立刻失败（等价 CI 上连不到上游）。
	cliTestWriteConfig(t, cfgFile, cfg)
	writeAccounts(t, cfg.AccountsFile, []map[string]any{
		{"name": "a", "session": "s1", "expireAt": time.Now().Unix() + 14*86400},
	})

	env := &cliTestEnv{
		dir:     dir,
		cfgFile: cfgFile,
		out:     &bytes.Buffer{},
		errOut:  &bytes.Buffer{},
	}
	env.cli = &CLI{Out: env.out, Err: env.errOut, In: strings.NewReader("")}
	return env
}

// cliTestWriteConfig 按 cfg 写出一份 CLI 会读的 config.json。
func cliTestWriteConfig(t *testing.T, path string, cfg *Config) {
	t.Helper()
	cliTestWriteJSON(t, path, map[string]any{
		"accounts_file":        cfg.AccountsFile,
		"log_dir":              cfg.LogDir,
		"sessions_from_client": cfg.SessionsFromClient,
		"log_console":          cfg.LogConsole,
		"upstream":             cfg.Upstream,
	})
}

// setUpstream 把 config.json 指到另一个上游（本地假上游）。
func (e *cliTestEnv) setUpstream(t *testing.T, upstream string) {
	t.Helper()
	cliTestWriteConfig(t, e.cfgFile, makeConfig(e.dir, upstream))
}

// cliTestWriteJSON 把 payload 写成 JSON 文件。
func cliTestWriteJSON(t *testing.T, path string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("cliTestWriteJSON: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("cliTestWriteJSON: %v", err)
	}
}

// cliTestAccounts 读回账号文件里的账号名（按文件顺序）。
func cliTestAccounts(t *testing.T, cfgFile string) []string {
	t.Helper()
	cfg, err := LoadConfig(cfgFile)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	names := []string{}
	for _, acc := range pool.Accounts() {
		names = append(names, acc.Name)
	}
	return names
}

// run 跑一次 CLI（argv 不含程序名），返回退出码与 stdout。
func (e *cliTestEnv) run(argv ...string) (int, string) {
	e.out.Reset()
	e.errOut.Reset()
	code := e.cli.Run(append([]string{"-c", e.cfgFile}, argv...))
	return code, e.out.String()
}

// ------------------------------------------------------------ 帮助 / 参数

func TestCLIDefaultsToServe(t *testing.T) {
	// 没有子命令（argv 为空，或只给了全局参数）不再打帮助，而是直接跑 serve：
	// 这里用注入的 serve 替身顶替真的监听，断言它被调用并收到默认端口。
	t.Chdir(t.TempDir()) // 让默认配置路径 <cwd>/config.json 落在临时目录里

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	calls := 0
	gotPort := 0
	cli := &CLI{Out: out, Err: errOut, In: strings.NewReader("")}
	cli.serve = func(cfg *Config, gw *Gateway) error {
		calls++
		gotPort = cfg.Port
		return nil
	}

	for _, argv := range [][]string{nil, {}} {
		out.Reset()
		if code := cli.Run(argv); code != 0 {
			t.Fatalf("Run(%v) = %d, want 0（stdout：%s stderr：%s）", argv, code, out, errOut)
		}
		if strings.Contains(out.String(), "usage:") {
			t.Fatalf("缺省子命令不应打帮助：%s", out)
		}
	}
	if calls != 2 {
		t.Fatalf("serve 调用次数 = %d, want 2", calls)
	}
	if gotPort != 17890 {
		t.Fatalf("serve 收到的端口 = %d, want 17890", gotPort)
	}

	// 只带全局参数（-c）同样落到 serve。
	cfgFile := filepath.Join(t.TempDir(), "x.json")
	if code := cli.Run([]string{"-c", cfgFile}); code != 0 {
		t.Fatalf("Run(-c x.json) = %d, want 0（stderr：%s）", code, errOut)
	}
	if calls != 3 {
		t.Fatalf("serve 调用次数 = %d, want 3", calls)
	}

	// serve 返回错误时退出码为 1（与其它子命令一致）。
	cli.serve = func(cfg *Config, gw *Gateway) error { return errors.New("boom") }
	if code := cli.Run(nil); code != 1 {
		t.Fatalf("serve 失败 = %d, want 1", code)
	}
}

func TestCLIGlobalHelpFlag(t *testing.T) {
	// 显式 -h/--help 仍打全局帮助返回 0，且不启动 serve。
	env := cliTestSetup(t)
	called := false
	env.cli.serve = func(cfg *Config, gw *Gateway) error {
		called = true
		return nil
	}

	for _, flag := range []string{"-h", "--help"} {
		code, out := env.run(flag)
		if code != 0 {
			t.Fatalf("%s = %d, want 0", flag, code)
		}
		for _, want := range []string{"usage: loomy2api", "serve", "accounts", "chat",
			"不带命令时默认启动 serve"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s 帮助缺少 %q：%s", flag, want, out)
			}
		}
	}
	if called {
		t.Fatalf("-h 不应启动 serve")
	}
}

// ------------------------------------------------------------ 默认配置文件

func TestCLIEnsureConfigFile(t *testing.T) {
	// (a) 路径不存在（连目录都没有）→ 写出内置默认模板，内容能被 LoadConfig 解析。
	path := filepath.Join(t.TempDir(), "cfg", "config.json")

	created, err := EnsureConfigFile(path)
	if err != nil {
		t.Fatalf("EnsureConfigFile: %v", err)
	}
	if !created {
		t.Fatalf("首次调用 created = false, want true")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Port != 17890 {
		t.Fatalf("默认配置端口 = %d, want 17890", cfg.Port)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), `"_comment"`) {
		t.Fatalf("写出的默认配置缺 _comment 注释键：%s", raw)
	}

	// (b) 文件已存在 → created=false 且一个字节都不改（哪怕内容是畸形的）。
	if err := os.WriteFile(path, []byte("{ 畸形"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	created, err = EnsureConfigFile(path)
	if err != nil {
		t.Fatalf("再次 EnsureConfigFile: %v", err)
	}
	if created {
		t.Fatalf("已存在时 created = true, want false")
	}
	if raw, err = os.ReadFile(path); err != nil || string(raw) != "{ 畸形" {
		t.Fatalf("已存在的文件被改写：%q（err=%v）", raw, err)
	}
}

func TestCLICreatesConfigBeforeServe(t *testing.T) {
	// -c 指向连目录都不存在的路径：Run 先落地默认配置，再把配置交给 serve。
	cfgFile := filepath.Join(t.TempDir(), "sub", "dir", "config.json")

	served := false
	sourcePath := ""
	out := &bytes.Buffer{}
	cli := &CLI{Out: out, Err: &bytes.Buffer{}, In: strings.NewReader("")}
	cli.serve = func(cfg *Config, gw *Gateway) error {
		served = true
		sourcePath = cfg.SourcePath
		return nil
	}

	if code := cli.Run([]string{"-c", cfgFile}); code != 0 {
		t.Fatalf("Run(-c %s) = %d, want 0（输出：%s）", cfgFile, code, out)
	}
	if !served {
		t.Fatalf("serve 未被调用")
	}
	if sourcePath != filepath.Clean(cfgFile) {
		t.Fatalf("serve 收到的 SourcePath = %q, want %q", sourcePath, filepath.Clean(cfgFile))
	}
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("配置文件没被创建：%v", err)
	}
	if !strings.Contains(string(raw), `"_comment"`) {
		t.Fatalf("自动创建的配置缺 _comment 注释键：%s", raw)
	}
	if !strings.Contains(out.String(), "已创建默认配置") {
		t.Fatalf("缺少创建提示：%s", out)
	}
}

func TestCLISubcommandHelp(t *testing.T) {
	env := cliTestSetup(t)
	code, out := env.run("add", "--help")
	if code != 0 {
		t.Fatalf("add --help = %d, want 0", code)
	}
	if !strings.Contains(out, "usage: loomy2api add") {
		t.Fatalf("add --help 输出：%s", out)
	}
}

func TestCLIRejectsBadArguments(t *testing.T) {
	env := cliTestSetup(t)

	code, _ := env.run("nope")
	if code != 2 {
		t.Fatalf("未知子命令 = %d, want 2", code)
	}
	if !strings.Contains(env.errOut.String(), "未知子命令") {
		t.Fatalf("stderr：%s", env.errOut.String())
	}

	code, _ = env.run("add")
	if code != 2 {
		t.Fatalf("add 缺 name = %d, want 2", code)
	}
	if !strings.Contains(env.errOut.String(), "usage: loomy2api add") {
		t.Fatalf("stderr 缺 usage：%s", env.errOut.String())
	}
}

// ------------------------------------------------------------ 账号池视图

func TestCLIAccountsListsPool(t *testing.T) {
	env := cliTestSetup(t)
	code, out := env.run("accounts")
	if code != 0 {
		t.Fatalf("accounts = %d, want 0", code)
	}
	if !strings.Contains(out, "a") {
		t.Fatalf("accounts 没列出账号：%s", out)
	}
	if !strings.Contains(out, "策略") {
		t.Fatalf("accounts 缺策略行：%s", out)
	}
}

func TestCLIQuotaReportsTotal(t *testing.T) {
	env := cliTestSetup(t)
	code, out := env.run("quota")
	if code != 0 {
		t.Fatalf("quota = %d, want 0", code)
	}
	if !strings.Contains(out, "合计可用积分") {
		t.Fatalf("quota 缺合计行：%s", out)
	}
}

func TestCLICommandsTolerateUnreachableUpstream(t *testing.T) {
	// CI 连不到上游：额度刷新必须降级成「未知」而不是抛错（Python 的
	// test_commands_tolerate_unreachable_upstream 就是为这个矩阵加的）。
	env := cliTestSetup(t)
	for _, argv := range [][]string{{"accounts"}, {"quota"}} {
		code, out := env.run(argv...)
		if code != 0 {
			t.Fatalf("%v = %d, want 0", argv, code)
		}
		if !strings.Contains(out, "a") {
			t.Fatalf("%v 没列出账号：%s", argv, out)
		}
	}
}

// ------------------------------------------------------- models / chat（真上游）

func TestCLIModelsAndChat(t *testing.T) {
	// 这两条命令会真的调上游：用 FakeUpstream 当上游，验证表格与回显的形状。
	fake := &FakeUpstream{}
	base := fake.Start()
	defer fake.Stop()

	env := cliTestSetup(t)
	env.setUpstream(t, base)

	code, out := env.run("models")
	if code != 0 {
		t.Fatalf("models = %d, want 0（输出：%s）", code, out)
	}
	for _, want := range []string{"fake-model", "x1.0", "reasoning,tools"} {
		if !strings.Contains(out, want) {
			t.Fatalf("models 输出缺少 %q：%s", want, out)
		}
	}

	code, out = env.run("chat", "hi")
	if code != 0 {
		t.Fatalf("chat = %d, want 0（输出：%s）", code, out)
	}
	for _, want := range []string{"[a ", "echo:hi", "usage: "} {
		if !strings.Contains(out, want) {
			t.Fatalf("chat 输出缺少 %q：%s", want, out)
		}
	}
}

// ------------------------------------------------------------------ serve

func TestCLIServeOverridesPort(t *testing.T) {
	// Python 的 test_serve_passes_gateway_through：cmd_serve 必须覆盖端口，并把一份
	// 能用的配置交给 serve()（这里用注入的 serve 顶替真的监听）。
	env := cliTestSetup(t)

	gotPort := 0
	gotAccounts := -1
	env.cli.serve = func(cfg *Config, gw *Gateway) error {
		gotPort = cfg.Port
		if gw == nil {
			// 对应 Python fake_serve 的 `gateway or cli.Gateway(cfg)`。
			created, err := NewGateway(cfg, nil)
			if err != nil {
				return err
			}
			gw = created
		}
		gotAccounts = len(gw.Pool().Accounts())
		return nil
	}

	code, _ := env.run("serve", "--port", "19999")
	if code != 0 {
		t.Fatalf("serve --port = %d, want 0", code)
	}
	if gotPort != 19999 {
		t.Fatalf("serve 传给 Serve 的端口 = %d, want 19999", gotPort)
	}
	if gotAccounts != 1 {
		t.Fatalf("网关里的账号数 = %d, want 1", gotAccounts)
	}
}

// ------------------------------------------------------------ add / remove

func TestCLIAddAndRemove(t *testing.T) {
	env := cliTestSetup(t)

	code, out := env.run("add", "b", "--phone", "13800000000", "--password", "pw", "--no-login")
	if code != 0 {
		t.Fatalf("add = %d, want 0", code)
	}
	if !strings.Contains(out, "已添加账号 b") {
		t.Fatalf("add 输出：%s", out)
	}

	code, out = env.run("remove", "b", "-y")
	if code != 0 {
		t.Fatalf("remove = %d, want 0", code)
	}
	if !strings.Contains(out, "已删除 b") {
		t.Fatalf("remove 输出：%s", out)
	}
	if names := cliTestAccounts(t, env.cfgFile); len(names) != 1 || names[0] != "a" {
		t.Fatalf("删除后账号 = %v, want [a]", names)
	}
}

func TestCLIRemoveAsksForConfirmation(t *testing.T) {
	env := cliTestSetup(t)

	env.cli.In = strings.NewReader("n\n")
	code, out := env.run("remove", "a")
	if code != 0 || !strings.Contains(out, "已取消") {
		t.Fatalf("拒绝确认：code=%d out=%s", code, out)
	}
	if names := cliTestAccounts(t, env.cfgFile); len(names) != 1 {
		t.Fatalf("取消后账号 = %v, want 1 个", names)
	}

	env.cli.In = strings.NewReader("yes\n")
	code, out = env.run("remove", "a")
	if code != 0 || !strings.Contains(out, "已删除 a") {
		t.Fatalf("确认删除：code=%d out=%s", code, out)
	}
	if names := cliTestAccounts(t, env.cfgFile); len(names) != 0 {
		t.Fatalf("删除后账号 = %v, want 空", names)
	}
}

// ------------------------------------------------------------------ login

func TestCLILoginRefreshesSessions(t *testing.T) {
	env := cliTestSetup(t)
	// 有账号密码才能自动重登；FakeAccountClient 顶替真的账号服务。
	writeAccounts(t, cfgAccountsFile(t, env), []map[string]any{
		{"name": "a", "loginid": "13800000000", "password": "pw"},
	})

	fake := &FakeAccountClient{}
	env.cli.newPool = func(cfg *Config) (*Pool, error) {
		pool, err := cliPool(cfg)
		if err != nil {
			return nil, err
		}
		pool.Client = fake
		return pool, nil
	}

	code, out := env.run("login")
	if code != 0 {
		t.Fatalf("login = %d, want 0（输出：%s）", code, out)
	}
	if !strings.Contains(out, "✓ a:") {
		t.Fatalf("login 输出：%s", out)
	}
	if len(fake.Fills) != 1 || fake.Fills[0] != "a" {
		t.Fatalf("Fill 调用 = %v, want [a]", fake.Fills)
	}
}

func TestCLILoginReportsFailures(t *testing.T) {
	env := cliTestSetup(t)
	// 夹具里的账号只有 session、没有账号密码：重登必然失败。
	code, out := env.run("login")
	if code != 1 {
		t.Fatalf("login = %d, want 1（输出：%s）", code, out)
	}
	if !strings.Contains(out, "✗ a:") {
		t.Fatalf("login 输出：%s", out)
	}
}

// cfgAccountsFile 返回配置里 accounts_file 的绝对路径。
func cfgAccountsFile(t *testing.T, env *cliTestEnv) string {
	t.Helper()
	cfg, err := LoadConfig(env.cfgFile)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg.AccountsFile
}
