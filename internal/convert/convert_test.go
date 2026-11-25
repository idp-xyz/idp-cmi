package convert

import (
	"context"
	"errors"
	"strings"
	"testing"

	"idp-cmi/internal/mcpclient"
)

func TestTheScriptIsTheConverterOfTheType(t *testing.T) {
	for ext, want := range map[string]string{
		"pdf":  "pdftext in/original.pdf --markdown out/extract.md",
		"docx": "ooxml2md in/original.docx -o out/extract.md",
		"pptx": "ooxml2md in/original.pptx -o out/extract.md",
		"xlsx": "ooxml2md in/original.xlsx -o out/extract.md --max-rows 10000 --max-cols 200",
	} {
		if got := runCode(ext, "in/original."+ext); !strings.Contains(got, want) || !strings.HasPrefix(got, "mkdir -p out && ") {
			t.Errorf("%s: %q", ext, got)
		}
	}
}

func TestWhatTheSandboxSaysIsClassified(t *testing.T) {
	for text, transientWant := range map[string]bool{
		"the sandbox is at capacity; try again later":                           true,
		"you already hold the maximum number of workspaces; delete one first":   true,
		"the workspace is running another command":                              true,
		"the sandbox failed to complete the request":                            true,
		"the file is larger than the transfer limit":                            false,
		".docm files can carry macros and are not accepted":                     false,
		"the file reference is not valid for this run (use the attachment ...)": false,
		"workspace not found":                                                   false,
	} {
		if got := classifyTool(text, "x").transient; got != transientWant {
			t.Errorf("%q: transient %v", text, got)
		}
	}
	for text, transientWant := range map[string]bool{
		"the file could not be moved: the Workspace did not answer: dial tcp":    true,
		"the file could not be moved: the Workspace refused the file (HTTP 503)": true,
		"the file could not be moved: the Workspace refused the file (HTTP 429)": true,
		"the file reference is not valid for this run":                           false,
		"the file could not be moved: the Workspace refused the file (HTTP 422)": false,
		"zip entry too large": false,
	} {
		if got := classifyTransfer(text, CodeImportRefused).transient; got != transientWant {
			t.Errorf("%q: transient %v", text, got)
		}
	}
}

func TestWhatTheGatewayDoesIsClassified(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{&mcpclient.NetworkError{Err: errors.New("connection refused")}, CodeGatewayDown},
		{&mcpclient.NetworkError{Err: errors.New("timeout"), Timeout: true}, CodeGatewayTimeout},
		{&mcpclient.HTTPError{Status: 502}, CodeGatewayDown},
		{&mcpclient.HTTPError{Status: 401}, CodeGatewayDown},
		{&mcpclient.RPCError{Code: -32001, Message: "forbidden", HTTPStatus: 403}, CodeGatewayDown},
		{context.DeadlineExceeded, CodeGatewayTimeout},
	}
	for _, c := range cases {
		if e := classifyCall(c.err); !e.transient || e.code != c.code {
			t.Errorf("%v: %+v", c.err, e)
		}
	}
}
