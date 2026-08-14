# confidential-doc-upload

Tinfoil confidential document processing service. Extracts structured markdown from uploaded documents (PDF, DOCX, PPTX, HTML, XLSX, CSV, images) inside a [Tinfoil Container](https://tinfoil.sh).

## Architecture

```
                  Tinfoil confidential VM

  ┌────────────────────────────┐        VLM/OCR
  │ Router container (:5000)   ├────────────────────►
  │ HTTP API + Tinfoil API key │
  │ network; parser code denied│
  └─────────────┬──────────────┘
                │ private Unix socket volume
  ┌─────────────▼─────────────────────────────────────┐
  │ Parser container                                 │
  │ no network attachment · no secrets · separate PID│
  │ namespace · non-root · read-only root filesystem │
  │                                                   │
  │ parserd ──► sandbox-exec ──► fresh parser/pass    │
  │             limits + Landlock + seccomp           │
  │             selected MuPDF/Python process exits   │
  └───────────────────────────────────────────────────┘
```

Born-digital PDFs never touch the VLM. Scanned pages are rendered to images and sent to the VLM for OCR.

## Security

All document formats are attacker-controlled input. MuPDF and the Python format
libraries therefore run only in short-lived subprocesses inside a dedicated
parser container. Unix file permissions prevent the secret-bearing router UID
from reading or executing parser code even though both roles share one image.


| Layer | Enforcement |
| --- | --- |
| **Secret boundary** | Only the router container receives `TINFOIL_API_KEY`. Router and parser use different UIDs and separate PID namespaces; the parser has no secret environment. Parser executables/source are owner-only to the parser UID. Brokers and every request parser are non-dumpable. |
| **No parser network** | The parser container has no network attachment. The child seccomp policy also rejects socket, socketpair, io_uring, and every network syscall. |
| **Allowlisted router egress** | The secret-bearing router may connect only to `atc.tinfoil.sh` for client-verified attestation bundles and `inference.tinfoil.sh` for end-to-end encrypted VLM requests. cvmimage enforces the resolved-IP allowlist outside the container. |
| **Fresh document children** | Every parse pass receives exactly one document over stdin in a fresh process and exits afterward. Admission is capped at two parser processes; multi-file requests also use at most two workers. The socket volume holds a non-blocking single-broker lock. |
| **Filesystem isolation** | Every child enters a new, fail-closed Landlock domain. A PDF pass can open only its binary and loader. A Python pass can additionally open only its interpreter, immutable libraries, and fixed parser source. The other parser, `/proc`, `/run`, `/tmp`, `/sys`, `/etc`, and the socket volume are invisible. Executable access is separately allowlisted. |
| **No surviving children** | `fork`, `vfork`, `clone3`, and non-thread `clone` are rejected. The parser runs in its own process group, uses a parent-death signal, and the whole group is killed on cancellation or limit violation. |
| **Empty environment** | `sandbox-exec` replaces itself with the parser using an empty environment. No request parser inherits broker configuration. |
| **Syscall restrictions** | Seccomp rejects cross-process inspection/signalling and resource-limit changes, namespaces, mounts, writable opens, filesystem mutation, kernel keyrings, BPF, perf, userfaultfd, io_uring, modules, and IPC. Filter installation is synchronized across threads and fails closed. |
| **Hard resource limits** | Every parser gets kernel limits for address space (1536 MiB), CPU time (120s), open files (64), output (256 MiB), stderr (1 MiB), and zero-sized files/core dumps/locked memory/message queues. The parser container also has memory, CPU, and PID cgroup caps. |
| **Authenticated private API** | The router gets connect-only group access to the parser socket. The broker additionally authenticates every connection with kernel `SO_PEERCRED` and accepts only the fixed router/parser UIDs. |
| **Immutable runtime** | The router runs as 65533:65532 and the parser as 65532:65532, with all capabilities dropped, `no-new-privileges`, no IPC namespace sharing, read-only root filesystems, and no parser tmpfs. |
| **Bounded admission** | Upload size, file count, multipart parts, active requests, total document workers, per-request workers, VLM fan-out, parser response size, and parser wall time are bounded. Parser slots are acquired before request bodies are buffered. |
| **Pinned inputs** | Base images use digests, MuPDF uses a pinned version and verified SHA-256, Python packages are exact-version/hash locked with binary-only installs, and Go modules are verified before reproducible, path-trimmed builds. Build-only `pip` is removed from the runtime image. |

The per-PDF invariant is precise: initial extraction uses one fresh `pdfparser`
process. Modes that need page images (`images`, `vision`, `vlm`, and OCR of a
scanned PDF) use one additional fresh render process for that PDF. No parser
process handles two documents or two passes. At most two untrusted parsers may
overlap, and each has its own Landlock domain, inherited pipes, limits, and
process group. Non-PDF extraction and image rendering follow the same model.

VLM calls remain in the router and use the [Tinfoil SDK](https://github.com/tinfoilsh/tinfoil-go)
with remote attestation. The seccomp policy is defense in depth around the
stronger container/PID/network/secret boundary; it is deliberately not
described as a complete defense against a hostile kernel.

### Concurrency and efficiency

Parser work is not globally sequential. `parserd` admits two passes by
default, and a multi-file request can schedule two documents at once. Each
admitted pass still creates a new process, enters a new Landlock domain, reads
one document from stdin, emits one bounded response, and exits. There is no
parser process pool and no process reuse across documents. A global router gate
preserves the configured aggregate work ceiling across concurrent requests.

The two-worker limit is intentional: it recovers parallel throughput without
allowing attacker-controlled parsing to expand to the VM's full CPU and memory.
On the 2026-08-13/14 box3 validation snapshot, 100 tiny raw-PDF requests at
concurrency four took a median 0.485s with the serialized baseline and 0.282s
with this design. Twenty 15-page PDFs improved from 4.5 to 8.4 documents per
second. An eight-file request fell from 0.05s to 0.02s. The exact cvmimage
v0.11.0 debug guest measured 0.28–0.29s steady-state and 0.02s respectively.
These are parser-overhead microbenchmarks, not latency claims for large PDFs or
VLM modes.

### Threat-model limits

- Concurrent children share the parser container's guest kernel, CPU caches,
  memory bandwidth, and cgroup. Landlock and seccomp block direct file/process
  access, but do not eliminate timing, cache, or resource-contention side
  channels. Set `PARSER_WORKERS=1` when temporal non-overlap is more important
  than throughput.
- A guest-kernel or container-runtime escape is outside the per-document
  sandbox guarantee. The confidential VM protects the guest from the host;
  short-lived processes, namespaces, Landlock, seccomp, and cgroups protect
  workloads inside that guest.
- Resource exhaustion is bounded, not impossible. An adversarial document can
  consume its full CPU, memory, output, and wall-time allowances before the
  process group is killed.
- A router compromise is more severe than a parser compromise: the router
  necessarily sees plaintext documents, has network access, and owns the VLM
  credential. Egress is restricted to the attestation and encrypted-inference
  origins, but v0.11.0 does not provide a domain-filtering DNS proxy, so DNS is
  not treated as a complete exfiltration boundary. The parser boundary is
  designed to prevent a parser-library RCE from reaching those assets.
- Scanned pages and explicitly requested VLM modes leave this enclave for the
  remotely attested VLM service. Born-digital `raw` and `images` paths do not
  make VLM calls.

### Validation gate

The box3 snapshot was exercised with the full Go test suite, race detector,
`go vet`, `govulncheck`, a Python dependency audit, and an all-severity Trivy
image scan. No reachable Go vulnerability, Python-package vulnerability, or
Alpine OS vulnerability was found. Trivy reports one module-level metadata
warning for the deprecated `x/crypto/openpgp` package in the SDK's transitive
module graph; `govulncheck` confirms that package has no reachable call path.
MuPDF is statically linked and therefore invisible to ordinary image scanners;
the separately pinned and checksum-verified build input is MuPDF 1.28.2, which
includes the upstream 1.28 security fixes and subsequent stability fixes.
Live negative tests verified denied `/proc`, temporary-file, socket, shell, and
cross-parser access, plus kernel-authenticated Unix-socket peers. Under load the
v0.11.0 guest showed exactly two simultaneous parser processes and zero after
the requests completed.

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
{"status": "ok", "router": true, "parser": true, "vlm": true}
```

The endpoint returns `503` only when the required parser is unavailable. An
unavailable optional VLM is reported as `"status":"degraded"` with `200`, so
`raw`, `images`, and other non-VLM paths remain ready.

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


| Variable | Default | Description |
| --- | --- | --- |
| `TINFOIL_API_KEY` | required for VLM modes | Router-only Tinfoil VLM key |
| `TINFOIL_PROXY_URL` | `https://inference.tinfoil.sh/v1/` | Fixed encrypted-inference proxy; keep aligned with the cvm egress allowlist |
| `TINFOIL_ATTESTATION_URL` | `https://atc.tinfoil.sh` | Fixed client-verified attestation-bundle origin; keep aligned with the cvm egress allowlist |
| `VLM_MODEL` | `gemma4-31b` | VLM model name |
| `ROUTER_PORT` | `5000` | Router listen port |
| `PARSER_SOCKET` | `/run/docparser/parser.sock` | Private router/parser Unix socket |
| `MAX_FILE_SIZE_MB` | `50` | Per-file limit, bounded to 1–64 MiB |
| `MAX_FILES` | `10` | Files per request, bounded to 1–10 |
| `MAX_PARTS` | `64` | Multipart parts, bounded to 1–128 |
| `MAX_ACTIVE_REQUESTS` | `4` | Admitted requests, bounded to 1–32 |
| `MAX_PARALLEL` | `8` | VLM calls per request, bounded to 1–32 |
| `PARSER_TIMEOUT_SECONDS` | `120` | Parser wall/CPU limit, bounded to 1–300 seconds |
| `PARSER_MEMORY_LIMIT_MB` | `1536` | Parser address-space limit, bounded to 512–4096 MiB |
| `PARSER_MAX_OUTPUT_MB` | `256` | Child and broker response limit, bounded to 1–512 MiB |
| `PARSER_WORKERS` | `2` | Parser processes, bounded to 1–2; set to 1 for strict serialization |


## Development

```bash
docker build -t doc-upload .

docker volume create doc-parser-socket

docker run -d --name doc-parser \
    --network none --user 65532:65532 --read-only \
    --cap-drop ALL --security-opt no-new-privileges --ipc none \
    --memory 4g --cpus 4 --pids-limit 128 \
    -e PARSER_WORKERS=2 \
    -v doc-parser-socket:/run/docparser \
    doc-upload /usr/local/bin/parserd

docker run -d --name doc-upload -p 5000:5000 \
    --user 65533:65532 --read-only \
    --cap-drop ALL --security-opt no-new-privileges --ipc none \
    --memory 6g --pids-limit 256 \
    -v doc-parser-socket:/run/docparser \
    -e TINFOIL_API_KEY=your-key doc-upload

curl -F "files=@test.pdf" http://localhost:5000/v1/convert/file?mode=raw
```

Production applies the equivalent topology and limits from
`tinfoil-config.yml`. Neither container requires `--privileged` or additional
Linux capabilities.
