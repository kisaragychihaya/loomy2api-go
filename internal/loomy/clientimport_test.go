package loomy

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// 移植自 tests/test_client_import.py —— 桌面客户端登录态导入（不需要真客户端）。
//
// 每个用例以 t.TempDir() 为根，在 <root>/<sha256(user)[:12]>/userData/auth-session.json
// 造一份客户端会话文件，再构造账号池看它有没有被导入。目录名不参与解析（Pool 用
// “*” 通配），所以这里固定用 "abc123"，与 Python 用例一致。

// ciTestSetup 对应 TestClientImport.setUp：临时目录 + 客户端数据根 + 打开导入的配置。
func ciTestSetup(t *testing.T) (dir string, root string, cfg *Config) {
	t.Helper()
	dir = t.TempDir()
	root = filepath.Join(dir, "client")
	cfg = makeConfig(dir, "http://127.0.0.1:1/api/v1")
	cfg.ClientRoot = root
	cfg.SessionsFromClient = true
	return dir, root, cfg
}

// ciTestClientSession 写一份客户端 auth-session.json，返回其中的 updatedAt（毫秒）。
func ciTestClientSession(t *testing.T, root, session, userid string) int64 {
	t.Helper()
	dir := filepath.Join(root, "abc123", "userData")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	updatedAt := time.Now().UnixMilli()
	body := fmt.Sprintf(`{"session":%q,"userid":%q,"phone":"13800006207","updatedAt":%d}`,
		session, userid, updatedAt)
	if err := os.WriteFile(filepath.Join(dir, "auth-session.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("写 auth-session.json: %v", err)
	}
	return updatedAt
}

// ciTestWriteAccounts 写 {"accounts":[...]}。
func ciTestWriteAccounts(t *testing.T, dir string, accounts []map[string]any) {
	t.Helper()
	writeAccounts(t, filepath.Join(dir, "accounts.json"), accounts)
}

// ciTestNames 返回池里账号的名字（按池内顺序）。
func ciTestNames(pool *Pool) []string {
	names := make([]string, 0, len(pool.Accounts()))
	for _, acc := range pool.Accounts() {
		names = append(names, acc.Name)
	}
	return names
}

func TestClientImportCreatesPoolEntry(t *testing.T) {
	dir, root, cfg := ciTestSetup(t)
	updatedAt := ciTestClientSession(t, root, "client-session", "u1")
	ciTestWriteAccounts(t, dir, nil)

	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	names := ciTestNames(pool)
	if len(names) != 1 || names[0] != "desktop-6207" {
		t.Fatalf("池里的账号 = %v, want [desktop-6207]", names)
	}
	imported := pool.Accounts()[0]
	if imported.Source != "client" || imported.Persist {
		t.Fatalf("source/persist = %q/%v, want client/false", imported.Source, imported.Persist)
	}
	if imported.Session != "client-session" || imported.Userid != "u1" {
		t.Fatalf("session/userid = %q/%q", imported.Session, imported.Userid)
	}
	if want := updatedAt/1000 + SessionExpireSeconds; imported.ExpireAt != want {
		t.Fatalf("expire_at = %d, want %d", imported.ExpireAt, want)
	}
	if !imported.SessionValid() {
		t.Fatal("导入的会话应当有效")
	}
	if days, known := imported.DaysLeft(); !known || math.Abs(days-14) > 0.1 {
		t.Fatalf("days_left = %v（known=%v）, want ≈14", days, known)
	}
}

func TestImportedAccountIsNotPersisted(t *testing.T) {
	dir, root, cfg := ciTestSetup(t)
	ciTestClientSession(t, root, "client-session", "u1")
	ciTestWriteAccounts(t, dir, nil)

	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	pool.Save(true)

	raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("读取 accounts.json: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("解析 accounts.json: %v", err)
	}
	if items := jlist(obj, "accounts"); len(items) != 0 {
		t.Fatalf("客户端派生账号被写回了 accounts.json: %v", items)
	}
}

func TestSameUserIsNotDuplicated(t *testing.T) {
	// 一个账号的两份会话（我们自己的 + 客户端的）只算一条池记录。
	dir, root, cfg := ciTestSetup(t)
	ciTestClientSession(t, root, "client-session", "same-user")
	ciTestWriteAccounts(t, dir, []map[string]any{{
		"name": "main", "loginid": "13800006207", "password": "pw",
		"session": "own-session", "userid": "same-user",
		"expireAt": time.Now().Unix() + 14*86400,
	}})

	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if names := ciTestNames(pool); len(names) != 1 || names[0] != "main" {
		t.Fatalf("池里的账号 = %v, want [main]", names)
	}
}

func TestDifferentUserIsAdded(t *testing.T) {
	dir, root, cfg := ciTestSetup(t)
	ciTestClientSession(t, root, "client-session", "other-user")
	ciTestWriteAccounts(t, dir, []map[string]any{{
		"name": "main", "session": "own-session", "userid": "same-user",
		"expireAt": time.Now().Unix() + 14*86400,
	}})

	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	names := ciTestNames(pool)
	sort.Strings(names)
	if len(names) != 2 || names[0] != "desktop-6207" || names[1] != "main" {
		t.Fatalf("池里的账号 = %v, want [desktop-6207 main]", names)
	}
}

func TestImportCanBeDisabled(t *testing.T) {
	dir, root, cfg := ciTestSetup(t)
	ciTestClientSession(t, root, "client-session", "u1")
	ciTestWriteAccounts(t, dir, nil)
	cfg.SessionsFromClient = false

	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if names := ciTestNames(pool); len(names) != 0 {
		t.Fatalf("关掉导入后仍有账号: %v", names)
	}
}

func TestBrokenClientFileIsIgnored(t *testing.T) {
	dir, root, cfg := ciTestSetup(t)
	sessionDir := filepath.Join(root, "abc123", "userData")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "auth-session.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("写坏 JSON: %v", err)
	}
	ciTestWriteAccounts(t, dir, nil)

	pool, err := NewPool(cfg, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if names := ciTestNames(pool); len(names) != 0 {
		t.Fatalf("坏文件不该导入任何账号，实际: %v", names)
	}
}
