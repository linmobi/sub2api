package service

import (
	"errors"

	"github.com/tidwall/gjson"
)

var errOpenAIWSNonstreamOutputLimit = errors.New("upstream nonstream output exceeds buffer limit")

// A WS response may finish output items in a different order from their output
// indexes. Keep the latest complete item at each index, with a request-local
// bound. Counting its whole done frame also bounds overhead from many small
// items, while replacements release the previous frame's charge.
type openAIWSNonstreamOutputItems struct {
	items         *responsesStreamOutputItems
	frameBytes    map[int]int
	bufferedBytes int
	limitBytes    int
}

func newOpenAIWSNonstreamOutputItems(limitBytes int) *openAIWSNonstreamOutputItems {
	return &openAIWSNonstreamOutputItems{
		items:      newResponsesStreamOutputItems(),
		frameBytes: make(map[int]int),
		limitBytes: limitBytes,
	}
}

func (r *openAIWSNonstreamOutputItems) Observe(message []byte) error {
	if r == nil || !gjson.ValidBytes(message) || gjson.GetBytes(message, "type").String() != "response.output_item.done" {
		return nil
	}
	item := gjson.GetBytes(message, "item")
	if !item.IsObject() {
		return nil
	}
	index := int(gjson.GetBytes(message, "output_index").Int())
	frameBytes := len(message) + 8
	nextBytes := r.bufferedBytes - r.frameBytes[index] + frameBytes
	if nextBytes > r.limitBytes {
		return errOpenAIWSNonstreamOutputLimit
	}
	r.items.Observe(message)
	r.frameBytes[index] = frameBytes
	r.bufferedBytes = nextBytes
	return nil
}

func (r *openAIWSNonstreamOutputItems) BuildOutput() ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	return r.items.BuildOutput()
}
