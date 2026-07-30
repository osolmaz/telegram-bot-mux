package httpx

import (
	"net/http"
	"testing"
)

func TestEndToEndHeaders(t *testing.T) {
	source := http.Header{"Connection": {"close"}, "Host": {"example.test"}, "X-Test": {"one", "two"}}
	cloned := CloneEndToEndHeaders(source)
	if cloned.Get("Connection") != "" || cloned.Get("Host") != "" || cloned.Get("X-Test") != "one" {
		t.Fatalf("cloned headers = %v", cloned)
	}
	cloned["X-Test"][0] = "changed"
	if source.Get("X-Test") != "one" {
		t.Fatal("header values were not cloned")
	}
	if IsHopByHopHeader("X-Test") || !IsHopByHopHeader("Upgrade") {
		t.Fatal("hop-by-hop classification failed")
	}
}
