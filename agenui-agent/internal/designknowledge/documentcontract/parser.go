package documentcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

func Parse(raw []byte) (Document, error) {
	start := bytes.Index(raw, []byte(markerStart))
	if start < 0 {
		return Document{}, errors.New("document contract marker is missing")
	}
	start += len(markerStart)
	end := bytes.Index(raw[start:], []byte(markerEnd))
	if end < 0 {
		return Document{}, errors.New("document contract marker is not closed")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw[start : start+end]))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode document contract: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) == nil {
		return Document{}, errors.New("document contract contains multiple JSON values")
	}
	document.Normalize()
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

func RoundTrip(document Document) ([]byte, error) {
	raw, err := Render(document)
	if err != nil {
		return nil, err
	}
	parsed, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	left, _ := json.Marshal(documentNormalized(document))
	right, _ := json.Marshal(parsed)
	if !bytes.Equal(left, right) {
		return nil, errors.New("render/parse AST mismatch")
	}
	return raw, nil
}

func documentNormalized(document Document) Document { document.Normalize(); return document }
