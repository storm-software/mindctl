package inference

import "encoding/json"

// Event is one provider-neutral streaming event.
type Event struct {
	Type                                                                     string
	ResponseID, ProviderRequestID, ItemID, ItemType, CallID, Name, Namespace string
	Role, Delta, ItemText, Input                                             string
	Thinking, Signature, StopReason                                          string
	ArgumentsDelta                                                           string
	Summary, EncryptedContent                                                json.RawMessage
	OutputIndex                                                              int
	Status                                                                   string
	Usage                                                                    Usage
	Data                                                                     json.RawMessage
}
