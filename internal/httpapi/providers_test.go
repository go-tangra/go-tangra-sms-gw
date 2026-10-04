package httpapi

import (
	"testing"

	_ "github.com/go-tangra/go-tangra-sms-gw/v4/internal/provider/voicecom"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/sealed"
)

func TestProviderConfigRedactionAndMerge(t *testing.T) {
	tok, dlrTok := "carrier-token-123456", "0123456789abcdef0123456789abcdef"
	stored := sealed.Config{"url": "https://c.example/send", "sid": "9", "encoding": "utf-8", "token": tok, "dlr_token": dlrTok,
		"callback_url": "https://edge.example/dlr?dlr_token=" + dlrTok}
	shown := redactConfig("voicecom", stored)
	if shown["token"] != sealed.Marker || shown["dlr_token"] != sealed.Marker || shown["callback_url"] != "https://edge.example/dlr?dlr_token="+sealed.Marker {
		t.Fatalf("%v", shown)
	}
	// Sent back unchanged: every stored value survives.
	back := mergeConfig("voicecom", stored, shown)
	for k, v := range stored {
		if back[k] != v {
			t.Fatalf("%s: %q", k, back[k])
		}
	}
	// A changed field carrying the marker is refused; "" clears a credential.
	edited := map[string]string{}
	for k, v := range shown {
		edited[k] = v
	}
	edited["callback_url"] = "https://other.example/dlr?dlr_token=" + sealed.Marker
	edited["token"] = ""
	merged := mergeConfig("voicecom", stored, edited)
	if _, ok := merged["token"]; ok {
		t.Fatal("cleared credential kept")
	}
	if err := checkConfig("voicecom", merged); err == nil {
		t.Fatal("marker in a changed field accepted")
	}
	if err := checkConfig("voicecom", back); err != nil {
		t.Fatal(err)
	}
}
