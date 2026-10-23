// Package discover finds other Mindctl gateways on the local network.
// It does not change routing.
package discover

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const serviceName = "_ai-gateway._tcp.local"

// Gateway is one advertised listener.
type Gateway struct {
	Name, Host string
	Port       int
}

// Query asks the LAN for _ai-gateway._tcp advertisements.
func Query(ctx context.Context) ([]Gateway, error) {
	payload := question()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	destination := &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353}
	if _, err := conn.WriteToUDP(payload, destination); err != nil {
		return nil, err
	}
	buffer := make([]byte, 64<<10)
	var packets [][]byte
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if len(packets) == 0 && !errors.Is(err, context.DeadlineExceeded) {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					return []Gateway{}, nil
				}
			}
			break
		}
		copied := make([]byte, n)
		copy(copied, buffer[:n])
		packets = append(packets, copied)
	}
	return Parse(packets), nil
}

// Parse reads mDNS responses into gateway advertisements.
func Parse(packets [][]byte) []Gateway {
	var found []Gateway
	seen := map[string]struct{}{}
	for _, packet := range packets {
		for _, gateway := range parsePacket(packet) {
			key := gateway.Name + "\x00" + gateway.Host
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			found = append(found, gateway)
		}
	}
	if found == nil {
		return []Gateway{}
	}
	return found
}

func question() []byte {
	name := encodeName(serviceName)
	packet := make([]byte, 12+len(name)+4)
	binary.BigEndian.PutUint16(packet[4:], 1)
	copy(packet[12:], name)
	binary.BigEndian.PutUint16(packet[12+len(name):], 12)
	binary.BigEndian.PutUint16(packet[12+len(name)+2:], 1)
	return packet
}

func parsePacket(packet []byte) []Gateway {
	if len(packet) < 12 {
		return nil
	}
	questions := int(binary.BigEndian.Uint16(packet[4:6]))
	answers := int(binary.BigEndian.Uint16(packet[6:8]))
	offset := 12
	for index := 0; index < questions && offset < len(packet); index++ {
		_, next, ok := readName(packet, offset)
		if !ok || next+4 > len(packet) {
			return nil
		}
		offset = next + 4
	}
	var found []Gateway
	for index := 0; index < answers && offset < len(packet); index++ {
		_, next, ok := readName(packet, offset)
		if !ok || next+10 > len(packet) {
			return found
		}
		kind := binary.BigEndian.Uint16(packet[next:])
		rdlen := int(binary.BigEndian.Uint16(packet[next+8:]))
		dataAt := next + 10
		if dataAt+rdlen > len(packet) {
			return found
		}
		if kind == 33 && rdlen >= 6 {
			port := int(binary.BigEndian.Uint16(packet[dataAt+4:]))
			target, _, ok := readName(packet, dataAt+6)
			if ok && port > 0 {
				found = append(found, Gateway{Name: strings.TrimSuffix(target, ".local"), Host: target, Port: port})
			}
		}
		offset = dataAt + rdlen
	}
	return found
}

func encodeName(name string) []byte {
	var out []byte
	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}

func readName(packet []byte, offset int) (string, int, bool) {
	var labels []string
	jumped := false
	next := offset
	for hops := 0; hops < 16; hops++ {
		if offset >= len(packet) {
			return "", 0, false
		}
		length := int(packet[offset])
		if length == 0 {
			if !jumped {
				next = offset + 1
			}
			return strings.Join(labels, "."), next, true
		}
		if length&0xC0 == 0xC0 {
			if offset+1 >= len(packet) {
				return "", 0, false
			}
			pointer := int(binary.BigEndian.Uint16(packet[offset:]) & 0x3FFF)
			if !jumped {
				next = offset + 2
				jumped = true
			}
			offset = pointer
			continue
		}
		offset++
		if offset+length > len(packet) {
			return "", 0, false
		}
		labels = append(labels, string(packet[offset:offset+length]))
		offset += length
		if !jumped {
			next = offset
		}
	}
	return "", 0, false
}

// Format renders one gateway for the CLI.
func Format(gateway Gateway) string {
	return fmt.Sprintf("%s\t%s\t%d", gateway.Name, gateway.Host, gateway.Port)
}
