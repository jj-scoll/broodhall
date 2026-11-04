package main

// Order-preserving JSON for redact: result rows keep their column order, which a Go map
// would sort away. Numbers stay json.Number so they re-encode exactly.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

type ordObject struct {
	keys []string
	vals []any
}

func (o *ordObject) get(k string) any {
	for i, kk := range o.keys {
		if kk == k {
			return o.vals[i]
		}
	}
	return nil
}

func (o *ordObject) set(k string, v any) {
	for i, kk := range o.keys {
		if kk == k {
			o.vals[i] = v
			return
		}
	}
	o.keys, o.vals = append(o.keys, k), append(o.vals, v)
}

// decodeOrderedStream parses one or more whitespace-separated JSON documents; anything that
// is not JSON is an error. Empty input is an empty stream.
func decodeOrderedStream(in []byte) ([]any, error) {
	d := json.NewDecoder(bytes.NewReader(in))
	d.UseNumber()
	var docs []any
	for {
		v, err := decodeOrdValue(d)
		if err == io.EOF {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if _, isDelim := v.(json.Delim); isDelim {
			return nil, errors.New("unbalanced JSON")
		}
		docs = append(docs, v)
	}
}

func decodeOrdValue(d *json.Decoder) (any, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := &ordObject{}
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeOrdValue(d)
			if err != nil {
				return nil, err
			}
			o.keys, o.vals = append(o.keys, kt.(string)), append(o.vals, v)
		}
		_, err := d.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decodeOrdValue(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := d.Token()
		return a, err
	}
	return tok, nil
}

func encodeOrdered(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case *ordObject:
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeScalar(b, k)
			b.WriteByte(':')
			encodeOrdered(b, t.vals[i])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeOrdered(b, e)
		}
		b.WriteByte(']')
	default:
		encodeScalar(b, t)
	}
}

func encodeScalar(b *bytes.Buffer, v any) {
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	b.Write(bytes.TrimRight(sb.Bytes(), "\n"))
}
