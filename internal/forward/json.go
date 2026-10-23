package forward

import (
	"bytes"
	"encoding/json"
)

func newDecoder(body []byte) *json.Decoder {
	return json.NewDecoder(bytes.NewReader(body))
}
