# Stage 1: Build MuPDF 1.28.2 from source (pinned + checksum verified)
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS mupdf-builder

RUN apk add --no-cache build-base curl

ARG MUPDF_VERSION=1.28.2
ARG MUPDF_SHA256=44075a84e329db55b9bef5f342a70fd26d69e48ad1d33cb89d9664581c641156

RUN curl -fsSL "https://mupdf.com/downloads/archive/mupdf-${MUPDF_VERSION}-source.tar.gz" -o /tmp/mupdf.tar.gz && \
    echo "${MUPDF_SHA256}  /tmp/mupdf.tar.gz" | sha256sum -c && \
    tar xzf /tmp/mupdf.tar.gz -C /tmp && \
    rm /tmp/mupdf.tar.gz

RUN cd /tmp/mupdf-${MUPDF_VERSION}-source && \
    make HAVE_X11=no HAVE_GLUT=no HAVE_CURL=no build=release libs -j$(nproc) && \
    make HAVE_X11=no HAVE_GLUT=no HAVE_CURL=no build=release prefix=/usr/local install

# Stage 2: Build Go binaries (router + parser linked against MuPDF)
FROM golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS go-builder

RUN apk add --no-cache gcc musl-dev

COPY --from=mupdf-builder /usr/local/lib/libmupdf*.a /usr/local/lib/
COPY --from=mupdf-builder /usr/local/include/mupdf/ /usr/local/include/mupdf/

WORKDIR /app
COPY go.mod go.sum ./
ENV GOTOOLCHAIN=local
RUN go mod download && go mod verify

COPY internal/ ./internal/
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /app/bin/router -ldflags="-s -w -buildid=" ./cmd/router && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /app/bin/parserd -ldflags="-s -w -buildid=" ./cmd/parserd && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /app/bin/sandbox-exec -ldflags="-s -w -buildid=" ./cmd/sandbox-exec && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /app/bin/healthcheck -ldflags="-s -w -buildid=" ./cmd/healthcheck
RUN CGO_ENABLED=1 go build -mod=readonly -trimpath -buildvcs=false -o /app/bin/pdfparser -ldflags="-s -w -buildid=" ./cmd/pdfparser && \
	chmod 0500 /app/bin/pdfparser /app/bin/parserd /app/bin/sandbox-exec

# Stage 3: Runtime. The same immutable image runs either the secret-bearing
# router or the networkless parser broker, selected by the deployment command.
FROM python:3.12.14-alpine@sha256:3b80023c96c186093365774a00db452bfc635476319e71e56a840e251457701f
RUN addgroup -S -g 65532 app && \
    adduser -S -D -H -u 65532 -G app app && \
    install -d -o app -g app -m 0750 /run/docparser

COPY sidecar/requirements.lock /tmp/requirements.lock
RUN python -m pip install --no-cache-dir --only-binary=:all: --require-hashes -r /tmp/requirements.lock && \
	python -m pip uninstall --yes pip && \
    rm /tmp/requirements.lock

COPY --from=go-builder /app/bin/router /usr/local/bin/router
COPY --chown=65532:65532 --from=go-builder /app/bin/pdfparser /usr/local/bin/pdfparser
COPY --chown=65532:65532 --from=go-builder /app/bin/parserd /usr/local/bin/parserd
COPY --chown=65532:65532 --from=go-builder /app/bin/sandbox-exec /usr/local/bin/sandbox-exec
COPY --from=go-builder /app/bin/healthcheck /usr/local/bin/healthcheck
COPY --chown=65532:65532 sidecar/app.py /app/docparser.py

# The router's UID can connect to the broker but cannot read or execute parser
# code from the shared image. The parser UID owns these immutable files.
RUN chown 65532:65532 /usr/local/bin/python3.12 && \
	chmod 0500 /usr/local/bin/python3.12 && \
	chmod 0400 /app/docparser.py

EXPOSE 5000

USER 65532:65532

CMD ["/usr/local/bin/router"]
