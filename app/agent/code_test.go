package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestSessionCode(t *testing.T) {
	secret := "00112233445566778899aabbccddeeff"
	code, err := EncodeCode([]string{"192.168.1.20", "100.64.0.7", "not an ip", "fe80::1"}, 8787, "", secret)
	if err != nil || !strings.HasPrefix(code, "HEN") {
		t.Fatalf("encode: %v %q", err, code)
	}
	addresses, got, err := DecodeCode(code)
	if err != nil || got != secret || !reflect.DeepEqual(addresses, []string{"http://192.168.1.20:8787", "http://100.64.0.7:8787"}) {
		t.Fatalf("decode: %v %q %v", err, got, addresses)
	}
	// Pasted with spaces, line breaks or in lower case, it still works.
	messy := "  " + strings.ToLower(code[:10]) + " \n" + code[10:] + "\t"
	if a, s, err := DecodeCode(messy); err != nil || s != secret || len(a) != 2 {
		t.Fatalf("decode of a messy paste: %v", err)
	}
	// The host's own address comes first, in whatever form it was typed.
	for extra, want := range map[string]string{
		"vadym.example.com":         "http://vadym.example.com:8787",
		"203.0.113.9":               "http://203.0.113.9:8787",
		"203.0.113.9:9000":          "http://203.0.113.9:9000",
		"https://henosis.example/":  "https://henosis.example",
		"http://host.example:1234/": "http://host.example:1234",
	} {
		code, err := EncodeCode([]string{"192.168.1.20"}, 8787, extra, secret)
		if err != nil {
			t.Fatal(err)
		}
		addresses, _, err := DecodeCode(code)
		if err != nil || len(addresses) != 2 || addresses[0] != want || addresses[1] != "http://192.168.1.20:8787" {
			t.Errorf("extra %q: %v %v", extra, err, addresses)
		}
	}
	// Damaged codes are refused with a reason, never half understood.
	for name, bad := range map[string]string{
		"empty":           "",
		"not a code":      "hello there",
		"an invite code":  "HN-ABCD-EFGH-JKLM",
		"cut off":         code[:len(code)-6],
		"one letter off":  code[:20] + flip(code[20]) + code[21:],
		"only the prefix": "HEN",
	} {
		if _, _, err := DecodeCode(bad); err == nil {
			t.Errorf("%s (%q) was accepted", name, bad)
		}
	}
	if _, err := EncodeCode(nil, 8787, "", "abc"); err == nil {
		t.Error("a short secret was accepted")
	}
	if _, _, err := DecodeCode(mustEncode(t, nil, 8787, "", secret)); err == nil {
		t.Error("a code without any address was accepted")
	}
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

func mustEncode(t *testing.T, ips []string, port int, extra, secret string) string {
	t.Helper()
	code, err := EncodeCode(ips, port, extra, secret)
	if err != nil {
		t.Fatal(err)
	}
	return code
}
