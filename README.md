# confidential-doc-upload

Tinfoil confidential document conversion service. Converts PDFs to markdown using
[docling-serve](https://github.com/docling-project/docling-serve) with a smart
routing layer that optimizes for speed and quality based on document type.

## Architecture

```
Client → Tinfoil Shim (TLS + auth) → Router (:5000) → Docling-serve (:5001)
```

### Router (`router/`)

Lightweight Python/FastAPI service that:
1. **Classifies PDFs** — born-digital vs scanned (< 5ms, text extraction heuristic)
2. **Chooses optimal pipeline** — skips OCR for born-digital, uses EasyOCR for scans
3. **Splits large documents** — sends chunks to different docling workers in parallel
4. **Merges results** — concatenates markdown from chunks

### Docling-serve

Upstream [docling-serve](https://github.com/docling-project/docling-serve) v1.15.0
with optimized settings:
- `UVICORN_WORKERS=8` — 8 parallel worker processes for true GPU parallelism
- `LAYOUT_BATCH_SIZE=16` — allows GPU to batch work across workers efficiently
- `ENG_LOC_NUM_WORKERS=1` — one docling worker per uvicorn process

## Performance

On a 19-page academic PDF (NVIDIA H200):

| Config | Latency | Improvement |
|--------|---------|-------------|
| Original (v1.13.1, defaults) | 22s | baseline |
| Optimized (v1.15.0, tuned) | 14s | 1.6x |
| + Router (skip OCR for born-digital) | 14s | 1.6x |
| + Split processing (7 chunks × 3pp) | **4s** | **5.5x** |

Concurrent throughput: 8 requests in ~24s with batch_size=16.

## Configuration

### Router environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCLING_URL` | `http://localhost:5001` | Docling-serve URL |
| `ROUTER_PORT` | `5000` | Router listen port |
| `CHUNK_PAGES` | `3` | Pages per chunk when splitting |
| `SPLIT_THRESHOLD` | `6` | Min pages before splitting kicks in |
| `LOG_LEVEL` | `INFO` | Log level |

### Docling-serve environment variables

See `tinfoil-config.yml` for the full list. Key settings:
- `UVICORN_WORKERS=8` — parallel worker processes
- `DOCLING_SERVE_LAYOUT_BATCH_SIZE=16` — GPU batch size
- `DOCLING_SERVE_MAX_FILE_SIZE=52428800` — 50MB limit
- `DOCLING_SERVE_MAX_NUM_PAGES=200` — page limit

## Local development

```bash
# Build router image
docker build -t doc-upload-router ./router

# Run the stack
docker run -d --name doc-upload --network host --gpus all --runtime nvidia \
    -e UVICORN_WORKERS=8 \
    -e DOCLING_SERVE_LAYOUT_BATCH_SIZE=16 \
    -e DOCLING_SERVE_TABLE_BATCH_SIZE=16 \
    -e DOCLING_SERVE_OCR_BATCH_SIZE=16 \
    -e DOCLING_SERVE_ENG_LOC_NUM_WORKERS=1 \
    ghcr.io/docling-project/docling-serve-cu130:v1.15.0

docker run -d --name doc-router --network host \
    -e DOCLING_URL=http://localhost:5001 \
    doc-upload-router

# Test
curl -X POST http://localhost:5000/v1/convert/file \
    -F "files=@document.pdf" \
    -F 'options={"to_formats":["md"]}'
```
