package dnsfilter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// HostAliases contains the Windows host address supplied by the authenticated
// controller. It is separate from user DNS policy so either can change safely.
type HostAliases struct {
	HostIPv4 netip.Addr `json:"hostIPv4"`
}

func dockerHostAnswer(query []byte, path string, gateway netip.Addr) ([]byte, bool, error) {
	var request dnsmessage.Message
	if err := request.Unpack(query); err != nil {
		return nil, false, err
	}
	if request.Response || len(request.Questions) != 1 {
		return nil, false, errors.New("DNS request must contain one query question")
	}
	question := request.Questions[0]
	name := strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))
	if name != "host.docker.internal" && name != "gateway.docker.internal" {
		return nil, false, nil
	}
	response := dnsmessage.Message{Header: dnsmessage.Header{ID: request.ID, Response: true, OpCode: request.OpCode, RecursionDesired: request.RecursionDesired, RecursionAvailable: true}, Questions: request.Questions}
	if question.Class != dnsmessage.ClassINET || (question.Type != dnsmessage.TypeA && question.Type != dnsmessage.TypeAAAA) {
		answer, err := response.Pack()
		return answer, true, err
	}
	ip := gateway
	if name == "host.docker.internal" {
		file, err := os.Open(path)
		if err != nil {
			return nil, true, fmt.Errorf("read Docker host alias configuration: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(file, 1025))
		file.Close()
		if err != nil {
			return nil, true, err
		}
		if len(data) > 1024 {
			return nil, true, errors.New("Docker host alias configuration exceeds 1 KiB")
		}
		var aliases HostAliases
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&aliases); err != nil {
			return nil, true, fmt.Errorf("decode Docker host alias configuration: %w", err)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, true, errors.New("Docker host alias configuration contains trailing JSON")
		}
		if !aliases.HostIPv4.Is4() || aliases.HostIPv4.IsUnspecified() || aliases.HostIPv4.IsLoopback() {
			return nil, true, errors.New("Docker host alias configuration has no usable host IPv4 address")
		}
		ip = aliases.HostIPv4
	}
	if !ip.IsValid() {
		return nil, true, errors.New("Docker network has no gateway for the host alias")
	}
	header := dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 5}
	if question.Type == dnsmessage.TypeA && ip.Is4() {
		response.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.AResource{A: ip.As4()}}}
	}
	if question.Type == dnsmessage.TypeAAAA && ip.Is6() {
		response.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.AAAAResource{AAAA: ip.As16()}}}
	}
	answer, err := response.Pack()
	return answer, true, err
}
