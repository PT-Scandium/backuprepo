package web

import (
	"html/template"
	"net/http"
)

// row is one entry in the folder table.
type row struct {
	Name       string
	Path       string // absolute local path (for the delete form)
	Href       string // drill-in link for folders; empty for files
	IsDir      bool
	Type       string
	Size       string
	Modified   string
	Owner      string
	LastBackup string
}

type crumb struct {
	Name string
	Href string
}

type pageData struct {
	Username string
	Location string
	Crumbs   []crumb
	Path     string // current directory ("" at the root listing)
	Rows     []row
	AtRoot   bool
	Message  string
}

// render writes the main page HTML for data, sending a 500 if templating fails.
func render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// renderClosed writes the "web UI has been closed" confirmation page.
func renderClosed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = closedTmpl.Execute(w, nil)
}

// Warm color scheme per the spec: cream background, rust/amber accents, brown text.
const baseCSS = `
:root{--bg:#fff8f0;--panel:#fffdf9;--accent:#c0532b;--accent2:#e07b39;--text:#4a342e;--muted:#9b8579;--line:#f0dcc8;}
*{box-sizing:border-box}
body{margin:0;font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:var(--bg);color:var(--text)}
header{background:linear-gradient(135deg,var(--accent),var(--accent2));color:#fff;padding:18px 24px}
header h1{margin:0 0 4px;font-size:20px;letter-spacing:.3px}
header .meta{font-size:13px;opacity:.92}
header .meta b{font-weight:600}
main{max-width:1000px;margin:0 auto;padding:20px 24px 96px}
.crumbs{margin:4px 0 16px;font-size:14px}
.crumbs a{color:var(--accent);text-decoration:none}
.crumbs a:hover{text-decoration:underline}
.crumbs span.sep{color:var(--muted);margin:0 6px}
.msg{background:#fdeede;border:1px solid var(--line);color:var(--accent);padding:10px 14px;border-radius:8px;margin-bottom:16px;font-size:14px}
table{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);border-radius:10px;overflow:hidden}
th,td{padding:10px 14px;text-align:left;font-size:14px;border-bottom:1px solid var(--line)}
th{background:#fae7d4;color:var(--accent);font-weight:600}
tr:last-child td{border-bottom:none}
tr:hover td{background:#fffaf3}
td.num{text-align:right;font-variant-numeric:tabular-nums}
a.folder{color:var(--accent);text-decoration:none;font-weight:600}
a.folder:hover{text-decoration:underline}
.icon{font-size:15px}
.del{background:none;border:none;cursor:pointer;font-size:16px;color:var(--accent);padding:2px 6px;border-radius:6px}
.del:hover{background:#fbe0d3}
.bar{position:fixed;left:0;right:0;bottom:0;background:var(--panel);border-top:1px solid var(--line);padding:12px 24px;display:flex;gap:12px;justify-content:flex-end}
.btn{border:none;border-radius:8px;padding:10px 20px;font-size:14px;font-weight:600;cursor:pointer}
.btn-up{background:var(--accent);color:#fff}
.btn-up:hover{background:var(--accent2)}
.btn-close{background:#efe2d4;color:var(--text)}
.btn-close:hover{background:#e6d4c2}
.empty{color:var(--muted);font-style:italic;padding:18px 0}
`

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>backuprepo</title><style>` + baseCSS + `</style></head>
<body>
<header>
  <h1>backuprepo</h1>
  <div class="meta">User <b>{{.Username}}</b> &nbsp;·&nbsp; Server <b>{{.Location}}</b></div>
</header>
<main>
  <div class="crumbs">
    {{range $i, $c := .Crumbs}}{{if $i}}<span class="sep">/</span>{{end}}<a href="{{$c.Href}}">{{$c.Name}}</a>{{end}}
  </div>
  {{if .Message}}<div class="msg">{{.Message}}</div>{{end}}
  {{if .Rows}}
  <table>
    <thead><tr>
      <th>Filename</th><th>File Type</th><th class="num">File Size</th>
      <th>Last Modified</th><th>Modified By</th><th>Last Backup</th><th>Actions</th>
    </tr></thead>
    <tbody>
    {{range .Rows}}
      <tr>
        <td>{{if .IsDir}}<span class="icon">📁</span> <a class="folder" href="{{.Href}}">{{.Name}}</a>{{else}}<span class="icon">📄</span> {{.Name}}{{end}}</td>
        <td>{{.Type}}</td>
        <td class="num">{{.Size}}</td>
        <td>{{.Modified}}</td>
        <td>{{.Owner}}</td>
        <td>{{.LastBackup}}</td>
        <td>
          <form method="post" action="/delete" style="margin:0"
                onsubmit="return confirm('Permanently delete “{{.Name}}” from BOTH this computer and the backup? This cannot be undone.');">
            <input type="hidden" name="path" value="{{.Path}}">
            <button class="del" type="submit" title="Delete locally and from backup (unrecoverable)">🗑️</button>
          </form>
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{else if not .Message}}
  <div class="empty">This folder is empty.</div>
  {{end}}
</main>
<div class="bar">
  <form method="post" action="/upload" style="margin:0"><input type="hidden" name="path" value="{{.Path}}"><button class="btn btn-up" type="submit">Upload changed files</button></form>
  <form method="post" action="/close" style="margin:0" onsubmit="return confirm('Stop the web UI server?');"><button class="btn btn-close" type="submit">Close</button></form>
</div>
</body></html>`))

var closedTmpl = template.Must(template.New("closed").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>backuprepo — closed</title><style>` + baseCSS + `</style></head>
<body><header><h1>backuprepo</h1></header><main><div class="msg">The web UI has been closed. You can shut this tab.</div></main></body></html>`))
