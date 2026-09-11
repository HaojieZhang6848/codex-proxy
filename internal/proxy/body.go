package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errModelFieldType is returned when "model" is present but not a string.
var errModelFieldType = errors.New(`the "model" field of the request body must be a string`)

// modelField describes the top-level "model" member of a JSON object body.
type modelField struct {
	Name  string
	Found bool
	start int64
	end   int64
}

// readBody buffers the request body. It is needed because the model name may
// have to be rewritten before the request is forwarded.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	var reader io.Reader = r.Body
	if limit > 0 {
		reader = http.MaxBytesReader(w, r.Body, limit)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, fmt.Errorf("the request body exceeds the %d byte limit", tooLarge.Limit)
		}
		return nil, err
	}
	return body, nil
}

// modelNameOf returns the model requested by the client, if the body is a JSON
// object carrying a top-level "model" field.
func modelNameOf(body []byte) (string, bool) {
	field, err := locateModelField(body)
	if err != nil {
		return "", false
	}
	return field.Name, field.Found
}

// rewriteModel replaces the value of the top-level "model" field and leaves
// every other byte of the body untouched. If the field is absent, or already
// equals name, the original slice is returned unchanged.
func rewriteModel(body []byte, name string) ([]byte, bool, error) {
	field, err := locateModelField(body)
	if err != nil {
		return nil, false, err
	}
	if !field.Found || field.Name == name {
		return body, false, nil
	}
	quoted, err := json.Marshal(name)
	if err != nil {
		return nil, false, err
	}
	out := make([]byte, 0, len(body)+len(quoted))
	out = append(out, body[:field.start]...)
	out = append(out, quoted...)
	out = append(out, body[field.end:]...)
	return out, true, nil
}

// locateModelField scans the top level of a JSON object and records where the
// value of the "model" member starts and ends in the original byte slice.
func locateModelField(body []byte) (modelField, error) {
	var field modelField
	if len(bytes.TrimSpace(body)) == 0 {
		return field, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return field, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		// Not a JSON object: nothing to look at.
		return field, nil
	}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return field, err
		}
		if key, ok := keyToken.(string); !ok || key != "model" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return field, err
			}
			continue
		}
		start, ok := valueStart(body, dec.InputOffset())
		if !ok {
			return field, errModelFieldType
		}
		valueToken, err := dec.Token()
		if err != nil {
			return field, err
		}
		end := dec.InputOffset()
		switch v := valueToken.(type) {
		case string:
			field = modelField{Name: v, Found: true, start: start, end: end}
		case nil:
			field = modelField{Found: true, start: start, end: end}
		default:
			return field, errModelFieldType
		}
		return field, nil
	}
	return field, nil
}

// valueStart returns the offset of the value that follows the ":" of the member
// whose key ended at offset from.
func valueStart(body []byte, from int64) (int64, bool) {
	i := from
	for i < int64(len(body)) && isJSONSpace(body[i]) {
		i++
	}
	if i >= int64(len(body)) || body[i] != ':' {
		return 0, false
	}
	i++
	for i < int64(len(body)) && isJSONSpace(body[i]) {
		i++
	}
	if i >= int64(len(body)) {
		return 0, false
	}
	return i, true
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
