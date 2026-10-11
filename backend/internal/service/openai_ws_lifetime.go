package service

import (
	"encoding/json"
	"time"

	"github.com/tidwall/gjson"
)

// Renew between turns, leaving headroom for long reasoning/tool generations.
// The upstream's hard 60-minute limit still applies to any single connection.
const openAIWSConnRenewAfter = 30 * time.Minute

// Never move a store=false continuation unless this session owns its complete
// history. A tool result alone or an external response reference is insufficient.
func prepareOpenAIWSLifetimeRotationPayload(payload []byte, input []json.RawMessage, inputExists, historyComplete, storeDisabled bool) ([]byte, bool, error) {
	if gjson.GetBytes(payload, "conversation").Exists() && gjson.GetBytes(payload, "conversation").Type != gjson.Null {
		return payload, false, nil
	}
	if !storeDisabled {
		// Persisted state can be hydrated on the replacement connection.
		return payload, true, nil
	}
	if !historyComplete {
		return payload, false, nil
	}
	for _, item := range input {
		if gjson.GetBytes(item, "type").String() == "item_reference" {
			return payload, false, nil
		}
	}
	if openAIWSRawItemsHasFunctionCallOutput(input) && !openAIWSRawItemsHaveToolCallContextForOutputs(input) {
		return payload, false, nil
	}
	updated, _, err := dropPreviousResponseIDFromRawPayload(payload)
	if err != nil {
		return nil, false, err
	}
	updated, err = setOpenAIWSPayloadInputSequence(updated, input, inputExists)
	return updated, err == nil, err
}
