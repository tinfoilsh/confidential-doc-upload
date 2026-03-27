package main

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

func postMultipartStreaming(ctx context.Context, pdfData []byte, filename string, fields []fieldPair) ([]byte, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mw.Close()

		fw, err := mw.CreateFormFile("files", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := fw.Write(pdfData); err != nil {
			pw.CloseWithError(err)
			return
		}
		for _, f := range fields {
			if err := mw.WriteField(f.key, f.value); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", doclingURL+"/v1/convert/file", pr)
	if err != nil {
		pr.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = -1

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docling request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading docling response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := string(body)
		if len(detail) > 512 {
			detail = detail[:512] + "..."
		}
		return nil, fmt.Errorf("docling returned %d: %s", resp.StatusCode, detail)
	}
	return body, nil
}
