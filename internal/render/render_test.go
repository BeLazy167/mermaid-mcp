package render

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	for _, test := range []struct {
		input string
		want  Format
		err   bool
	}{
		{input: "", want: FormatPNG},
		{input: " PNG ", want: FormatPNG},
		{input: "svg", want: FormatSVG},
		{input: "pdf", err: true},
	} {
		got, err := ParseFormat(test.input)
		if (err != nil) != test.err || got != test.want {
			t.Errorf("ParseFormat(%q) = %q, %v", test.input, got, err)
		}
	}
}

func TestValidateRequest(t *testing.T) {
	valid := Request{Diagram: "graph TD; A-->B", Format: FormatPNG}
	if err := ValidateRequest(valid, 1_024); err != nil {
		t.Fatalf("ValidateRequest(valid) error = %v", err)
	}
	for _, test := range []struct {
		name    string
		request Request
		max     int64
		code    ErrorCode
	}{
		{name: "empty", request: Request{Diagram: " \n", Format: FormatPNG}, max: 10, code: CodeInvalidInput},
		{name: "invalid UTF-8", request: Request{Diagram: string([]byte{0xff}), Format: FormatPNG}, max: 10, code: CodeInvalidInput},
		{name: "too large", request: valid, max: 2, code: CodeTooLarge},
		{name: "format", request: Request{Diagram: "graph TD", Format: "pdf"}, max: 10, code: CodeUnsupportedFormat},
		{name: "init directive", request: Request{Diagram: `%%{ init: {"htmlLabels": true} }%%\ngraph TD; A-->B`, Format: FormatSVG}, max: 1_000, code: CodeRenderRejected},
		{name: "config directive", request: Request{Diagram: `%% { CONFIG : {"themeCSS": "x"} } %%\ngraph TD; A-->B`, Format: FormatSVG}, max: 1_000, code: CodeRenderRejected},
		{name: "front matter config", request: Request{Diagram: "---\ntitle: safe\nconfig:\n  htmlLabels: true\n---\ngraph TD; A-->B", Format: FormatSVG}, max: 1_000, code: CodeRenderRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			var renderErr *Error
			if err := ValidateRequest(test.request, test.max); !errors.As(err, &renderErr) || renderErr.Code != test.code {
				t.Fatalf("ValidateRequest() error = %v, want %q", err, test.code)
			}
		})
	}
}

func TestPublicMessageAndMIMEType(t *testing.T) {
	internal := errors.New("secret internal detail")
	err := &Error{Code: CodeInternal, Message: "rendering failed", Cause: internal}
	if !errors.Is(err, internal) || PublicMessage(err) != "rendering failed" || strings.Contains(PublicMessage(err), "secret") {
		t.Fatalf("error handling = %q", PublicMessage(err))
	}
	if PublicMessage(errors.New("unknown")) != "rendering failed" {
		t.Fatal("unknown error leaked")
	}
	if FormatPNG.MIMEType() != "image/png" || FormatSVG.MIMEType() != "image/svg+xml" {
		t.Fatal("wrong MIME type")
	}
}

func TestContextError(t *testing.T) {
	for cause, code := range map[error]ErrorCode{
		context.Canceled:         CodeCanceled,
		context.DeadlineExceeded: CodeTimeout,
	} {
		var renderErr *Error
		if err := contextError(cause); !errors.As(err, &renderErr) || renderErr.Code != code {
			t.Fatalf("contextError(%v) = %v", cause, err)
		}
	}
}

func TestSanitizeRendererMessage(t *testing.T) {
	tempDir := "/tmp/private-render"
	message := "Parse error in " + tempDir + "/diagram.mmd\n" +
		"useful detail\x00\n at file:///secret/worker.js:10\nsecret stack"
	got := sanitizeRendererMessage(message, tempDir)
	if got != "Parse error in <temp>/diagram.mmd\nuseful detail" {
		t.Fatalf("sanitizeRendererMessage() = %q", got)
	}
}
