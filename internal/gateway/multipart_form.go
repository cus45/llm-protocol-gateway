package gateway

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"
)

// multipartFormField reads one plain (non-file) field out of a multipart form
// without decoding the rest: the audio part can be megabytes and is relayed as
// sent, never buffered into a second copy.
func multipartFormField(contentType string, body []byte, field string) (string, error) {
	reader, err := newMultipartFormReader(contentType, body)
	if err != nil {
		return "", err
	}
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", errors.New("the multipart form does not parse: " + err.Error())
		}
		if part.FormName() != field || part.FileName() != "" {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, 1<<20))
		if err != nil {
			return "", errors.New("the multipart form does not parse: " + err.Error())
		}
		return strings.TrimSpace(string(value)), nil
	}
}

func newMultipartFormReader(contentType string, body []byte) (*multipart.Reader, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, errors.New("send the audio as multipart/form-data, as OpenAI does: a file and a model")
	}
	return multipart.NewReader(bytes.NewReader(body), params["boundary"]), nil
}

// multipartFormWithField returns the form with one plain field replaced, under
// the same boundary and with every other part byte for byte. The audio upload
// is only re-encoded when the model actually has to change — a provider with no
// model mapping uploads exactly the bytes the client sent.
func multipartFormWithField(contentType string, body []byte, field, value string) ([]byte, error) {
	reader, err := newMultipartFormReader(contentType, body)
	if err != nil {
		return nil, err
	}
	_, params, _ := mime.ParseMediaType(contentType)

	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		return nil, err
	}
	replaced := false
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("the multipart form does not parse: " + err.Error())
		}
		writer2, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, err
		}
		if part.FormName() == field && part.FileName() == "" {
			if _, err := io.WriteString(writer2, value); err != nil {
				return nil, err
			}
			replaced = true
			continue
		}
		if _, err := io.Copy(writer2, part); err != nil {
			return nil, err
		}
	}
	if !replaced {
		// No model field came with the form: leave it alone rather than adding
		// one the client did not send.
		return body, nil
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
