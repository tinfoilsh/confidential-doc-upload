# Confidential Document Processing

Tinfoil's document processing service converts uploaded files (PDF, DOCX, PPTX, HTML, XLSX, CSV, images) into markdown inside a secure enclave, so documents can be used as model input without leaving the confidential boundary.

## How it works

1. The router receives a multipart upload on `POST /v1/convert/file`
2. Each document is parsed in a fresh, sandboxed process inside a separate parser container with no network and no secrets
3. Born-digital PDFs are converted directly from their text layer; scanned pages and images are sent to an attested vision model for OCR
4. The router returns markdown, optionally with page images or visual descriptions depending on `mode`

Client-facing behavior (modes, response shape, supported formats, limits, and how to pair it with file inputs in chat) is documented at [docs.tinfoil.sh/guides/document-processing](https://docs.tinfoil.sh/guides/document-processing).

The parser sandbox, threat model, and validation results are described in [SECURITY.md](SECURITY.md).

## Running locally

```bash
docker build -t doc-upload .

docker volume create doc-parser-socket

docker run -d --name doc-parser \
    --network none --user 65532:65532 --read-only \
    --cap-drop ALL --security-opt no-new-privileges --ipc none \
    --memory 4g --cpus 4 --pids-limit 128 \
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

Production applies the equivalent topology and limits from `tinfoil-config.yml`. Tunables are read from the environment in [`internal/server/`](internal/server/) and [`cmd/parserd/`](cmd/parserd/).

## Architecture Overview

- **[cmd/router/](cmd/router/)**: HTTP API, admission limits, and vision model dispatch via the Tinfoil Go SDK
- **[cmd/parserd/](cmd/parserd/)**: Broker in the parser container that spawns one sandboxed process per document
- **[cmd/sandbox-exec/](cmd/sandbox-exec/)**: Applies Landlock, seccomp, and resource limits before executing a parser
- **[cmd/pdfparser/](cmd/pdfparser/)**, **[internal/pdftomd/](internal/pdftomd/)**: MuPDF-based PDF to markdown conversion
- **[sidecar/](sidecar/)**: Python parsers for Office, HTML, and spreadsheet formats

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)
- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
