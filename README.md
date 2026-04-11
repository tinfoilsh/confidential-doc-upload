# confidential-doc-upload

Tinfoil confidential document processing service. Extracts text and renders page images from uploaded documents (PDF, DOCX, PPTX, HTML, XLSX, CSV, images) inside a Trusted Execution Environment.

No GPU required. Text extraction uses [PyMuPDF](https://pymupdf.readthedocs.io/) (C-native, 48x faster than pdfplumber). Visual element extraction and OCR use a remote VLM via the [Tinfoil SDK](https://github.com/tinfoilsh/tinfoil-go) with attestation verification.

## Architecture

```
Client → Tinfoil Shim (TLS + auth) → Go Router (:5000) → Python Sidecar (:5002)
                                           ↓
                                      Tinfoil SDK → VLM (gemma4-31b)
```

Single container runs both processes:
- **Go Router** — HTTP API, mode selection, parallel VLM dispatch
- **Python Sidecar** — PyMuPDF text extraction + page rendering, format-specific parsers

## API

### `POST /v1/convert/file?mode=text|images|raw|vlm`

Upload one or more files as `multipart/form-data` with field name `files`.

#### Modes

| Mode | Description | Speed (18p PDF) |
|------|-------------|-----------------|
| **`text`** (default) | Full extraction: PyMuPDF text + VLM visual descriptions for born-digital pages, VLM OCR for scanned pages | ~13s |
| **`images`** | Text where available + page images as base64 PNG. No VLM calls. For vision-capable downstream models. | **0.8s** |
| **`raw`** | Text layer only. No VLM, no rendering. Fastest possible. | **0.1s** |
| **`vlm`** | VLM full-page OCR on every page. Highest quality. | ~30s+ |

#### Response

```json
{
  "document": {
    "md_content": "# Title\n\nExtracted text...",
    "pages": [
      {"page": 1, "image": "base64...", "is_scanned": false},
      {"page": 2, "image": "base64...", "is_scanned": true}
    ]
  },
  "status": "success",
  "processing_time": 1.23
}
```

- `md_content` — always present. Full text in `text`/`vlm` modes, text-layer only in `images`/`raw`.
- `pages[]` — only in `images` mode. Each page has `image` (base64 PNG), `is_scanned` flag.
- Multi-file uploads return `documents[]` array instead of `document`.

#### Examples

```bash
# Default (text mode) — full extraction with VLM
curl -X POST https://doc-upload.example.com/v1/convert/file \
    -H "Authorization: Bearer $API_KEY" \
    -F "files=@document.pdf"

# Images mode — fast, for vision models
curl -X POST https://doc-upload.example.com/v1/convert/file?mode=images \
    -H "Authorization: Bearer $API_KEY" \
    -F "files=@document.pdf"

# Raw mode — text layer only, instant
curl -X POST https://doc-upload.example.com/v1/convert/file?mode=raw \
    -H "Authorization: Bearer $API_KEY" \
    -F "files=@document.pdf"
```

### `GET /health`

Returns service health status.

```json
{"status": "ok", "router": true, "sidecar": true, "vlm": true}
```

### `GET /metrics`

Prometheus metrics: `router_requests_total`, `router_duration_seconds`, `router_active_requests`, `router_errors_total`.

## Supported formats

| Format | Extension | Extraction method |
|--------|-----------|-------------------|
| PDF (born-digital) | `.pdf` | PyMuPDF text extraction |
| PDF (scanned) | `.pdf` | Page rendering → VLM OCR |
| Word | `.docx` | python-docx (headings, tables, lists, formatting) |
| PowerPoint | `.pptx` | python-pptx (slides, tables, notes) |
| HTML | `.html`, `.htm` | BeautifulSoup (headings, tables, lists, code) |
| Excel | `.xlsx`, `.xls` | openpyxl (sheets as markdown tables) |
| CSV | `.csv` | Auto-dialect detection, markdown table |
| Markdown | `.md` | Passthrough |
| Images | `.png`, `.jpg`, etc. | VLM OCR or passthrough |
| Text | `.txt`, `.json`, `.xml`, `.yaml` | Passthrough |

## Configuration

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TINFOIL_API_KEY` | (required, secret) | API key for Tinfoil VLM calls |
| `VLM_MODEL` | `gemma4-31b` | Model name for VLM calls via Tinfoil SDK |
| `SIDECAR_URL` | `http://localhost:5002` | Python sidecar URL (internal) |
| `ROUTER_PORT` | `5000` | Go router listen port |
| `MAX_FILE_SIZE_MB` | `50` | Max file size per upload |
| `MAX_FILES` | `10` | Max files per request |
| `MAX_PARALLEL` | `32` | Max concurrent VLM calls |

### Limits

- Max 200 pages per PDF
- Max 50MB per file
- Max 10 files per request
- Sidecar response capped at 512MB

## Security

- **Zero persistent state** — all processing is in-memory, request-scoped. No disk writes, no caches, no database.
- **No cross-request leakage** — file data lives only in Go/Python memory until GC. No filenames in API responses.
- **Attested VLM calls** — Tinfoil SDK verifies remote enclave identity before sending document images.
- **Sanitized errors** — generic error messages to clients, details logged server-side only.
- **Request timeouts** — ReadTimeout 5min, IdleTimeout 2min. No WriteTimeout (VLM calls can be long).
- **Sidecar isolated** — binds to 127.0.0.1, not externally accessible.

## Local development

```bash
# Build
docker build -t doc-upload .

# Run
docker run -d --name doc-upload --network host \
    -e TINFOIL_API_KEY=your-key \
    -e VLM_MODEL=gemma4-31b \
    doc-upload

# Test
curl -F "files=@test.pdf" http://localhost:5000/v1/convert/file?mode=raw
```

## Deployment

Runs as a single container on a Tinfoil CVM. No GPU required. See `tinfoil-config.yml` for the production configuration.

```yaml
cvm-version: 0.7.5
cpus: 4
memory: 8192

containers:
  - name: "doc-upload"
    image: "ghcr.io/tinfoilsh/confidential-doc-upload@sha256:..."
    env:
      - VLM_MODEL: "gemma4-31b"
    secrets:
      - TINFOIL_API_KEY
```
