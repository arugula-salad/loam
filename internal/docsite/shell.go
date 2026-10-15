package docsite

import "html/template"

// shellTmpl is every page's frame: the navigation beside the page on a wide
// screen and above it on a narrow one, light or dark as the reader's system
// is. It has no script and fetches nothing; the style is inline because the
// CSP allows nothing else.
var shellTmpl = template.Must(template.New("shell").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Home}}{{.Title}}{{else}}{{.Title}} · loam docs{{end}}</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #ffffff; --fg: #1f2328; --muted: #59636e; --line: #d1d9e0;
  --code-bg: #f6f8fa; --link: #0969da; --current: #eef1f4;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0d1117; --fg: #e6edf3; --muted: #9198a1; --line: #3d444d;
    --code-bg: #161b22; --link: #4493f8; --current: #1c2128;
  }
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body {
  margin: 0; background: var(--bg); color: var(--fg);
  font: 16px/1.6 system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}
a { color: var(--link); text-decoration: none; }
a:hover { text-decoration: underline; }
.wrap { display: flex; gap: 2rem; max-width: 66rem; margin: 0 auto; padding: 1rem; }
nav { flex: 0 0 13rem; font-size: 0.9rem; }
nav .home { display: block; font-weight: 600; font-size: 1.1rem; margin-bottom: 0.75rem; color: var(--fg); }
nav ul { list-style: none; margin: 0; padding: 0; }
nav li a { display: block; padding: 0.2rem 0.5rem; border-radius: 6px; color: var(--muted); }
nav li a[aria-current] { background: var(--current); color: var(--fg); font-weight: 600; }
main { flex: 1 1 auto; min-width: 0; max-width: 48rem; }
h1, h2, h3, h4 { line-height: 1.25; margin: 1.5em 0 0.5em; }
h1 { margin-top: 0.25em; font-size: 2rem; }
h2 { padding-bottom: 0.3em; border-bottom: 1px solid var(--line); }
code, pre { font: 0.875em/1.5 ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; }
:not(pre) > code { background: var(--code-bg); padding: 0.15em 0.35em; border-radius: 6px; overflow-wrap: anywhere; }
pre { background: var(--code-bg); padding: 0.75rem 1rem; border-radius: 6px; overflow-x: auto; }
pre code { font-size: 1em; }
table { display: block; max-width: 100%; overflow-x: auto; border-collapse: collapse; margin: 1em 0; }
th, td { border: 1px solid var(--line); padding: 0.35rem 0.7rem; text-align: left; vertical-align: top; }
th { background: var(--code-bg); }
blockquote { margin: 1em 0; padding: 0 1em; color: var(--muted); border-left: 0.25em solid var(--line); }
img { max-width: 100%; }
hr { border: 0; border-top: 1px solid var(--line); }
@media (max-width: 48rem) {
  .wrap { display: block; }
  nav { border-bottom: 1px solid var(--line); padding-bottom: 0.75rem; margin-bottom: 1rem; }
  nav ul { display: flex; flex-wrap: wrap; gap: 0.25rem; }
}
</style>
</head>
<body>
<div class="wrap">
<nav>
<a class="home" href="/docs">loam docs</a>
<ul>
{{- range .Nav}}
<li><a href="{{.Route}}"{{if .Current}} aria-current="page"{{end}}>{{.Title}}</a></li>
{{- end}}
</ul>
</nav>
<main>
{{.Body}}
</main>
</div>
</body>
</html>
`))

// landingTmpl is /docs: where to start, and every page.
var landingTmpl = template.Must(template.New("landing").Parse(`<h1>loam docs</h1>
<p>loam runs sandboxes: Firecracker microVMs you create, exec into, expose and checkpoint
over an HTTP API. The primer is the place to start; the reference has every route.</p>
<ul>
{{- range .}}
<li><a href="{{.Route}}">{{.Title}}</a></li>
{{- else}}
<li>No pages yet.</li>
{{- end}}
</ul>
`))
