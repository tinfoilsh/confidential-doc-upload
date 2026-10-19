# Confidential Document Processing

Tinfoil's document processing service converts uploaded files (PDF, DOCX, PPTX, HTML, XLSX, CSV, images) into markdown inside a secure enclave, so documents can be used as model input without leaving the confidential boundary. The model router uses it to serve `/v1/convert/file` and file inputs on the Responses and Chat Completions APIs.

Modes, response shape, supported formats, and limits are documented at [docs.tinfoil.sh/guides/document-processing](https://docs.tinfoil.sh/guides/document-processing). Error codes are listed in [ERRORS.md](ERRORS.md), and the parser sandbox and threat model are described in [SECURITY.md](SECURITY.md).

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) to test and run the service locally.

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)
- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
