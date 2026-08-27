package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abigotado/jira-cli/internal/errx"
)

type issue struct {
	key, summary, description string
	priority                  int
}

func (i issue) Fields() []Field {
	return []Field{
		{Name: "key", Value: i.key, Raw: i.key},
		{Name: "summary", Value: i.summary, Raw: i.summary},
		{Name: "priority", Raw: i.priority},
		{Name: "description", Value: i.description, Raw: i.description, OnRequest: true},
	}
}

func writer(format Format, fields []string) (*Writer, *bytes.Buffer, *bytes.Buffer) {
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &Writer{Format: format, Fields: fields, Out: out, Err: stderr}, out, stderr
}

type outputBoundaryWriter struct {
	buffer         bytes.Buffer
	writeErr       error
	failFirstWrite bool
	firstWriteSize int
	failEveryWrite bool
	writes         int
}

func (w *outputBoundaryWriter) Write(value []byte) (int, error) {
	w.writes++
	if w.failFirstWrite && w.writes == 1 {
		return 0, w.writeErr
	}
	if w.firstWriteSize > 0 && w.writes == 1 {
		n := min(w.firstWriteSize, len(value))
		if _, err := w.buffer.Write(value[:n]); err != nil {
			return 0, err
		}
		return n, w.writeErr
	}
	if w.failEveryWrite {
		return 0, w.writeErr
	}
	return w.buffer.Write(value)
}

func (w *outputBoundaryWriter) Bytes() []byte {
	return w.buffer.Bytes()
}

func (w *outputBoundaryWriter) String() string {
	return w.buffer.String()
}

func decodeEnvelope(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	return env
}

func TestSuccessEnvelopeAndCompactProjection(t *testing.T) {
	w, out, stderr := writer(FormatJSON, nil)
	w.WithContext("work", "https://example.atlassian.net")
	if err := w.Success(issue{key: "WL-1", summary: "Ship it", description: "long", priority: 2}); err != nil {
		t.Fatalf("Success: %v", err)
	}
	env := decodeEnvelope(t, out)
	if env["ok"] != true || env["v"] != float64(1) {
		t.Errorf("contract fields = %v", env)
	}
	data := env["data"].(map[string]any)
	if _, ok := data["description"]; ok {
		t.Error("on-request description leaked into compact output")
	}
	if data["priority"] != float64(2) {
		t.Errorf("priority lost its numeric type: %v", data["priority"])
	}
	meta := env["meta"].(map[string]any)
	if meta["profile"] != "work" || meta["site"] != "https://example.atlassian.net" {
		t.Errorf("invocation context = %v", meta)
	}
	if _, ok := meta["count"]; ok {
		t.Error("single object pretends to be a collection")
	}
	if stderr.Len() != 0 {
		t.Errorf("JSON success wrote stderr: %q", stderr.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Errorf("JSON envelope should be one compact line: %q", out.String())
	}
}

func TestCollectionAndPaginationMetadata(t *testing.T) {
	tests := []struct {
		name       string
		emit       func(*Writer) error
		wantCount  float64
		wantTrunc  bool
		wantCursor string
	}{
		{"empty complete collection", func(w *Writer) error { return w.Success([]issue{}) }, 0, false, ""},
		{"page with cursor", func(w *Writer) error { return w.SuccessPage([]issue{{key: "WL-1"}}, true, "opaque") }, 1, true, "opaque"},
		{"last page", func(w *Writer) error { return w.SuccessPage([]issue{{key: "WL-1"}}, false, "") }, 1, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out, _ := writer(FormatJSON, nil)
			if err := tt.emit(w); err != nil {
				t.Fatalf("emit: %v", err)
			}
			meta := decodeEnvelope(t, out)["meta"].(map[string]any)
			if meta["count"] != tt.wantCount || meta["truncated"] != tt.wantTrunc {
				t.Errorf("meta = %v", meta)
			}
			if got, _ := meta["next_cursor"].(string); got != tt.wantCursor {
				t.Errorf("next_cursor = %q, want %q", got, tt.wantCursor)
			}
		})
	}
}

func TestProjectionValidationAndShape(t *testing.T) {
	tests := []struct {
		name    string
		format  Format
		fields  []string
		data    any
		wantErr bool
	}{
		{"select on request field", FormatJSON, []string{"key", "description"}, issue{key: "WL-1", description: "body"}, false},
		{"unknown JSON field", FormatJSON, []string{"bogus"}, issue{}, true},
		{"unknown text field", FormatText, []string{"bogus"}, issue{}, true},
		{"fields with raw", FormatRaw, []string{"key"}, issue{}, true},
		{"fields on static payload", FormatJSON, []string{"key"}, struct{ A int }{1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out, _ := writer(tt.format, tt.fields)
			err := w.Success(tt.data)
			if tt.wantErr {
				if errx.ExitCode(err) != errx.CodeUsage {
					t.Fatalf("error = %v, code = %d", err, errx.ExitCode(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("Success: %v", err)
			}
			env := decodeEnvelope(t, out)
			data := env["data"].(map[string]any)
			if len(data) != 2 || data["description"] != "body" {
				t.Errorf("projection = %v", data)
			}
		})
	}
}

func TestValidateRejectsInvalidProjectionBeforeRendering(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		fields []string
		data   any
		want   errx.Code
	}{
		{name: "known object field", format: FormatJSON, fields: []string{"key"}, data: issue{}},
		{name: "known empty collection field", format: FormatJSON, fields: []string{"key"}, data: []issue{}},
		{name: "unknown field", format: FormatJSON, fields: []string{"authorization"}, data: issue{}, want: errx.CodeUsage},
		{name: "raw with fields", format: FormatRaw, fields: []string{"key"}, data: issue{}, want: errx.CodeUsage},
		{name: "non field payload", format: FormatJSON, fields: []string{"key"}, data: struct{ Key string }{}, want: errx.CodeUsage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w, stdout, stderr := writer(test.format, test.fields)
			err := w.Validate(test.data)
			if got := errx.ExitCode(err); got != test.want {
				t.Fatalf("exit code = %d, want %d (err=%v)", got, test.want, err)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("validation rendered output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestConcreteSliceAndObjectShapesAreStable(t *testing.T) {
	tests := []struct {
		name      string
		data      any
		wantArray bool
	}{
		{"object", issue{key: "WL-1"}, false},
		{"concrete slice", []issue{{key: "WL-1"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out, _ := writer(FormatJSON, []string{"key"})
			if err := w.Success(tt.data); err != nil {
				t.Fatalf("Success: %v", err)
			}
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			isArray := strings.HasPrefix(strings.TrimSpace(string(env.Data)), "[")
			if isArray != tt.wantArray {
				t.Errorf("data = %s, wantArray=%v", env.Data, tt.wantArray)
			}
		})
	}
}

func TestFailureEnvelopeAndExitStatus(t *testing.T) {
	candidates := []errx.Candidate{{ID: "10000", Name: "Work", Kind: "project"}}
	tests := []struct {
		name       string
		err        error
		wantStatus errx.Code
		wantReason string
		wantDetail string
	}{
		{"ambiguous", errx.Ambiguous("project", "W", candidates), errx.CodeAmbiguous, "AMBIGUOUS_PROJECT", "candidates"},
		{"not found", errx.NotFound("project", "Wrok", candidates), errx.CodeNotFound, "NOT_FOUND_PROJECT", "did_you_mean"},
		{"permission", errx.Permission("SCOPE_DENIED", "missing scope"), errx.CodePermission, "SCOPE_DENIED", ""},
		{"conflict", errx.Conflict("STALE_ISSUE", "changed"), errx.CodeConflict, "STALE_ISSUE", ""},
		{"retryable", errx.Retryable("RATE_LIMITED", time.Second, "slow"), errx.CodeRetryable, "RATE_LIMITED", "retry_after"},
		{"raw error", errors.New("boom"), errx.CodeInternal, "INTERNAL", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out, stderr := writer(FormatJSON, nil)
			if got := w.Failure(tt.err); got != tt.wantStatus {
				t.Errorf("status = %d, want %d", got, tt.wantStatus)
			}
			env := decodeEnvelope(t, out)
			body := env["error"].(map[string]any)
			if body["code"] != tt.wantReason || env["hint"] == "" {
				t.Errorf("failure = %v", env)
			}
			if tt.wantDetail != "" {
				if _, ok := body[tt.wantDetail]; !ok {
					t.Errorf("error body missing %s: %v", tt.wantDetail, body)
				}
			}
			if stderr.Len() != 0 {
				t.Errorf("JSON failure wrote stderr: %q", stderr.String())
			}
		})
	}
}

func TestJSONOutputBoundaryFailureDoesNotAppendOrLeak(t *testing.T) {
	writeErr := errors.New("OUTPUT_WRITER_TOKEN_SENTINEL OUTPUT_WRITER_PATH_SENTINEL")
	tests := []struct {
		name          string
		newOutput     func() *outputBoundaryWriter
		outputStarted bool
	}{
		{
			name: "partial first write does not append failure envelope",
			newOutput: func() *outputBoundaryWriter {
				return &outputBoundaryWriter{writeErr: writeErr, firstWriteSize: 1}
			},
			outputStarted: true,
		},
		{
			name: "persistent zero byte failure leaves stdout empty",
			newOutput: func() *outputBoundaryWriter {
				return &outputBoundaryWriter{writeErr: writeErr, failEveryWrite: true}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout := test.newOutput()
			stderr := &bytes.Buffer{}
			w := &Writer{Format: FormatJSON, Out: stdout, Err: stderr}

			successErr := w.Success(issue{key: "WL-1", summary: "safe"})
			if successErr == nil {
				t.Fatal("Success succeeded despite output boundary failure")
			}
			if code := w.Failure(errx.Conflict("WRITE_OUTCOME_UNKNOWN", "the write outcome is unknown").Wrap(successErr)); code != errx.CodeConflict {
				t.Fatalf("failure exit code = %d, want %d", code, errx.CodeConflict)
			}

			if test.outputStarted {
				if stdout.writes != 1 {
					t.Fatalf("stdout writes = %d, want no second envelope after output started", stdout.writes)
				}
				if strings.Contains(stdout.String(), `"ok":false`) {
					t.Fatalf("stdout appended a failure envelope after partial output: %q", stdout.String())
				}
			} else if len(stdout.Bytes()) != 0 {
				t.Fatalf("persistent write failure produced stdout: %q", stdout.String())
			}

			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{"OUTPUT_WRITER_TOKEN_SENTINEL", "OUTPUT_WRITER_PATH_SENTINEL"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("output boundary failure leaked %q: stdout=%q stderr=%q", sentinel, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestJSONFailureAfterZeroByteSuccessWriteUsesSafeInternalEnvelope(t *testing.T) {
	writeErr := errors.New("ZERO_BYTE_TOKEN_SENTINEL ZERO_BYTE_PATH_SENTINEL")
	tests := []struct {
		name string
	}{
		{name: "first zero byte error then failure envelope succeeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout := &outputBoundaryWriter{writeErr: writeErr, failFirstWrite: true}
			stderr := &bytes.Buffer{}
			w := &Writer{Format: FormatJSON, Out: stdout, Err: stderr}

			successErr := w.Success(issue{key: "WL-1", summary: "safe"})
			if successErr == nil {
				t.Fatal("Success succeeded despite the first write failing")
			}
			if code := w.Failure(successErr); code != errx.CodeInternal {
				t.Fatalf("failure exit code = %d, want %d", code, errx.CodeInternal)
			}
			if stdout.writes != 2 {
				t.Fatalf("stdout writes = %d, want failed success then one failure envelope", stdout.writes)
			}

			var envelope Envelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("decode failure envelope: %v\n%s", err, stdout.String())
			}
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != "INTERNAL" {
				t.Fatalf("envelope = %#v, want an INTERNAL failure envelope", envelope)
			}
			combined := stdout.String() + stderr.String()
			for _, sentinel := range []string{"ZERO_BYTE_TOKEN_SENTINEL", "ZERO_BYTE_PATH_SENTINEL"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("safe failure envelope leaked %q: stdout=%q stderr=%q", sentinel, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestJSONEnvelopeKeySetsArePinned(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Writer) error
		want []string
	}{
		{"object", func(w *Writer) error { return w.Success(issue{}) }, []string{"data", "ok", "v"}},
		{"collection", func(w *Writer) error { return w.Success([]issue{}) }, []string{"data", "meta", "ok", "v"}},
		{"failure", func(w *Writer) error { w.Failure(errx.Usage("bad")); return nil }, []string{"error", "hint", "ok", "v"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out, _ := writer(FormatJSON, nil)
			if err := tt.emit(w); err != nil {
				t.Fatal(err)
			}
			env := decodeEnvelope(t, out)
			got := make([]string, 0, len(env))
			for key := range env {
				got = append(got, key)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("keys = %v, want %v; assess an envelope version bump", got, tt.want)
			}
		})
	}
}

func TestTextOutputIsOneLinePerEntity(t *testing.T) {
	w, out, stderr := writer(FormatText, []string{"key", "description", "priority"})
	rows := []issue{
		{key: "WL-1", description: "first\nsecond", priority: 1},
		{key: "WL-2", priority: 2},
	}
	if err := w.Success(rows); err != nil {
		t.Fatalf("Success: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "first second") {
		t.Errorf("text output = %q", out.String())
	}
	for index, line := range lines {
		if len(strings.Split(line, "  ")) != 3 {
			t.Errorf("line %d lost a projected column: %q", index, line)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("text success wrote stderr: %q", stderr.String())
	}
}

func TestTextFailureUsesOnlyStderr(t *testing.T) {
	w, out, stderr := writer(FormatText, nil)
	w.Failure(errx.Ambiguous("project", "W", []errx.Candidate{{ID: "1", Name: "Work"}}))
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	for _, want := range []string{"error:", "hint:", "Work"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		value   string
		want    Format
		wantErr bool
	}{
		{"text", FormatText, false}, {"json", FormatJSON, false}, {"raw", FormatRaw, false},
		{"", "", true}, {"JSON", "", true}, {"yaml", "", true},
	}
	for _, tt := range tests {
		t.Run("value="+tt.value, func(t *testing.T) {
			got, err := ParseFormat(tt.value)
			if tt.wantErr {
				if errx.ExitCode(err) != errx.CodeUsage {
					t.Errorf("error = %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ParseFormat = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestDefaultFormatForRegularAndClosedFiles(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	if got := DefaultFormat(file); got != FormatJSON {
		t.Errorf("regular file = %q", got)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := DefaultFormat(file); got != FormatJSON {
		t.Errorf("closed file = %q", got)
	}
}

func TestUnsupportedWriterFormatFailsInternal(t *testing.T) {
	w, _, _ := writer(Format("xml"), nil)
	if err := w.Success(issue{}); errx.ExitCode(err) != errx.CodeInternal {
		t.Errorf("error = %v, code = %d", err, errx.ExitCode(err))
	}
}
