package inference

import "encoding/json"

// Event is one provider-neutral streaming event.
type Event struct {
	Type                                                                     string
	ResponseID, ProviderRequestID, ItemID, ItemType, CallID, Name, Namespace string
	Delta, Input                                                             string
	ArgumentsDelta                                                           string
	OutputIndex                                                              int
	Status                                                                   string
	Usage                                                                    Usage
	Data                                                                     json.RawMessage
}
