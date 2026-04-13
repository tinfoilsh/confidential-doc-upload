FROM golang:1.25-alpine AS go-builder
WORKDIR /app
COPY router/go.mod ./
COPY router/*.go ./
RUN CGO_ENABLED=0 go mod tidy && go build -o router .

FROM python:3.12-slim

RUN pip install --no-cache-dir \
    pymupdf \
    python-docx \
    python-pptx \
    beautifulsoup4 \
    openpyxl \
    fastapi \
    uvicorn \
    python-multipart

COPY --from=go-builder /app/router /usr/local/bin/router
COPY sidecar/app.py /app/sidecar/app.py
EXPOSE 5000 5002

CMD ["bash", "-c", "uvicorn sidecar.app:app --host 127.0.0.1 --port 5002 --workers 2 --app-dir /app & /usr/local/bin/router & wait -n"]
