package discover

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseSRVAdvertisement(t *testing.T) {
	name := encodeName("gateway._ai-gateway._tcp.local")
	target := encodeName("gateway.local")
	var packet bytes.Buffer
	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[6:], 1)
	packet.Write(header)
	packet.Write(name)
	meta := make([]byte, 10)
	binary.BigEndian.PutUint16(meta[0:], 33)
	binary.BigEndian.PutUint16(meta[2:], 1)
	binary.BigEndian.PutUint16(meta[8:], uint16(6+len(target)))
	packet.Write(meta)
	priority := make([]byte, 6)
	binary.BigEndian.PutUint16(priority[4:], 8080)
	packet.Write(priority)
	packet.Write(target)
	found := Parse([][]byte{packet.Bytes()})
	if len(found) != 1 || found[0].Port != 8080 || found[0].Host != "gateway.local" {
		t.Fatalf("found=%+v", found)
	}
}
