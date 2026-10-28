// Command loomy2api 是 Loomy（讯飞）→ OpenAI/Anthropic 兼容网关的单二进制入口。
//
// 对应 Python 的 loomy2api/__main__.py：
//
//	if __name__ == "__main__":
//	    raise SystemExit(main())
package main

import (
	"os"

	"loomy2api/internal/loomy"
)

func main() {
	os.Exit(loomy.Main(os.Args[1:]))
}
