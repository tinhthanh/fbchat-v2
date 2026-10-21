package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestHelloPayloadDeclaresCompatibleContract(t *testing.T) {
	payload := helloPayload()

	if got := payload["protocolVersion"]; got != bridgeProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %d", got, bridgeProtocolVersion)
	}
	if got := payload["bridgeVersion"]; got != bridgeVersion {
		t.Fatalf("bridgeVersion = %v, want %q", got, bridgeVersion)
	}

	capabilities, ok := payload["capabilities"].([]string)
	if !ok {
		t.Fatalf("capabilities has type %T, want []string", payload["capabilities"])
	}
	for _, required := range []string{"newClient", "connect", "connectE2EE", "isConnected", "events", "getCookies"} {
		if !slices.Contains(capabilities, required) {
			t.Errorf("capabilities does not contain %q", required)
		}
	}
}

func TestServeRequestsRejectsOversizedLine(t *testing.T) {
	input := strings.NewReader(strings.Repeat("x", 65) + "\n")

	err := serveRequests(input, 64)

	if err == nil {
		t.Fatal("serveRequests() accepted a request larger than its hard limit")
	}
}

func TestGetCookiesWithoutClientReturnsEmptyMap(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	handle(&request{ID: 7, Method: "getCookies"})
	os.Stdout = stdout
	w.Close()
	var resp struct {
		ID   uint64 `json:"id"`
		OK   bool   `json:"ok"`
		Data struct {
			Cookies map[string]string `json:"cookies"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 7 || !resp.OK || resp.Data.Cookies == nil || len(resp.Data.Cookies) != 0 {
		t.Fatalf("getCookies without client = %+v", resp)
	}
}
