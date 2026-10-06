// Package processing defines the closed vocabulary allowed in document errors.
package processing

import (
	"context"
	"errors"
)

type Code string

const (
	Failed                Code = "document_processing_failed"
	Empty                 Code = "document_empty"
	ExtractionFailed      Code = "document_extraction_failed"
	RenderFailed          Code = "document_render_failed"
	ParserFailed          Code = "document_parser_failed"
	ParserTimeout         Code = "document_parser_timeout"
	ParserOutputLimit     Code = "document_parser_output_limit"
	ParserUnavailable     Code = "document_parser_unavailable"
	ParserInvalidResponse Code = "document_parser_invalid_response"
	ParserBusy            Code = "document_parser_busy"
	OCRFailed             Code = "document_ocr_failed"
	OCRUnavailable        Code = "document_ocr_unavailable"
	OCRTimeout            Code = "document_ocr_timeout"
	Canceled              Code = "document_processing_canceled"
)

func Message(code Code) string {
	switch code {
	case Empty:
		return "The document is empty. Please upload a file with content."
	case ExtractionFailed, ParserFailed:
		return "Could not extract content from the document. Try exporting it again or uploading a text version."
	case RenderFailed:
		return "Could not render the document pages. Try exporting the document again or splitting it into smaller files."
	case ParserTimeout:
		return "Document processing exceeded the time limit. Try splitting the document into smaller files."
	case ParserOutputLimit:
		return "Document processing exceeded an output limit. Try splitting the document into smaller files."
	case ParserUnavailable:
		return "The document processing service is temporarily unavailable. Please try again later."
	case ParserInvalidResponse:
		return "The document processing service returned an invalid response. Please try again later."
	case ParserBusy:
		return "parser busy"
	case OCRFailed:
		return "Could not recognize text in the document images. Please retry or upload a text version."
	case OCRUnavailable:
		return "The document text-recognition service is temporarily unavailable. Please try again later."
	case OCRTimeout:
		return "Document text recognition exceeded the time limit. Try splitting the document into smaller files."
	case Canceled:
		return "Document processing was canceled. Please retry the upload."
	default:
		return "Document processing failed. Please retry or upload a text version."
	}
}

// Normalize prevents untrusted codes from becoming response or metric values.
func Normalize(code Code) Code {
	switch code {
	case Empty, ExtractionFailed, RenderFailed, ParserFailed, ParserTimeout,
		ParserOutputLimit, ParserUnavailable, ParserInvalidResponse, ParserBusy,
		OCRFailed, OCRUnavailable, OCRTimeout, Canceled:
		return code
	default:
		return Failed
	}
}

type Error struct {
	code  Code
	cause error
}

func (err *Error) Error() string { return Message(err.code) }
func (err *Error) Unwrap() error { return err.cause }

func Wrap(err error, code Code) error {
	return &Error{code: Normalize(code), cause: err}
}

func CodeOf(err error) Code {
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	var classified *Error
	if errors.As(err, &classified) {
		return Normalize(classified.code)
	}
	return Failed
}

// AtStage retains a specific failure reason, but gives unclassified parser
// failures the extraction/rendering context known by the caller.
func AtStage(err error, stage Code) error {
	switch CodeOf(err) {
	case Failed, ParserFailed:
		return Wrap(err, stage)
	default:
		return err
	}
}
