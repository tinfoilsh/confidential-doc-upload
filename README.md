# confidential-doc-upload

Tinfoil confidential document processing service. Extracts structured markdown from uploaded documents (PDF, DOCX, PPTX, HTML, XLSX, CSV, images) inside a [Tinfoil Container](https://tinfoil.sh).

## Architecture

```
                    ┌──────────────────────────────────┐
                    │        Go Router (:5000)         │
                    │      HTTP API · VLM dispatch     │
                    └──────┬──────────────┬────────────┘
                           │              │
              born-digital │              │ scanned pages
                    ┌──────▼──────┐  ┌────▼────────────┐
                    │  pdfparser  │  │  Tinfoil SDK    │
                    │  ┌────────┐ │  │  → VLM (OCR)    │
                    │  │ MuPDF  │ │  └─────────────────┘
                    │  │ 1.27.2 │ │
                    │  └────────┘ │
                    │  SANDBOXED  │
                    │  no env     │
                    │  no network │
                    │  dies after │
                    │  each req   │
                    └─────────────┘
```

Born-digital PDFs never touch the VLM. Scanned pages are rendered to images and sent to the VLM for OCR.

## Security

PDF parsing runs untrusted C code (MuPDF) on user-uploaded files. Each PDF is parsed in a **sandboxed subprocess** that dies after every request:


| Layer                   | What it does                                                                                    |
| ----------------------- | ----------------------------------------------------------------------------------------------- |
| **Process-per-request** | Parser subprocess is forked, runs, exits. OS reclaims all memory. Zero cross-user data leakage. |
| **Empty environment**   | No env vars — no API keys, no secrets, nothing to steal.                                        |
| **No network**          | `CLONE_NEWNET` — empty network stack. Cannot reach the VLM or the internet.                     |
| **Memory limit**        | 512MB. Decompression bombs are killed.                                                          |
| **Timeout**             | 120s hard kill. Malicious PDFs cannot hang the service.                                         |


The parser reads PDF bytes from stdin, writes JSON to stdout, and exits. It cannot open files, make connections, or access secrets. VLM calls use the [Tinfoil SDK](https://github.com/tinfoilsh/tinfoil-go) with remote attestation. MuPDF is built from source with a pinned SHA256 checksum.

## API

### `POST /v1/convert/file?mode=text|vision|images|raw|vlm`

Upload files as `multipart/form-data` with field name `files`.

#### Modes


| Mode | Description | Speed (18p PDF) |
|------|-------------|-----------------|
| `text` (default) | Markdown from text layer. VLM OCR only for scanned pages. | ~1s |
| `vision` | Text + VLM visual descriptions for figures/charts on born-digital pages. | ~13s |
| `images` | Text + page images as base64 PNG. No VLM. | ~1s |
| `raw` | Text layer only. No VLM, no rendering. | ~1s |
| `vlm` | VLM full-page OCR on every page. | ~30s+ |


#### Response

```json
{
  "document": {
    "md_content": "# Title\n\nExtracted text...",
    "pages": [{"page": 1, "image": "base64...", "is_scanned": false}]
  },
  "status": "success",
  "processing_time": 1.23
}
```

#### Examples

```bash
# Default — fast, no VLM for born-digital PDFs
curl -X POST https://doc-upload.example.com/v1/convert/file \
    -H "Authorization: Bearer $API_KEY" \
    -F "files=@document.pdf"

# Vision — includes VLM descriptions of charts and figures
curl -X POST https://doc-upload.example.com/v1/convert/file?mode=vision \
    -H "Authorization: Bearer $API_KEY" \
    -F "files=@document.pdf"

# Raw — text layer only
curl -X POST https://doc-upload.example.com/v1/convert/file?mode=raw \
    -H "Authorization: Bearer $API_KEY" \
    -F "files=@document.pdf"
```

### `GET /health`

```json
{"status": "ok", "router": true, "sidecar": true, "vlm": true}
```

### `GET /metrics`

Prometheus metrics: `router_requests_total`, `router_duration_seconds`, `router_active_requests`, `router_errors_total`.

## PDF extraction

The PDF parser produces markdown with:

- **Headers** from font size analysis
- **Bold / italic / monospace / strikeout** via MuPDF per-character style flags
- **Tables** from drawn borders (vector path intersection)
- **Code blocks** from monospace font runs
- **Bullet lists** with indentation
- **Multi-column layout** with correct reading order
- **Scanned page detection** (pages with <50 characters)

## Supported formats


| Format                      | Extraction       |
| --------------------------- | ---------------- |
| PDF (born-digital)          | MuPDF → markdown |
| PDF (scanned)               | VLM OCR          |
| DOCX, PPTX, HTML, XLSX, CSV | Python parsers   |
| Images                      | VLM OCR          |
| Markdown, text, JSON, XML   | Passthrough      |


## Configuration


| Variable           | Default      | Description              |
| ------------------ | ------------ | ------------------------ |
| `TINFOIL_API_KEY`  | (required)   | API key for Tinfoil VLM  |
| `VLM_MODEL`        | `gemma4-31b` | VLM model name           |
| `ROUTER_PORT`      | `5000`       | Listen port              |
| `MAX_FILE_SIZE_MB` | `50`         | Max file size            |
| `MAX_FILES`        | `10`         | Max files per request    |
| `MAX_PARALLEL`     | `32`         | Max concurrent VLM calls |


## Development

```bash
docker build -t doc-upload .
docker run -d --name doc-upload --network host --privileged \
    -e TINFOIL_API_KEY=your-key doc-upload
curl -F "files=@test.pdf" http://localhost:5000/v1/convert/file?mode=raw
```

`--privileged` is required for `CLONE_NEWNET` sandbox isolation. In production this runs inside a Tinfoil CVM.