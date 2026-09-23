# Security architecture

All document formats are attacker-controlled input. MuPDF and the Python format
libraries therefore run only in short-lived subprocesses inside a dedicated
parser container. Unix file permissions prevent the secret-bearing router UID
from reading or executing parser code even though both roles share one image.


| Layer | Enforcement |
| --- | --- |
| **Secret boundary** | Only the router container receives `TINFOIL_API_KEY`. Router and parser use different UIDs and separate PID namespaces; the parser has no secret environment. Parser executables/source are owner-only to the parser UID. Brokers and every request parser are non-dumpable. |
| **No parser network** | The parser container has no network attachment. The child seccomp policy also rejects socket, socketpair, io_uring, and every network syscall. |
| **Allowlisted router egress** | The secret-bearing router may connect only to `atc.tinfoil.sh` for client-verified attestation bundles, `tuf-repo-cdn.sigstore.dev` for signed Sigstore trust-root metadata, and the ATC-advertised inference routers for end-to-end encrypted VLM requests. cvmimage enforces the resolved-IP allowlist outside the container. |
| **Fresh document children** | Every parse pass receives exactly one document over stdin in a fresh process and exits afterward. Admission is capped at two parser processes; multi-file requests also use at most two workers. The socket volume holds a non-blocking single-broker lock. |
| **Filesystem isolation** | Every child enters a new, fail-closed Landlock domain. A PDF pass can open only its binary and loader. A Python pass can additionally open only its interpreter, immutable libraries, and fixed parser source. The other parser, `/proc`, `/run`, `/tmp`, `/sys`, `/etc`, and the socket volume are invisible. Executable access is separately allowlisted. |
| **No surviving children** | `fork`, `vfork`, `clone3`, and non-thread `clone` are rejected. The parser runs in its own process group, uses a parent-death signal, and the whole group is killed on cancellation or limit violation. |
| **Empty environment** | `sandbox-exec` replaces itself with the parser using an empty environment. No request parser inherits broker configuration. |
| **Syscall restrictions** | Seccomp rejects cross-process inspection/signalling and resource-limit changes, namespaces, mounts, writable opens, filesystem mutation, kernel keyrings, BPF, perf, userfaultfd, io_uring, modules, and IPC. Filter installation is synchronized across threads and fails closed. |
| **Hard resource limits** | Every parser gets kernel limits for address space (1536 MiB), CPU time (120s), open files (64), output (256 MiB), stderr (1 MiB), and zero-sized files/core dumps/locked memory/message queues. The parser container also has memory, CPU, and PID cgroup caps. |
| **Authenticated private API** | The router gets connect-only group access to the parser socket. The broker additionally authenticates every connection with kernel `SO_PEERCRED` and accepts only the fixed router/parser UIDs. |
| **Immutable runtime** | The router runs as 65533:65532 and the parser as 65532:65532, with all capabilities dropped, `no-new-privileges`, no IPC namespace sharing, read-only root filesystems, and no parser tmpfs. |
| **Bounded admission** | Upload size, file count, multipart parts, body-unread waiting requests, active requests, total document workers, per-request workers, VLM fan-out, parser response size, and parser wall time are bounded. Only four requests may enter the body-buffering path; sixteen more may wait. Parser slots are acquired before parser request bodies are buffered. |
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

## Concurrency and efficiency

Parser work is not globally sequential. `parserd` admits two passes by
default, and a multi-file request can schedule two documents at once. Each
admitted pass still creates a new process, enters a new Landlock domain, reads
one document from stdin, emits one bounded response, and exits. There is no
parser process pool and no process reuse across documents. A global router gate
preserves the configured aggregate work ceiling across concurrent requests.
An independent sixteen-request queue absorbs short bursts for up to 30 seconds
without admitting more than four requests into the memory-heavy processing
path.

The two-worker limit is intentional: it recovers parallel throughput without
allowing attacker-controlled parsing to expand to the VM's full CPU and memory.
On the 2026-08-13/14 box3 validation snapshot, 100 tiny raw-PDF requests at
concurrency four took a median 0.485s with the serialized baseline and 0.282s
with this design. Twenty 15-page PDFs improved from 4.5 to 8.4 documents per
second. An eight-file request fell from 0.05s to 0.02s. The exact cvmimage
v0.11.0 debug guest measured 0.28–0.29s steady-state and 0.02s respectively.
These are parser-overhead microbenchmarks, not latency claims for large PDFs or
VLM modes.

## Threat-model limits

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

## Validation gate

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

