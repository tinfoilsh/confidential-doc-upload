package processing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassificationNeverUsesErrorText(t *testing.T) {
	const private = "private@example.test /secret/customer.pdf secret-token"
	for _, code := range []Code{Failed, Empty, ExtractionFailed, RenderFailed,
		ParserFailed, ParserTimeout, ParserOutputLimit, ParserUnavailable,
		ParserInvalidResponse, ParserBusy, OCRFailed, OCRUnavailable, OCRTimeout, Canceled, Code(private)} {
		t.Run(string(code), func(t *testing.T) {
			cause := errors.New(private)
			err := Wrap(cause, code)
			if strings.Contains(err.Error(), private) || !errors.Is(err, cause) {
				t.Fatal("error must hide its cause but retain unwrapping")
			}
			got := CodeOf(fmt.Errorf("%s: %w", private, err))
			if got != Normalize(code) || strings.Contains(Message(got), private) {
				t.Fatalf("unsafe classification: %q", got)
			}
		})
	}
	if CodeOf(errors.New(string(ParserTimeout))) != Failed {
		t.Fatal("untyped error text influenced classification")
	}
}

func TestStagesPreserveSpecificReasonsAndCancellation(t *testing.T) {
	for _, stage := range []Code{ExtractionFailed, RenderFailed, OCRFailed} {
		if got := CodeOf(AtStage(Wrap(errors.New("private"), ParserFailed), stage)); got != stage {
			t.Fatalf("generic parser failure: got %s, want %s", got, stage)
		}
		for _, code := range []Code{Empty, ParserTimeout, ParserOutputLimit, ParserUnavailable, ParserInvalidResponse, OCRTimeout} {
			if got := CodeOf(AtStage(Wrap(errors.New("private"), code), stage)); got != code {
				t.Fatalf("specific classification lost: got %s, want %s", got, code)
			}
		}
		if got := CodeOf(AtStage(Wrap(context.Canceled, stage), stage)); got != Canceled {
			t.Fatalf("cancellation misclassified: %s", got)
		}
	}
}

func FuzzUnknownCodesStayPrivate(f *testing.F) {
	f.Add("private@example.test")
	f.Add(string(ParserTimeout))
	f.Add("\r\nsecret")
	f.Fuzz(func(t *testing.T, input string) {
		code := CodeOf(Wrap(errors.New(input), Code(input)))
		if code != Normalize(Code(input)) {
			t.Fatal("classification escaped the allowlist")
		}
		if CodeOf(errors.New(input)) != Failed {
			t.Fatal("raw error text influenced classification")
		}
	})
}
