package lsp

import (
	"encoding/json"

	"github.com/infracost/go-proto/pkg/rat"
)

func mustRat(s string) *rat.Rat {
	r, err := rat.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return r
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
