# Development

The PDF parser links against MuPDF, so tests run inside the Docker build stage that has it.

Run the tests, race detector, and vet:

```bash
docker build --target go-builder -t doc-upload-builder .
docker run --rm -v "$PWD:/src:ro" -w /src doc-upload-builder \
    /bin/sh -c 'go test ./... && go test -race ./... && go vet ./...'
```

Run the parser and router as separate containers with the production isolation flags (`tinfoil-config.yml` applies the equivalent topology in production):

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
```

Convert a file:

```bash
curl -F "files=@test.pdf" "http://localhost:5000/v1/convert/file?mode=raw"
```
