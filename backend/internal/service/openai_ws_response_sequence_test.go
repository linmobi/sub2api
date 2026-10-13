package service

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSResponseSequence_ExactInteger(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		value int64
		valid bool
	}{
		{"0", 0, true}, {"9007199254740993", 9007199254740993, true},
		{"9223372036854775806", math.MaxInt64 - 1, true}, {"9223372036854775807", math.MaxInt64, true},
		{"9223372036854775808", 0, false}, {"9007199254740992.5", 0, false},
		{"-1", 0, false}, {"0.5", 0, false}, {"1e0", 0, false}, {`"2"`, 0, false},
		{"null", 0, false}, {"true", 0, false}, {"{}", 0, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			value, valid := parseOpenAIWSResponseSequence(gjson.Parse(tc.raw))
			require.Equal(t, tc.valid, valid)
			if valid {
				require.Equal(t, tc.value, value)
			}
		})
	}
}

func TestOpenAIWSResponseSequence_CheckedSuccessor(t *testing.T) {
	value, valid := openAIWSResponseSequenceSuccessor(math.MaxInt64-1, 1)
	require.True(t, valid)
	require.Equal(t, int64(math.MaxInt64), value)
	_, valid = openAIWSResponseSequenceSuccessor(math.MaxInt64, 1)
	require.False(t, valid)
	value, valid = openAIWSResponseSequenceSuccessor(-1, 3)
	require.True(t, valid)
	require.Equal(t, int64(3), value)

	event := buildOpenAIWSResponseSequenceErrorEvent()
	require.Equal(t, "error", gjson.GetBytes(event, "type").String())
	require.Equal(t, "upstream_protocol_error", gjson.GetBytes(event, "error.code").String())
	require.False(t, gjson.GetBytes(event, "sequence_number").Exists())
	require.False(t, gjson.GetBytes(event, "usage").Exists())
}
