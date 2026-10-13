package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSNonstreamKeepsCompletedItemsWhenTerminalOutputIsEmpty(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_done","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"synthetic answer","annotations":[]}]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_empty_terminal","object":"response","status":"completed","model":"gpt-6-astra","output":[],"usage":{"input_tokens":9,"output_tokens":4}}}`),
	}}
	svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":false,"input":"synthetic input"}`))
	require.NoError(t, err)
	require.Equal(t, "synthetic answer", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
	require.Equal(t, "msg_done", gjson.Get(rec.Body.String(), "output.0.id").String())
	require.Equal(t, "resp_empty_terminal", result.ResponseID)
	require.Equal(t, 4, result.Usage.OutputTokens)
}

func TestOpenAIWSNonstreamOutputRestorationDoesNotOverwriteOrInvent(t *testing.T) {
	for _, tc := range []struct {
		name, terminal, want string
		done                 bool
	}{
		{"populated_terminal", `{"type":"response.completed","response":{"id":"resp_full","status":"completed","output":[{"id":"msg_terminal","type":"message","content":[{"type":"output_text","text":"terminal answer"}]}]}}`, "terminal answer", true},
		{"no_items", `{"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[]}}`, "", false},
		{"failed_terminal", `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"server_error","message":"synthetic failure"},"output":[]}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := [][]byte{}
			if tc.done {
				events = append(events, []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_event","type":"message","content":[{"type":"output_text","text":"event answer"}]}}`))
			}
			events = append(events, []byte(tc.terminal))
			svc, account, c, rec, _ := astraHTTPBridgeFixture(t, &openAIWSCaptureConn{events: events})
			_, _ = svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":false,"input":"synthetic input"}`))
			require.Equal(t, tc.want, gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
		})
	}
}

func TestOpenAIWSNonstreamOutputRestorationUsesIndexesAndLatestDone(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_second","type":"message","phase":"final_answer","content":[{"type":"output_text","text":"second"}],"vendor_field":{"opaque":"preserved"}}}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_first","type":"message","content":[{"type":"output_text","text":"old first"}]}}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_first","type":"message","content":[{"type":"output_text","text":"final first"}]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_ordered","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4}}}`),
	}}
	svc, account, c, rec, _ := astraHTTPBridgeFixture(t, conn)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":false,"input":"synthetic input"}`))
	require.NoError(t, err)
	output := gjson.Get(rec.Body.String(), "output").Array()
	require.Len(t, output, 2)
	require.Equal(t, "msg_first", output[0].Get("id").String())
	require.Equal(t, "final first", output[0].Get("content.0.text").String())
	require.Equal(t, "msg_second", output[1].Get("id").String())
	require.Equal(t, "preserved", output[1].Get("vendor_field.opaque").String())
	require.Equal(t, "final_answer", output[1].Get("phase").String())
	require.Equal(t, "resp_ordered", result.ResponseID)
	require.Equal(t, 4, result.Usage.OutputTokens)
}

func TestOpenAIWSNonstreamOutputItemsBoundCurrentItems(t *testing.T) {
	first := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_first","type":"message","content":[{"type":"output_text","text":"first"}]}}`)
	shorter := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"replacement","type":"message"}}`)
	second := []byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_second","type":"message"}}`)
	limit := len(first) + 8
	items := newOpenAIWSNonstreamOutputItems(limit)
	_, ok := items.BuildOutput()
	require.False(t, ok)
	require.NoError(t, items.Observe(first), "the exact bound is accepted")
	require.NoError(t, items.Observe(first), "repeating the same index does not grow the buffer")
	require.Equal(t, limit, items.bufferedBytes)
	require.NoError(t, items.Observe(shorter), "replacing an index releases its previous byte charge")
	require.Equal(t, len(shorter)+8, items.bufferedBytes)
	require.ErrorIs(t, items.Observe(second), errOpenAIWSNonstreamOutputLimit, "distinct items share the aggregate bound")
	oversized := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","extra":"` + strings.Repeat("x", limit) + `"}}`)
	require.ErrorIs(t, items.Observe(oversized), errOpenAIWSNonstreamOutputLimit, "a replacement is bounded before retention")
	output, ok := items.BuildOutput()
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(output).Array(), 1)
	require.Equal(t, "replacement", gjson.GetBytes(output, "0.id").String(), "rejected observations do not mutate the retained output")
}
