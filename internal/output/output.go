// Package output renders jira-cli results while preserving the v1 machine
// envelope and keeping default projections small.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/abigotado/jira-cli/internal/errx"
)

// Format selects how a result is rendered.
type Format string

const (
	// FormatText is the compact human rendering.
	FormatText Format = "text"
	// FormatJSON is the v1 machine envelope.
	FormatJSON Format = "json"
	// FormatRaw is the modeled payload without envelope or projection.
	FormatRaw Format = "raw"
)

// ParseFormat validates a --output value.
func ParseFormat(value string) (Format, error) {
	switch Format(value) {
	case FormatText, FormatJSON, FormatRaw:
		return Format(value), nil
	default:
		return "", errx.Usage("invalid --output %q: want text, json, or raw", value)
	}
}

// ValidateFormat checks cross-flag output constraints without rendering.
func ValidateFormat(format Format, fields []string) error {
	if format == FormatRaw && len(fields) > 0 {
		return errx.Usage("--fields cannot be combined with --output raw")
	}
	return nil
}

// Field is one output-safe name/value pair for an entity.
type Field struct {
	// Name is the stable projection key.
	Name string
	// Value is the single-line human rendering.
	Value string
	// Raw preserves the JSON type during projection.
	Raw any
	// OnRequest excludes a potentially large field from the default
	// projection while keeping it available through --fields.
	OnRequest bool
}

// Renderable declares an entity's stable, compact output vocabulary.
type Renderable interface {
	Fields() []Field
}

// RenderableCollection supplies rows and a stable field vocabulary even when
// a page is empty. Jira issue fields are dynamic, so the request vocabulary
// must travel with the page instead of being inferred from its first row.
type RenderableCollection interface {
	RenderRows() []Renderable
	SchemaFields() []Field
}

// Envelope is the v1 machine contract. Its declaration order is output order.
type Envelope struct {
	OK    bool       `json:"ok"`
	V     int        `json:"v"`
	Data  any        `json:"data,omitempty"`
	Meta  *Meta      `json:"meta,omitempty"`
	Error *ErrorBody `json:"error,omitempty"`
	Hint  string     `json:"hint,omitempty"`
}

// Meta carries non-secret invocation and pagination context. Count and
// Truncated are pointers so an empty collection can truthfully emit zero and
// false while a single object does not pretend to be a collection.
type Meta struct {
	// Count is present for collections, including empty ones.
	Count *int `json:"count,omitempty"`
	// Truncated states whether the current page is incomplete.
	Truncated *bool `json:"truncated,omitempty"`
	// NextCursor is Jira's opaque next-page token.
	NextCursor string `json:"next_cursor,omitempty"`
	// Profile is the selected non-secret profile name.
	Profile string `json:"profile,omitempty"`
	// Site is the selected Jira tenant URL.
	Site string `json:"site,omitempty"`
}

// ErrorBody is the error half of the v1 envelope.
type ErrorBody struct {
	Code       string           `json:"code"`
	Message    string           `json:"message"`
	Candidates []errx.Candidate `json:"candidates,omitempty"`
	DidYouMean []errx.Candidate `json:"did_you_mean,omitempty"`
	RetryAfter string           `json:"retry_after,omitempty"`
}

// Writer renders results to explicitly separated output and diagnostic
// streams. In JSON mode stdout receives exactly one envelope.
type Writer struct {
	// Format selects the renderer.
	Format Format
	// Fields is an ordered projection allowlist.
	Fields []string
	// Out receives results only.
	Out io.Writer
	// Err receives diagnostics only.
	Err io.Writer

	profile string
	site    string
}

// New builds a Writer over stdout and stderr.
func New(format Format, fields []string) *Writer {
	return &Writer{Format: format, Fields: fields, Out: os.Stdout, Err: os.Stderr}
}

// WithContext adds the selected non-secret profile and Jira site to success
// metadata. It mutates the writer and returns it for command setup chaining.
func (w *Writer) WithContext(profile, site string) *Writer {
	w.profile = profile
	w.site = site
	return w
}

// Validate checks the requested projection against a value's live output
// vocabulary. Mutation commands call this before dispatching side effects.
func (w *Writer) Validate(data any) error {
	if err := ValidateFormat(w.Format, w.Fields); err != nil {
		return err
	}
	if len(w.Fields) == 0 {
		return nil
	}
	rows, _, ok := asRows(data)
	if !ok {
		return errx.Usage("--fields is not supported for this command")
	}
	if len(rows) > 0 {
		_, err := selectFields(rows[0].Fields(), w.Fields)
		return err
	}
	if schema := collectionSchema(data); schema != nil {
		_, err := selectFields(schema.Fields(), w.Fields)
		return err
	}
	return nil
}

// DefaultFormat returns JSON unless stdout is a terminal.
func DefaultFormat(stdout *os.File) Format {
	info, err := stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return FormatJSON
	}
	return FormatText
}

// Success renders a successful single value or complete collection.
func (w *Writer) Success(data any) error {
	return w.success(data, false, "", false)
}

// SuccessPage renders a collection page. nextCursor is opaque and is omitted
// when Jira reports no subsequent page.
func (w *Writer) SuccessPage(data any, truncated bool, nextCursor string) error {
	return w.success(data, truncated, nextCursor, true)
}

func (w *Writer) success(data any, truncated bool, nextCursor string, paged bool) error {
	if err := w.Validate(data); err != nil {
		return err
	}
	switch w.Format {
	case FormatText:
		return w.renderText(data)
	case FormatRaw:
		return w.encode(data)
	case FormatJSON:
		payload, err := w.project(data)
		if err != nil {
			return err
		}
		env := Envelope{OK: true, V: errx.EnvelopeVersion, Data: payload}
		rows, collection, _ := asRows(data)
		if collection || paged || w.profile != "" || w.site != "" || nextCursor != "" {
			env.Meta = &Meta{Profile: w.profile, Site: w.site, NextCursor: nextCursor}
		}
		if collection || paged {
			count := len(rows)
			env.Meta.Count = &count
			env.Meta.Truncated = boolPtr(truncated)
		}
		return w.encode(env)
	default:
		return errx.Internal("unsupported output format %q", w.Format)
	}
}

// Failure writes one error envelope and returns the process exit status.
func (w *Writer) Failure(err error) errx.Code {
	if err == nil {
		err = errx.Internal("attempted to render a nil failure")
	}
	code := errx.ExitCode(err)
	body := &ErrorBody{Code: "INTERNAL", Message: err.Error()}
	hint := "this is a bug in jira-cli; do not retry unchanged"

	var typed *errx.Error
	if errors.As(err, &typed) {
		body.Code = typed.Reason
		body.Message = typed.Message
		body.Candidates = typed.Candidates
		body.DidYouMean = typed.DidYouMean
		hint = typed.Hint
		if typed.RetryAfter > 0 {
			body.RetryAfter = typed.RetryAfter.String()
		}
	}

	if w.Format == FormatText {
		_, _ = fmt.Fprintf(w.Err, "error: %s\n", body.Message)
		if hint != "" {
			_, _ = fmt.Fprintf(w.Err, "hint: %s\n", hint)
		}
		for _, candidate := range append(body.Candidates, body.DidYouMean...) {
			_, _ = fmt.Fprintf(w.Err, "  - %s (%s)\n", candidate.Name, candidate.ID)
		}
		return code
	}

	env := Envelope{OK: false, V: errx.EnvelopeVersion, Error: body, Hint: hint}
	if encodeErr := w.encode(env); encodeErr != nil {
		_, _ = fmt.Fprintf(w.Err, "error: %s\n", body.Message)
		_, _ = fmt.Fprintf(w.Err, "error: could not encode error envelope: %v\n", encodeErr)
	}
	return code
}

func (w *Writer) encode(value any) error {
	if err := json.NewEncoder(w.Out).Encode(value); err != nil {
		return errx.Internal("encode output: %v", err)
	}
	return nil
}

func (w *Writer) project(data any) (any, error) {
	rows, collection, ok := asRows(data)
	if !ok {
		if len(w.Fields) > 0 {
			return nil, errx.Usage("--fields is not supported for this command")
		}
		return data, nil
	}

	projected := make([]map[string]any, 0, len(rows))
	if len(rows) == 0 && len(w.Fields) > 0 {
		if schema := collectionSchema(data); schema != nil {
			if _, err := selectFields(schema.Fields(), w.Fields); err != nil {
				return nil, err
			}
		}
	}
	for _, row := range rows {
		selected, err := selectFields(row.Fields(), w.Fields)
		if err != nil {
			return nil, err
		}
		item := make(map[string]any, len(selected))
		for _, field := range selected {
			item[field.Name] = field.Raw
		}
		projected = append(projected, item)
	}
	if !collection && len(projected) == 1 {
		return projected[0], nil
	}
	return projected, nil
}

func (w *Writer) renderText(data any) error {
	rows, _, ok := asRows(data)
	if !ok {
		if len(w.Fields) > 0 {
			return errx.Usage("--fields is not supported for this command")
		}
		return w.encode(data)
	}
	if len(rows) == 0 && len(w.Fields) > 0 {
		if schema := collectionSchema(data); schema != nil {
			if _, err := selectFields(schema.Fields(), w.Fields); err != nil {
				return err
			}
		}
	}
	for _, row := range rows {
		fields, err := selectFields(row.Fields(), w.Fields)
		if err != nil {
			return err
		}
		parts := make([]string, 0, len(fields))
		for _, field := range fields {
			if len(w.Fields) > 0 {
				parts = append(parts, textCell(field))
			} else if field.Value != "" {
				parts = append(parts, field.Value)
			}
		}
		if _, err := fmt.Fprintln(w.Out, strings.Join(parts, "  ")); err != nil {
			return errx.Internal("write output: %v", err)
		}
	}
	return nil
}

func selectFields(available []Field, wanted []string) ([]Field, error) {
	if len(wanted) == 0 {
		selected := make([]Field, 0, len(available))
		for _, field := range available {
			if !field.OnRequest {
				selected = append(selected, field)
			}
		}
		return selected, nil
	}
	selected := make([]Field, 0, len(wanted))
	for _, name := range wanted {
		found := false
		for _, field := range available {
			if field.Name == name {
				selected = append(selected, field)
				found = true
				break
			}
		}
		if !found {
			return nil, errx.Usage("unknown field %q: available are %s", name, fieldNames(available))
		}
	}
	return selected, nil
}

func textCell(field Field) string {
	if field.Value != "" {
		return singleLine(field.Value)
	}
	if field.Raw == nil {
		return ""
	}
	value := reflect.ValueOf(field.Raw)
	switch value.Kind() {
	case reflect.Map, reflect.Slice:
		if value.Len() == 0 {
			return ""
		}
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return ""
		}
	}
	encoded, err := json.Marshal(field.Raw)
	if err != nil {
		return singleLine(fmt.Sprintf("%v", field.Raw))
	}
	if raw, ok := field.Raw.(string); ok {
		return singleLine(raw)
	}
	return singleLine(string(encoded))
}

func singleLine(value string) string { return strings.Join(strings.Fields(value), " ") }

var renderableType = reflect.TypeOf((*Renderable)(nil)).Elem()

func asRows(data any) (rows []Renderable, collection, ok bool) {
	if data == nil {
		return nil, false, false
	}
	if rendered, valid := data.(RenderableCollection); valid {
		return rendered.RenderRows(), true, true
	}
	value := reflect.ValueOf(data)
	if value.Kind() == reflect.Slice {
		if !value.Type().Elem().Implements(renderableType) {
			return nil, false, false
		}
		rows = make([]Renderable, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			row, valid := value.Index(i).Interface().(Renderable)
			if !valid || reflect.ValueOf(row).Kind() == reflect.Pointer && reflect.ValueOf(row).IsNil() {
				return nil, false, false
			}
			rows = append(rows, row)
		}
		return rows, true, true
	}
	if row, valid := data.(Renderable); valid {
		return []Renderable{row}, false, true
	}
	return nil, false, false
}

func collectionSchema(data any) Renderable {
	if rendered, valid := data.(RenderableCollection); valid {
		return staticFields(rendered.SchemaFields())
	}
	value := reflect.ValueOf(data)
	if value.Kind() != reflect.Slice {
		return nil
	}
	element := value.Type().Elem()
	if element.Kind() == reflect.Pointer {
		candidate, _ := reflect.New(element.Elem()).Interface().(Renderable)
		return candidate
	}
	candidate, _ := reflect.Zero(element).Interface().(Renderable)
	return candidate
}

type staticFields []Field

func (fields staticFields) Fields() []Field { return fields }

func fieldNames(fields []Field) string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	return strings.Join(names, ", ")
}

func boolPtr(value bool) *bool { return &value }
