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
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

EXPOSE 5000 5002

ENTRYPOINT ["/app/entrypoint.sh"]
