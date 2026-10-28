package loomy

import _ "embed"

//go:embed web/index.html
var panelHTML []byte

// panelDocsHTML 是面板「文档」页的内容片段（不含 html/head/body，由
// /api/panel/docs 原样交给前端注入），同样编进二进制，离线可用。
//
//go:embed web/docs.html
var panelDocsHTML []byte
