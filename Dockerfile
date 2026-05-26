# Stage 1: Build MuPDF 1.27.2 from source (pinned + checksum verified)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS mupdf-builder

RUN apk add --no-cache build-base curl

ARG MUPDF_VERSION=1.27.2
ARG MUPDF_SHA256=553867b135303dc4c25ab67c5f234d8e900a0e36e66e8484d99adc05fe1e8737

RUN curl -fsSL "https://mupdf.com/downloads/archive/mupdf-${MUPDF_VERSION}-source.tar.gz" -o /tmp/mupdf.tar.gz && \
    echo "${MUPDF_SHA256}  /tmp/mupdf.tar.gz" | sha256sum -c && \
    tar xzf /tmp/mupdf.tar.gz -C /tmp && \
    rm /tmp/mupdf.tar.gz

RUN cd /tmp/mupdf-${MUPDF_VERSION}-source && \
    make HAVE_X11=no HAVE_GLUT=no HAVE_CURL=no build=release libs -j$(nproc) && \
    make HAVE_X11=no HAVE_GLUT=no HAVE_CURL=no build=release prefix=/usr/local install

# Stage 2: Build Go binaries (router + parser linked against MuPDF)
FROM golang:1.25-alpine@sha256:8d22e29d960bc50cd025d93d5b7c7d220b1ee9aa7a239b3c8f55a57e987e8d45 AS go-builder

RUN apk add --no-cache gcc musl-dev

COPY --from=mupdf-builder /usr/local/lib/libmupdf*.a /usr/local/lib/
COPY --from=mupdf-builder /usr/local/include/mupdf/ /usr/local/include/mupdf/

WORKDIR /app
COPY go.mod go.sum ./
COPY internal/ ./internal/
COPY cmd/ ./cmd/
RUN CGO_ENABLED=1 go build -o /app/bin/router -ldflags="-s -w" ./cmd/router
RUN CGO_ENABLED=1 go build -o /app/bin/pdfparser -ldflags="-s -w" ./cmd/pdfparser

# Stage 3: Runtime with minimal Python sidecar for non-PDF formats
FROM python:3.12-alpine@sha256:236173eb74001afe2f60862de935b74fcbd00adfca247b2c27051a70a6a39a2d

RUN apk add --no-cache curl

RUN pip install --no-cache-dir \
    python-docx \
    python-pptx \
    beautifulsoup4 \
    openpyxl \
    fastapi \
    uvicorn \
    python-multipart

COPY --from=go-builder /app/bin/router /usr/local/bin/router
COPY --from=go-builder /app/bin/pdfparser /usr/local/bin/pdfparser
COPY sidecar/app.py /app/sidecar/app.py

EXPOSE 5000 5002

CMD ["sh", "-c", "uvicorn sidecar.app:app --host 127.0.0.1 --port 5002 --workers 2 --app-dir /app & router & wait -n"]
