package cache

import "encoding/json"

// Codec serialises facade values for L2 (Redis). L1 stores values as
// `[]byte` after the same encode pass, so L1 and L2 stay in lockstep
// — no L1 hits returning a struct that L2 round-trips would mangle.
//
// Default is [JSONCodec]. Hot paths can swap to msgpack via a custom
// implementation.
type Codec interface {
	Encode(v any) ([]byte, error)
	Decode(data []byte, v any) error
}

// JSONCodec uses encoding/json. Stable, debuggable in Redis CLI, fine
// for the typical SaaS hot path.
type JSONCodec struct{}

// Encode serialises v as JSON.
func (JSONCodec) Encode(v any) ([]byte, error) { return json.Marshal(v) }

// Decode reads JSON into v.
func (JSONCodec) Decode(data []byte, v any) error { return json.Unmarshal(data, v) }
