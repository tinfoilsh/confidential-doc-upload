package main

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

func postMultipartStreaming(pdfData []byte, filename string, fields []fieldPair) ([]byte, int64, error) {
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

	req, err := http.NewRequest("POST", doclingURL+"/v1/convert/file", pr)
	if err != nil {
		pr.Close()
		return nil, 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = -1

	tPost := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("docling request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	postMs := time.Since(tPost).Milliseconds()
	return body, postMs, err
}
