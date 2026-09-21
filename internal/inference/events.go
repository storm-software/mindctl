package inference

import "encoding/json"

// Event is one provider-neutral streaming event.
type Event struct {
	Type                                    string
	ResponseID, ItemID, CallID, Name, Delta string
	ArgumentsDelta                          string
	Data                                    json.RawMessage
}
