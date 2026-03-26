package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	appName           = "liiy-filePlatform"
	defaultPort       = "8080"
	defaultDataDir    = "./data"
	defaultMaxUpload  = 32
	cleanupInterval   = time.Hour
	contentTTL        = 24 * time.Hour
	shutdownTimeout   = 10 * time.Second
	defaultMemStorage = 8 << 20
	minRetentionHours = 1
	maxRetentionHours = 24
)

type config struct {
	port           string
	dataDir        string
	maxUploadBytes int64
	maxUploadMB    int64
	configPath     string
	maxUploadFixed bool
	auth           authConfig
}

type server struct {
	mu             sync.RWMutex
	store          *Store
	maxUploadBytes int64
	maxUploadMB    int64
	configPath     string
	maxUploadFixed bool
	auth           authConfig
	sessions       *sessionManager
}

type pageData struct {
	AppName     string
	Flash       string
	Error       string
	MaxUploadMB int64
	ActiveTab   string
	FilterKind  string
	UploadMode  string
	OpenUpload  bool
	AuthEnabled bool
	CurrentUser string
	RetainHours int
	HourOptions []int
	Entries     []pageEntry
}

type pageEntry struct {
	ID           string
	Key          string
	OriginalName string
	Kind         string
	ContentType  string
	Size         string
	CreatedAt    string
	ExpiresAt    string
	Text         string
	RawURL       string
	DownloadURL  string
	RetainHours  int
}

const appScript = `(() => {
  const modal = document.getElementById("upload-modal");
  if (!modal) return;

  const picker = document.getElementById("upload-picker");
  const textPane = document.getElementById("upload-text-pane");
  const filePane = document.getElementById("upload-file-pane");
  const body = document.body;

  function setMode(mode) {
    const current = mode === "text" || mode === "file" ? mode : "";
    picker.classList.toggle("active", current === "");
    textPane.classList.toggle("active", current === "text");
    filePane.classList.toggle("active", current === "file");
    modal.dataset.mode = current;
  }

  function openModal(mode) {
    modal.classList.add("open");
    modal.setAttribute("aria-hidden", "false");
    body.classList.add("modal-open");
    setMode(mode);
  }

  function closeModal() {
    modal.classList.remove("open");
    modal.setAttribute("aria-hidden", "true");
    body.classList.remove("modal-open");
    setMode("");
  }

  document.querySelectorAll("[data-open-upload]").forEach((node) => {
    node.addEventListener("click", () => openModal(""));
  });

  document.querySelectorAll("[data-close-modal]").forEach((node) => {
    node.addEventListener("click", closeModal);
  });

  document.querySelectorAll(".choice-btn[data-mode]").forEach((node) => {
    node.addEventListener("click", () => openModal(node.dataset.mode));
  });

  document.querySelectorAll("[data-back]").forEach((node) => {
    node.addEventListener("click", () => setMode(""));
  });

  window.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && modal.classList.contains("open")) {
      closeModal();
    }
  });

  if (modal.dataset.open === "true") {
    openModal(modal.dataset.mode || "");
  } else {
    setMode("");
  }
})();`

var pageTemplate = template.Must(template.New("home").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.AppName}}</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #edf0e8;
      --panel: rgba(255, 255, 255, 0.94);
      --panel-strong: #ffffff;
      --text: #18211b;
      --muted: #607065;
      --line: rgba(24, 33, 27, 0.11);
      --accent: #1d6f5a;
      --accent-soft: rgba(29, 111, 90, 0.1);
      --accent-strong: #124d40;
      --warn: #a13e31;
      --shadow: 0 18px 44px rgba(24, 33, 27, 0.08);
    }

    * { box-sizing: border-box; }

    body {
      margin: 0;
      min-height: 100vh;
      font-family: "Segoe UI Variable Text", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
      color: var(--text);
      background:
        radial-gradient(circle at top left, rgba(29, 111, 90, 0.12), transparent 24rem),
        linear-gradient(180deg, #f7f8f3 0%, var(--bg) 100%);
    }

    body.modal-open {
      overflow: hidden;
    }

    a, button, input, textarea {
      font: inherit;
    }

    .page {
      width: min(1160px, calc(100vw - 2rem));
      margin: 0 auto;
      padding: 1.5rem 0 3rem;
    }

    .shell {
      display: grid;
      gap: 1rem;
      margin-bottom: 1rem;
      padding: 1.4rem;
      border-radius: 1.6rem;
      background: var(--panel);
      border: 1px solid var(--line);
      box-shadow: var(--shadow);
    }

    .eyebrow,
    .mini-tag {
      display: inline-flex;
      align-items: center;
      width: fit-content;
      padding: 0.34rem 0.72rem;
      border-radius: 999px;
      background: var(--accent-soft);
      color: var(--accent);
      font-size: 0.8rem;
      font-weight: 700;
      letter-spacing: 0.06em;
      text-transform: uppercase;
    }

    h1, h2, h3 {
      margin: 0;
      line-height: 1.08;
    }

    h1 {
      margin-top: 0.8rem;
      font-size: clamp(2rem, 4vw, 3rem);
    }

    .headline {
      margin: 0.7rem 0 0;
      max-width: 44rem;
      color: var(--muted);
      line-height: 1.7;
    }

    .tabs {
      display: inline-flex;
      width: fit-content;
      gap: 0.45rem;
      padding: 0.35rem;
      border-radius: 999px;
      background: #eef2ec;
      border: 1px solid rgba(24, 33, 27, 0.06);
    }

    .tab {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-width: 7rem;
      padding: 0.8rem 1.1rem;
      border-radius: 999px;
      color: var(--muted);
      text-decoration: none;
      font-weight: 700;
      transition: 120ms ease;
    }

    .tab.active {
      color: #fff;
      background: var(--accent);
      box-shadow: 0 12px 24px rgba(29, 111, 90, 0.24);
    }

    .toolbar {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 0.9rem;
    }

    .session-tools {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .logout-form {
      margin: 0;
    }

    .panel {
      padding: 1.3rem;
      border-radius: 1.45rem;
      background: var(--panel);
      border: 1px solid var(--line);
      box-shadow: var(--shadow);
    }

    .notice {
      margin-bottom: 1rem;
      padding: 0.9rem 1rem;
      border-radius: 1rem;
      border: 1px solid var(--line);
      font-size: 0.94rem;
    }

    .notice.ok {
      color: var(--accent);
      background: rgba(29, 111, 90, 0.08);
    }

    .notice.err {
      color: var(--warn);
      background: rgba(161, 62, 49, 0.08);
    }

    .upload-stage {
      display: grid;
      place-items: center;
      min-height: 24rem;
      text-align: center;
      background:
        radial-gradient(circle at top, rgba(29, 111, 90, 0.08), transparent 18rem),
        var(--panel);
    }

    .upload-card {
      display: grid;
      justify-items: center;
      gap: 1rem;
      width: min(32rem, 100%);
      padding: 2rem 1rem;
    }

    .upload-card p,
    .helper,
    .download-copy,
    .entry-info,
    .empty,
    .meta-copy {
      margin: 0;
      color: var(--muted);
      line-height: 1.7;
    }

    .primary-btn,
    .secondary-btn,
    .ghost-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: fit-content;
      border: 0;
      text-decoration: none;
      cursor: pointer;
      transition: 140ms ease;
    }

    .primary-btn {
      min-width: 9rem;
      padding: 1rem 1.3rem;
      border-radius: 999px;
      background: var(--accent);
      color: #fff;
      font-weight: 700;
      box-shadow: 0 14px 28px rgba(29, 111, 90, 0.24);
    }

    .primary-btn:hover,
    .secondary-btn:hover,
    .ghost-btn:hover,
    .filter:hover {
      transform: translateY(-1px);
    }

    .info-grid {
      display: grid;
      gap: 1rem;
      margin-top: 1rem;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    }

    .info-card {
      padding: 1rem;
      border-radius: 1.2rem;
      background: #f8faf6;
      border: 1px solid rgba(24, 33, 27, 0.08);
    }

    .info-card strong {
      display: block;
      margin-bottom: 0.45rem;
      font-size: 1rem;
    }

    .download-head {
      display: grid;
      gap: 1.2rem;
      margin-bottom: 1rem;
    }

    .filter-row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.7rem;
    }

    .filter {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0.72rem 1rem;
      border-radius: 999px;
      border: 1px solid rgba(24, 33, 27, 0.1);
      color: var(--muted);
      background: #f5f7f2;
      text-decoration: none;
      font-weight: 700;
      transition: 140ms ease;
    }

    .filter.active {
      color: #fff;
      border-color: transparent;
      background: var(--accent-strong);
      box-shadow: 0 12px 26px rgba(18, 77, 64, 0.2);
    }

    .grid {
      display: grid;
      gap: 1rem;
      grid-template-columns: repeat(auto-fill, minmax(285px, 1fr));
    }

    .entry {
      display: grid;
      gap: 1rem;
      min-height: 15rem;
      padding: 1rem;
      border-radius: 1.25rem;
      background: var(--panel);
      border: 1px solid var(--line);
      box-shadow: 0 10px 26px rgba(24, 33, 27, 0.06);
    }

    .entry-meta,
    .value {
      display: grid;
      gap: 0.7rem;
      align-content: start;
    }

    .entry-label {
      font-size: 0.76rem;
      color: var(--muted);
      text-transform: uppercase;
      letter-spacing: 0.08em;
    }

    .entry-key {
      font-size: 1.05rem;
      font-weight: 700;
      line-height: 1.4;
      word-break: break-word;
    }

    .text-content {
      margin: 0;
      padding: 0.95rem;
      max-height: 20rem;
      overflow: auto;
      border-radius: 1rem;
      border: 1px solid rgba(24, 33, 27, 0.08);
      background: #f7f8f4;
      white-space: pre-wrap;
      word-break: break-word;
      font-family: "Iosevka Term", "Cascadia Code", "SFMono-Regular", monospace;
      font-size: 0.9rem;
      line-height: 1.55;
    }

    .image-box {
      min-height: 12rem;
      display: grid;
      place-items: center;
      overflow: hidden;
      border-radius: 1rem;
      border: 1px solid rgba(24, 33, 27, 0.08);
      background: linear-gradient(135deg, rgba(29, 111, 90, 0.08), rgba(255, 255, 255, 0.95));
    }

    .image-box img {
      display: block;
      max-width: 100%;
      max-height: 18rem;
      object-fit: contain;
    }

    .file-box {
      display: grid;
      gap: 0.75rem;
      padding: 1rem;
      border-radius: 1rem;
      border: 1px solid rgba(24, 33, 27, 0.08);
      background: #f7f8f4;
    }

    .file-icon {
      width: 4rem;
      height: 4rem;
      display: grid;
      place-items: center;
      border-radius: 1rem;
      background: var(--accent-soft);
      color: var(--accent);
      font-size: 0.8rem;
      font-weight: 800;
      letter-spacing: 0.08em;
    }

    .file-name {
      font-weight: 700;
      word-break: break-word;
    }

    .actions {
      display: flex;
      flex-wrap: wrap;
      gap: 0.7rem;
    }

    .actions a {
      color: var(--accent);
      text-decoration: none;
      font-weight: 700;
    }

    .empty {
      padding: 1.5rem;
      border-radius: 1.3rem;
      border: 1px dashed rgba(24, 33, 27, 0.2);
      background: rgba(255, 255, 255, 0.68);
    }

    .modal {
      position: fixed;
      inset: 0;
      display: none;
      z-index: 50;
    }

    .modal.open {
      display: block;
    }

    .modal-backdrop {
      position: absolute;
      inset: 0;
      background: rgba(15, 20, 17, 0.5);
    }

    .modal-dialog {
      position: relative;
      width: min(32rem, calc(100vw - 1.5rem));
      margin: 6vh auto;
      padding: 1.25rem;
      border-radius: 1.5rem;
      background: var(--panel-strong);
      border: 1px solid rgba(24, 33, 27, 0.08);
      box-shadow: 0 24px 60px rgba(15, 20, 17, 0.2);
    }

    .modal-head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 1rem;
    }

    .ghost-btn {
      padding: 0.65rem 0.9rem;
      border-radius: 999px;
      background: #eef2ec;
      color: var(--text);
      font-weight: 700;
    }

    .choice-grid {
      display: grid;
      gap: 0.85rem;
      margin-top: 1rem;
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }

    .choice-btn {
      padding: 1.1rem;
      border-radius: 1.2rem;
      border: 1px solid rgba(24, 33, 27, 0.1);
      background: #f7f8f4;
      color: var(--text);
      text-align: left;
      cursor: pointer;
    }

    .choice-btn strong {
      display: block;
      margin-bottom: 0.35rem;
      font-size: 1rem;
    }

    .upload-pane {
      display: none;
    }

    .upload-pane.active {
      display: block;
    }

    form {
      display: grid;
      gap: 0.9rem;
      margin-top: 1rem;
    }

    label {
      display: grid;
      gap: 0.45rem;
      color: var(--muted);
      font-size: 0.93rem;
    }

    input, textarea {
      width: 100%;
      padding: 0.9rem 1rem;
      border-radius: 1rem;
      border: 1px solid rgba(24, 33, 27, 0.14);
      background: #fff;
      color: var(--text);
    }

    textarea {
      min-height: 12rem;
      resize: vertical;
      line-height: 1.55;
    }

    input[type="file"] {
      padding: 0.75rem 0.9rem;
    }

    select {
      width: 100%;
      padding: 0.9rem 1rem;
      border-radius: 1rem;
      border: 1px solid rgba(24, 33, 27, 0.14);
      background: #fff;
      color: var(--text);
      font: inherit;
    }

    .wheel-wrap {
      display: grid;
      gap: 0.45rem;
    }

    .wheel-note {
      margin: 0;
      color: var(--muted);
      font-size: 0.88rem;
      line-height: 1.6;
    }

    .secondary-btn {
      padding: 0.95rem 1.2rem;
      border-radius: 999px;
      background: var(--accent);
      color: #fff;
      font-weight: 700;
      box-shadow: 0 14px 28px rgba(29, 111, 90, 0.2);
    }

    .modal-actions {
      display: flex;
      flex-wrap: wrap;
      gap: 0.75rem;
      align-items: center;
    }

    @media (max-width: 720px) {
      .page {
        width: min(100vw - 1rem, 100%);
        padding-top: 1rem;
      }

      .shell,
      .panel,
      .modal-dialog {
        padding: 1rem;
        border-radius: 1.2rem;
      }

      .tabs,
      .choice-grid,
      .filter-row {
        width: 100%;
      }

      .tabs {
        display: grid;
        grid-template-columns: repeat(2, minmax(0, 1fr));
      }

      .tab,
      .filter,
      .primary-btn {
        width: 100%;
      }

      .choice-grid {
        grid-template-columns: 1fr;
      }
    }
  </style>
</head>
<body>
  <main class="page">
    <header class="shell">
      <div>
        <span class="eyebrow">Public Transfer</span>
        <h1>{{.AppName}}</h1>
        <p class="headline">上传页负责投递内容，下载页负责查看和下载所有公开内容。所有上传内容都会在 24 小时后自动清理。</p>
      </div>

      <div class="toolbar">
        <nav class="tabs" aria-label="页面切换">
          <a class="tab{{if eq .ActiveTab "upload"}} active{{end}}" href="/?tab=upload">上传</a>
          <a class="tab{{if eq .ActiveTab "download"}} active{{end}}" href="/?tab=download">下载</a>
        </nav>
        {{if .AuthEnabled}}
        <div class="session-tools">
          <span class="meta-copy">已登录: {{.CurrentUser}}</span>
          <form class="logout-form" method="post" action="/logout">
            <button type="submit" class="ghost-btn">退出</button>
          </form>
        </div>
        {{end}}
      </div>
    </header>

    {{if .Flash}}<div class="notice ok">{{.Flash}}</div>{{end}}
    {{if .Error}}<div class="notice err">{{.Error}}</div>{{end}}

    {{if eq .ActiveTab "upload"}}
    <section class="panel upload-stage">
      <div class="upload-card">
        <span class="mini-tag">Upload Entry</span>
        <h2>上传入口</h2>
        <p>点击下方按钮后，先选择上传纯文本还是文件，再在弹窗中完成上传。图片也通过文件入口上传。</p>
        <button type="button" class="primary-btn" data-open-upload>上传</button>
        <p class="helper">当前单次上传大小限制 {{.MaxUploadMB}} MB。文本会直接展示，图片会直接预览，普通文件会显示名称并提供下载。</p>
      </div>
    </section>

    <section class="info-grid">
      <article class="info-card">
        <strong>文本</strong>
        <p class="meta-copy">上传后直接在下载页显示完整内容，适合临时分享命令、日志和说明文字。</p>
      </article>
      <article class="info-card">
        <strong>图片</strong>
        <p class="meta-copy">图片文件会直接以预览卡片展示，访问者可查看原图或直接下载。</p>
      </article>
      <article class="info-card">
        <strong>文件</strong>
        <p class="meta-copy">任意文件都会保留文件名，以文件卡片方式公开展示，并在 24 小时后自动删除。</p>
      </article>
    </section>
    {{else}}
    <section class="panel download-head">
      <div>
        <span class="mini-tag">Download Board</span>
        <h2>已上传内容</h2>
        <p class="download-copy">所有访问者都可以查看和下载公开内容。使用下方标签筛选文本、图片或普通文件。</p>
      </div>

      <div class="filter-row">
        <a class="filter{{if eq .FilterKind ""}} active{{end}}" href="/?tab=download">全部</a>
        <a class="filter{{if eq .FilterKind "text"}} active{{end}}" href="/?tab=download&kind=text">文本</a>
        <a class="filter{{if eq .FilterKind "image"}} active{{end}}" href="/?tab=download&kind=image">图片</a>
        <a class="filter{{if eq .FilterKind "file"}} active{{end}}" href="/?tab=download&kind=file">文件</a>
      </div>
    </section>

    {{if .Entries}}
    <section class="grid">
      {{range .Entries}}
      <article class="entry">
        <div class="entry-meta">
          <div class="entry-label">Key</div>
          <div class="entry-key">{{.Key}}</div>
          <div class="entry-info">类型: {{.Kind}} | 文件名: {{.OriginalName}} | 大小: {{.Size}}</div>
          <div class="entry-info">上传: {{.CreatedAt}} | 到期: {{.ExpiresAt}} | 保留: {{.RetainHours}} 小时</div>
        </div>

        <div class="value">
          <div class="entry-label">Value</div>
          {{if eq .Kind "text"}}
          <pre class="text-content">{{.Text}}</pre>
          {{else if eq .Kind "image"}}
          <div class="image-box">
            <img src="{{.RawURL}}" alt="{{.OriginalName}}" loading="lazy">
          </div>
          {{else}}
          <div class="file-box">
            <div class="file-icon">FILE</div>
            <div class="file-name">{{.OriginalName}}</div>
            <div class="entry-info">{{.ContentType}}</div>
          </div>
          {{end}}

          <div class="actions">
            {{if eq .Kind "image"}}<a href="{{.RawURL}}" target="_blank" rel="noreferrer">查看原图</a>{{end}}
            <a href="{{.DownloadURL}}">下载</a>
          </div>
        </div>
      </article>
      {{end}}
    </section>
    {{else}}
    <div class="empty">当前筛选条件下还没有可下载内容。你可以切换标签查看全部内容，或先去上传页添加新的文本、图片或文件。</div>
    {{end}}
    {{end}}
  </main>

  <div class="modal{{if .OpenUpload}} open{{end}}" id="upload-modal" data-open="{{if .OpenUpload}}true{{else}}false{{end}}" data-mode="{{.UploadMode}}" aria-hidden="{{if .OpenUpload}}false{{else}}true{{end}}">
    <div class="modal-backdrop" data-close-modal></div>

    <div class="modal-dialog" role="dialog" aria-modal="true" aria-labelledby="upload-modal-title">
      <div class="modal-head">
        <div>
          <div class="mini-tag">Upload</div>
          <h3 id="upload-modal-title">选择并上传内容</h3>
        </div>
        <button type="button" class="ghost-btn" data-close-modal>关闭</button>
      </div>

      <section class="upload-pane" id="upload-picker">
        <p class="meta-copy">先选择上传形式，再填写内容并提交。</p>
        <div class="choice-grid">
          <button type="button" class="choice-btn" data-mode="text">
            <strong>纯文本</strong>
            <span class="meta-copy">直接粘贴文字内容，上传后会完整展示。</span>
          </button>
          <button type="button" class="choice-btn" data-mode="file">
            <strong>文件</strong>
            <span class="meta-copy">上传图片或任意文件，图片会预览，文件可下载。</span>
          </button>
        </div>
      </section>

      <section class="upload-pane" id="upload-text-pane">
        <div class="modal-actions">
          <button type="button" class="ghost-btn" data-back>返回</button>
          <div class="meta-copy">纯文本上传</div>
        </div>
        <form method="post" action="/upload" enctype="multipart/form-data">
          <input type="hidden" name="mode" value="text">
          <label>
            Key 名称
            <input type="text" name="key" placeholder="留空时自动生成文本名称">
          </label>
          <label class="wheel-wrap">
            保留时间
            <select name="retain_hours">
              {{range .HourOptions}}
              <option value="{{.}}"{{if eq $.RetainHours .}} selected{{end}}>{{.}} 小时</option>
              {{end}}
            </select>
            <span class="wheel-note">可保留 1 到 24 小时，当前上传上限 {{$.MaxUploadMB}} MB。</span>
          </label>
          <label>
            文本内容
            <textarea name="text" placeholder="把需要分享的文本粘贴到这里" required></textarea>
          </label>
          <button type="submit" class="secondary-btn">上传文本</button>
        </form>
      </section>

      <section class="upload-pane" id="upload-file-pane">
        <div class="modal-actions">
          <button type="button" class="ghost-btn" data-back>返回</button>
          <div class="meta-copy">文件上传</div>
        </div>
        <form method="post" action="/upload" enctype="multipart/form-data">
          <input type="hidden" name="mode" value="file">
          <label>
            Key 名称
            <input type="text" name="key" placeholder="留空时使用文件名">
          </label>
          <label class="wheel-wrap">
            保留时间
            <select name="retain_hours">
              {{range .HourOptions}}
              <option value="{{.}}"{{if eq $.RetainHours .}} selected{{end}}>{{.}} 小时</option>
              {{end}}
            </select>
            <span class="wheel-note">可保留 1 到 24 小时，当前文件上传限制 {{$.MaxUploadMB}} MB。</span>
          </label>
          <label>
            选择文件
            <input type="file" name="file" required>
          </label>
          <button type="submit" class="secondary-btn">上传文件</button>
        </form>
      </section>
    </div>
  </div>

  <script src="/app.js" defer></script>
</body>
</html>`))

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	store, err := NewStore(cfg.dataDir, contentTTL)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	srv := &server{
		store:          store,
		maxUploadBytes: cfg.maxUploadBytes,
		maxUploadMB:    cfg.maxUploadMB,
		configPath:     cfg.configPath,
		maxUploadFixed: cfg.maxUploadFixed,
		auth:           cfg.auth,
		sessions:       newSessionManager(sessionTTL),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.requireLogin(srv.handleIndex))
	mux.HandleFunc("GET /app.js", srv.requireLogin(handleAppJS))
	mux.HandleFunc("POST /upload", srv.requireLogin(srv.handleUpload))
	mux.HandleFunc("GET /raw/{id}", srv.requireLogin(srv.handleRaw))
	mux.HandleFunc("GET /download/{id}", srv.requireLogin(srv.handleDownload))
	mux.HandleFunc("GET /login", srv.handleLoginPage)
	mux.HandleFunc("POST /login", srv.handleLoginSubmit)
	mux.HandleFunc("POST /logout", srv.handleLogout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeSecurityHeaders(w)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go store.StartCleanupLoop(ctx.Done(), cleanupInterval)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("%s listening on http://127.0.0.1:%s", appName, cfg.port)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)
	s.syncUploadLimit()

	if err := s.store.CleanupExpired(); err != nil {
		http.Error(w, "cleanup failed", http.StatusInternalServerError)
		return
	}

	activeTab := normalizeTab(r.URL.Query().Get("tab"))
	filterKind := ""
	if activeTab == "download" {
		filterKind = normalizeKind(r.URL.Query().Get("kind"))
	}

	uploadMode := normalizeUploadMode(r.URL.Query().Get("mode"))
	errorText := strings.TrimSpace(r.URL.Query().Get("err"))
	flashText := strings.TrimSpace(r.URL.Query().Get("msg"))
	retainHours := normalizeRetentionHours(r.URL.Query().Get("retain"))
	maxUploadMB := s.currentMaxUploadMB()

	items := make([]pageEntry, 0)
	if activeTab == "download" {
		entries := s.store.List()
		items = make([]pageEntry, 0, len(entries))
		for _, entry := range entries {
			if filterKind != "" && entry.Kind != filterKind {
				continue
			}

			item := pageEntry{
				ID:           entry.ID,
				Key:          entry.Key,
				OriginalName: entry.OriginalName,
				Kind:         entry.Kind,
				ContentType:  entry.ContentType,
				Size:         humanSize(entry.Size),
				CreatedAt:    entry.CreatedAt.Local().Format("2006-01-02 15:04"),
				ExpiresAt:    entry.ExpiresAt.Local().Format("2006-01-02 15:04"),
				RawURL:       "/raw/" + entry.ID,
				DownloadURL:  "/download/" + entry.ID,
				RetainHours:  int(entry.ExpiresAt.Sub(entry.CreatedAt).Round(time.Hour) / time.Hour),
			}

			if entry.Kind == entryKindText {
				text, err := s.store.ReadText(entry.ID)
				if err != nil {
					item.Text = "[文本内容读取失败]"
				} else {
					item.Text = text
				}
			}

			items = append(items, item)
		}
	}

	data := pageData{
		AppName:     appName,
		Flash:       flashText,
		Error:       errorText,
		MaxUploadMB: maxUploadMB,
		ActiveTab:   activeTab,
		FilterKind:  filterKind,
		UploadMode:  uploadMode,
		OpenUpload:  activeTab == "upload" && (uploadMode != "" || errorText != ""),
		AuthEnabled: s.auth.Enabled,
		CurrentUser: s.auth.Username,
		RetainHours: retainHours,
		HourOptions: retentionOptions(),
		Entries:     items,
	}

	if err := pageTemplate.Execute(w, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)
	s.syncUploadLimit()

	if err := s.store.CleanupExpired(); err != nil {
		redirectWithMessage(w, r, "upload", "", defaultRetentionHours(), "", "清理过期文件失败")
		return
	}

	maxUploadBytes := s.currentMaxUploadBytes()
	maxUploadMB := s.currentMaxUploadMB()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(defaultMemStorage); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			redirectWithMessage(w, r, "upload", "", defaultRetentionHours(), "", fmt.Sprintf("上传内容超过 %d MB 限制", maxUploadMB))
			return
		}
		redirectWithMessage(w, r, "upload", "", defaultRetentionHours(), "", "无法解析上传内容")
		return
	}

	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	mode := normalizeUploadMode(r.FormValue("mode"))
	retainHours := normalizeRetentionHours(r.FormValue("retain_hours"))
	retentionTTL := time.Duration(retainHours) * time.Hour
	key := r.FormValue("key")
	text := r.FormValue("text")
	file, header, fileErr := r.FormFile("file")
	if fileErr == nil {
		defer file.Close()
	}

	hasText := strings.TrimSpace(text) != ""
	hasFile := fileErr == nil

	switch {
	case hasText && hasFile:
		redirectWithMessage(w, r, "upload", mode, retainHours, "", "文本和文件只能上传一种")
		return
	case !hasText && !hasFile:
		redirectWithMessage(w, r, "upload", mode, retainHours, "", "请填写文本或选择文件")
		return
	case !hasFile && fileErr != nil && !errors.Is(fileErr, http.ErrMissingFile):
		redirectWithMessage(w, r, "upload", mode, retainHours, "", "读取上传文件失败")
		return
	}

	var (
		entry Entry
		err   error
	)

	if hasText {
		entry, err = s.store.AddText(key, text, retentionTTL)
	} else {
		entry, err = s.store.AddFile(key, header, file, retentionTTL)
	}
	if err != nil {
		redirectWithMessage(w, r, "upload", mode, retainHours, "", "上传失败: "+err.Error())
		return
	}

	redirectWithMessage(w, r, "download", "", defaultRetentionHours(), "已上传: "+entry.Key, "")
}

func (s *server) handleRaw(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)

	if err := s.store.CleanupExpired(); err != nil {
		http.Error(w, "cleanup failed", http.StatusInternalServerError)
		return
	}

	entry, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	file, err := s.store.Open(entry.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	contentType := "application/octet-stream"
	switch entry.Kind {
	case entryKindImage:
		contentType = entry.ContentType
	case entryKindText:
		contentType = "text/plain; charset=utf-8"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", contentDisposition("inline", entry.OriginalName))
	http.ServeContent(w, r, entry.OriginalName, entry.CreatedAt, file)
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)

	if err := s.store.CleanupExpired(); err != nil {
		http.Error(w, "cleanup failed", http.StatusInternalServerError)
		return
	}

	entry, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	file, err := s.store.Open(entry.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	contentType := entry.ContentType
	if entry.Kind == entryKindText {
		contentType = "text/plain; charset=utf-8"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", entry.OriginalName))
	http.ServeContent(w, r, entry.OriginalName, entry.CreatedAt, file)
}

func redirectWithMessage(w http.ResponseWriter, r *http.Request, tab, mode string, retainHours int, msg, errText string) {
	target := "/"
	query := url.Values{}
	if tab = normalizeTab(tab); tab != "" {
		query.Set("tab", tab)
	}
	if mode = normalizeUploadMode(mode); mode != "" {
		query.Set("mode", mode)
	}
	query.Set("retain", strconv.Itoa(normalizeRetentionHours(strconv.Itoa(retainHours))))
	if msg != "" {
		query.Set("msg", msg)
	}
	if errText != "" {
		query.Set("err", errText)
	}
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func normalizeTab(tab string) string {
	switch strings.ToLower(strings.TrimSpace(tab)) {
	case "download":
		return "download"
	default:
		return "upload"
	}
}

func normalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case entryKindText, entryKindImage, entryKindFile:
		return strings.ToLower(strings.TrimSpace(kind))
	default:
		return ""
	}
}

func normalizeUploadMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "text", "file":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return ""
	}
}

func normalizeRetentionHours(value string) int {
	hours, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return defaultRetentionHours()
	}
	if hours < minRetentionHours {
		return minRetentionHours
	}
	if hours > maxRetentionHours {
		return maxRetentionHours
	}
	return hours
}

func defaultRetentionHours() int {
	return maxRetentionHours
}

func retentionOptions() []int {
	options := make([]int, 0, maxRetentionHours-minRetentionHours+1)
	for hour := minRetentionHours; hour <= maxRetentionHours; hour++ {
		options = append(options, hour)
	}
	return options
}

func (s *server) currentMaxUploadMB() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxUploadMB
}

func (s *server) currentMaxUploadBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxUploadBytes
}

func (s *server) syncUploadLimit() {
	if s.maxUploadFixed || s.configPath == "" {
		return
	}

	cfg, err := readConfigFile(s.configPath)
	if err != nil {
		return
	}

	maxUploadMB := cfg.MaxUploadMB
	if maxUploadMB <= 0 {
		maxUploadMB = defaultMaxUpload
	}

	s.mu.Lock()
	s.maxUploadMB = maxUploadMB
	s.maxUploadBytes = maxUploadMB * 1024 * 1024
	s.mu.Unlock()
}

func contentDisposition(mode, filename string) string {
	filename = sanitizeFilename(filename)
	if filename == "" {
		filename = "download.bin"
	}

	asciiFallback := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)

	return fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s", mode, asciiFallback, url.PathEscape(filename))
}

func writeSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func handleAppJS(w http.ResponseWriter, _ *http.Request) {
	writeSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write([]byte(appScript))
}

func humanSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, recorder.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
