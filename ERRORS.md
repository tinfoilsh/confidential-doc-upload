# Document processing errors

Conversion failures return a fixed message and a stable code. Responses keep the
legacy `error` string and include flat OpenAI-compatible `object`, `message`,
`type`, `code`, and `param` fields. The model router normalizes this into its
nested `error` object without losing the code or message on `/v1/convert/file`.
Its internal file-input conversion path preserves the code but intentionally
replaces server-error messages with its generic document-processing message.

Existing status behavior is preserved: conversion failures return 502; parser
backpressure returns 429 with the bounded `Retry-After` value. Request-validation
errors and successful conversion responses are unchanged.

| Code | Meaning |
| --- | --- |
| `document_empty` | The uploaded file is empty. |
| `document_extraction_failed` | Content extraction failed without a more specific reason. |
| `document_render_failed` | Page rendering failed without a more specific reason. |
| `document_parser_timeout` | The parser or its request reached a time limit. |
| `document_parser_output_limit` | A parser output or response size limit was exceeded. |
| `document_parser_unavailable` | The parser could not start or could not be reached, or returned a server error. |
| `document_parser_invalid_response` | The parser response could not be read or decoded. |
| `document_parser_busy` | Parser capacity is exhausted; honor `Retry-After`. |
| `document_ocr_failed` | Text recognition failed or returned no choices. |
| `document_ocr_unavailable` | The OCR client is unavailable or encountered transport, authentication, or server errors. |
| `document_ocr_timeout` | Text recognition exceeded its deadline. |
| `document_processing_canceled` | Processing was canceled. |
| `document_processing_failed` | Unclassified failure; no exception text is exposed. |

The private parser broker also uses `document_parser_failed` for a child-process
failure. The document service translates that into extraction or rendering
context. A process exit alone does not prove malformed input, memory exhaustion,
or a sandbox violation, so the API does not claim one of those causes.

`router_document_processing_errors_total{code="..."}` counts conversion failures
by the same closed code set. Existing `router_errors_total{type="502"}` and
`{type="429"}` counters remain available. Neither the new metric nor processing
failure logs contain filenames, document text, page indexes, parser stderr,
upstream response bodies, or raw exception messages. Unknown broker codes are
discarded rather than reflected. No request IDs or document hashes are added.

Deploy both the parser broker and document service from the same release to get
timeout and output-limit classifications. A new service talking to an old broker
still returns safe extraction/rendering categories for its plain-text errors.
