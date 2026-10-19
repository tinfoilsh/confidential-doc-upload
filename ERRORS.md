# Document processing errors

Conversion failures return 502 with a fixed message and one of the stable codes
below; parser backpressure returns 429 with a `Retry-After` header. The body
keeps the legacy `error` string and adds flat OpenAI-compatible `object`,
`message`, `type`, `code`, and `param` fields. The model router nests these in
its `error` object and keeps the code, but replaces server-error messages with a
generic one on file inputs.

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

The parser broker's internal `document_parser_failed` code is reported to
callers as an extraction or render failure. Unknown codes become
`document_processing_failed`.

Error responses, failure logs, and the `router_document_processing_errors_total`
metric never contain filenames, document text, page indexes, parser stderr,
upstream response bodies, or raw exception messages.
