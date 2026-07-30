package routing

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/osolmaz/telegram-bot-mux/internal/config"
)

func TestParseUpdate(t *testing.T) {
	update, err := ParseUpdate(json.RawMessage(`{"update_id":42,"callback_query":{"id":"q","data":"bk:h:a:grant:token"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if update.ID != 42 || update.Type != "callback_query" || update.CallbackData != "bk:h:a:grant:token" {
		t.Fatalf("unexpected update: %#v", update)
	}
}

func TestParseUpdateRejectsInvalidShapes(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"message":{}}`,
		`{"update_id":-1,"message":{}}`,
		`{"update_id":1,"message":{},"callback_query":{}}`,
		`{"update_id":"one","message":{}}`,
	} {
		if _, err := ParseUpdate(json.RawMessage(raw)); err == nil {
			t.Fatalf("ParseUpdate(%s) succeeded", raw)
		}
	}
}

func TestRouterBroadcastAndExclusive(t *testing.T) {
	broadcast := New(config.Config{
		Clients: []config.ClientConfig{{ID: "openclaw"}, {ID: "unyolo"}},
		Routing: config.RoutingConfig{Mode: "broadcast"},
	})
	message := mustUpdate(t, `{"update_id":1,"message":{"text":"hello"}}`)
	if got := broadcast.Targets(message); !reflect.DeepEqual(got, []string{"openclaw", "unyolo"}) {
		t.Fatalf("broadcast targets = %v", got)
	}

	exclusive := New(config.Config{
		Clients: []config.ClientConfig{{ID: "openclaw"}, {ID: "unyolo"}},
		Routing: config.RoutingConfig{
			Mode: "exclusive",
			Rules: []config.RoutingRule{{
				Clients:              []string{"unyolo"},
				UpdateTypes:          []string{"callback_query"},
				CallbackDataPrefixes: []string{"bk:"},
			}},
			FallbackClients: []string{"openclaw"},
		},
	})
	approval := mustUpdate(t, `{"update_id":2,"callback_query":{"data":"bk:h:a:x:y"}}`)
	otherCallback := mustUpdate(t, `{"update_id":3,"callback_query":{"data":"openclaw:button"}}`)
	if got := exclusive.Targets(approval); !reflect.DeepEqual(got, []string{"unyolo"}) {
		t.Fatalf("approval targets = %v", got)
	}
	if got := exclusive.Targets(otherCallback); !reflect.DeepEqual(got, []string{"openclaw"}) {
		t.Fatalf("callback targets = %v", got)
	}
	if got := exclusive.Targets(message); !reflect.DeepEqual(got, []string{"openclaw"}) {
		t.Fatalf("message targets = %v", got)
	}
}

func TestTargetsReturnsCopies(t *testing.T) {
	router := New(config.Config{Clients: []config.ClientConfig{{ID: "one"}}, Routing: config.RoutingConfig{Mode: "broadcast"}})
	first := router.Targets(Update{})
	first[0] = "changed"
	if got := router.Targets(Update{}); got[0] != "one" {
		t.Fatalf("targets were mutated: %v", got)
	}
}

func mustUpdate(t *testing.T, raw string) Update {
	t.Helper()
	update, err := ParseUpdate(json.RawMessage(strings.TrimSpace(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return update
}
