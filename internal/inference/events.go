package inference

import "encoding/json"

// Event is one provider-neutral streaming event.
type Event struct {
	Type                                                       string
	ResponseID, ProviderRequestID, ItemID, CallID, Name, Delta string
	ArgumentsDelta                                             string
	Status                                                     string
	Usage                                                      Usage
	Data                                                       json.RawMessage
}
