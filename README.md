# Netlify Go Serverless HLS & MP4 Streaming Proxy

A high-performance serverless playback proxy written in **Go 1.22+**, designed for **Netlify Functions** and AWS Lambda.

It features:
- **No ASN Mismatches**: Runs on Netlify / AWS Lambda IP space outside Cloudflare's ASN `13335`, allowing Streamtape and protected hosts to stream with **`HTTP 206 Partial Content`**.
- **In-Serverless Extraction**: Automatically resolves Streamtape (`st`) and StreamHG (`sh`) streams directly inside the function.
- **Byte-Range Streaming**: Full HTTP `Range` forwarding (`bytes=0-100`) for seamless seeking and low-latency playback.
- **HLS Playlist Rewriting**: Rewrites master playlists and variant streams, preserving multi-audio tracks and subtitles.
- **Stateless HMAC-SHA256 Sessions**: Sessions resolve across all distributed Netlify serverless instances without shared databases.
- **Zero Dependencies**: Pure Go standard library (`net/http`, `crypto/hmac`, `crypto/sha256`).

---

## Directory Structure

```
netlify-proxy-go/
├── netlify.toml                        # Netlify configuration & function routing
├── go.mod                              # Go module manifest (zero dependencies)
├── main.go                             # Standalone local dev server
├── netlify/
│   └── functions/
│       └── proxy/
│           └── main.go                 # Netlify Go serverless function entrypoint
├── pkg/
│   └── proxy/
│       ├── security.go                 # SSRF protection, CORS headers, token auth
│       ├── session.go                  # Stateless HMAC-SHA256 session manager
│       ├── rewriter.go                 # HLS playlist (.m3u8) rewriter
│       ├── extractors.go               # Streamtape & StreamHG same-node extractors
│       ├── handler.go                  # HTTP router, Range handler, streaming proxy
│       ├── lambda.go                   # Standard AWS Lambda / Netlify runtime adapter
│       └── proxy_test.go               # Automated test suite (Range, 206, Streamtape)
└── README.md                           # Deployment guide & API documentation
```

---

## 1. Local Testing

Run all unit and integration tests:
```bash
cd netlify-proxy-go
go test -v -count=1 ./...
```

Start the standalone local test server:
```bash
cd netlify-proxy-go
go run main.go
# Server starts on http://localhost:8888
```

### Test Endpoints Locally

* **Health Check**:
  ```bash
  curl -s http://localhost:8888/health
  ```

* **Extract Streamtape (e.g. Nilakanta 1107009 / `0A2vDYQz3wIbPQ6`)**:
  ```bash
  curl -s http://localhost:8888/extract/st/0A2vDYQz3wIbPQ6
  ```

* **Test HTTP 206 Partial Content Range Streaming**:
  ```bash
  curl -i -s -H "Range: bytes=0-100" "<playback_url_from_previous_command>"
  ```
  Returns `HTTP/1.1 206 Partial Content` with `Content-Range: bytes 0-100/...`!

---

## 2. Deploying to Netlify

### Option A: Using Netlify CLI

1. Install Netlify CLI (if needed):
   ```bash
   npm install -g netlify-cli
   ```

2. From within the `netlify-proxy-go` directory:
   ```bash
   cd netlify-proxy-go
   netlify login
   netlify init
   netlify deploy --prod
   ```

### Option B: Deploying via Netlify Git Integration

1. Push your repository to GitHub / GitLab.
2. In the Netlify Dashboard, click **Add new site** &rarr; **Import an existing project**.
3. Set **Base directory** to:
   ```
   netlify-proxy-go
   ```
4. Build command: *(leave empty or `go build -o .netlify/functions/proxy netlify/functions/proxy/main.go`)*
5. Publish directory: *(leave empty)*
6. Click **Deploy**. Netlify automatically detects Go and compiles the binary.

---

## 3. Environment Variables (Netlify Site Settings)

In the Netlify Dashboard (**Site configuration** &rarr; **Environment variables**):

| Variable | Description | Default |
| :--- | :--- | :--- |
| `PROXY_SECRET` | Secret key required in `Authorization: Bearer <key>` or `X-Proxy-Key` | *(Empty = Open access)* |
| `SESSION_TTL_SECONDS` | Session duration in seconds | `14400` (4 hours) |
| `ALLOWED_UPSTREAM_DOMAINS` | Comma-separated list of allowed domains | *(Empty = all public HTTPS)* |

---

## 4. Connecting to the Go Backend

Once deployed to Netlify (e.g. `https://my-go-proxy.netlify.app`), connect it to your Go backend:

### In `.env`:
```env
CF_WORKERS_URL=https://my-go-proxy.netlify.app
```

The Go backend communicates with Netlify Go Serverless Proxy using the exact same API contract as the Cloudflare Worker, delivering native binary performance and **`HTTP 206 Partial Content`** streaming.
