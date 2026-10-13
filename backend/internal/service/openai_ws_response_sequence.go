package service

import (
	"math"
	"strconv"

	"github.com/tidwall/gjson"
)

// Responses sequences are integers on the wire. Parse their original JSON
// number instead of a float conversion, which rounds large fractions and can
// silently saturate an overflowing integer.
func parseOpenAIWSResponseSequence(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	sequence, err := strconv.ParseInt(value.Raw, 10, 64)
	return sequence, err == nil && sequence >= 0
}

func openAIWSResponseSequenceSuccessor(last int64, fallback int) (int64, bool) {
	if last == math.MaxInt64 {
		return 0, false
	}
	next := last + 1
	if next < int64(fallback) {
		next = int64(fallback)
	}
	if next < 0 {
		next = 0
	}
	return next, true
}

// There is no lawful int64 successor after the largest sequence. An explicit
// unsequenced protocol error terminates the connection without inventing a
// lower Responses sequence or retaining provider-controlled content.
func buildOpenAIWSResponseSequenceErrorEvent() []byte {
	return []byte(`{"type":"error","status":502,"error":{"type":"upstream_error","code":"upstream_protocol_error","message":"Upstream response sequence is invalid or exhausted"}}`)
}
