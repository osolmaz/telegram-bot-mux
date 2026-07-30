package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/osolmaz/telegram-bot-mux/internal/config"
)

type Update struct {
	ID           int64
	Type         string
	CallbackData string
	Raw          json.RawMessage
}

type Router struct {
	mode     string
	clients  []string
	rules    []config.RoutingRule
	fallback []string
}

func New(cfg config.Config) *Router {
	clients := make([]string, 0, len(cfg.Clients))
	for _, client := range cfg.Clients {
		clients = append(clients, client.ID)
	}
	return &Router{
		mode:     cfg.Routing.Mode,
		clients:  slices.Clone(clients),
		rules:    slices.Clone(cfg.Routing.Rules),
		fallback: slices.Clone(cfg.Routing.FallbackClients),
	}
}

func ParseUpdate(raw json.RawMessage) (Update, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return Update{}, fmt.Errorf("decode update: %w", err)
	}
	if len(object) < 2 {
		return Update{}, errors.New("update must contain update_id and one update payload")
	}
	idRaw, exists := object["update_id"]
	if !exists {
		return Update{}, errors.New("update_id is required")
	}
	var id json.Number
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return Update{}, errors.New("update_id must be an integer")
	}
	parsedID, err := id.Int64()
	if err != nil || parsedID < 0 {
		return Update{}, errors.New("update_id must be a nonnegative integer")
	}
	keys := make([]string, 0, len(object)-1)
	for key := range object {
		if key != "update_id" {
			keys = append(keys, key)
		}
	}
	if len(keys) != 1 {
		return Update{}, errors.New("update must contain exactly one update payload")
	}
	updateType := keys[0]
	callbackData := ""
	if updateType == "callback_query" {
		var callback struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(object[updateType], &callback); err != nil {
			return Update{}, fmt.Errorf("decode callback_query: %w", err)
		}
		callbackData = callback.Data
	}
	return Update{ID: parsedID, Type: updateType, CallbackData: callbackData, Raw: slices.Clone(raw)}, nil
}

func (r *Router) Targets(update Update) []string {
	if r.mode == "broadcast" {
		return slices.Clone(r.clients)
	}
	for _, rule := range r.rules {
		if matches(rule, update) {
			return slices.Clone(rule.Clients)
		}
	}
	return slices.Clone(r.fallback)
}

func matches(rule config.RoutingRule, update Update) bool {
	if len(rule.UpdateTypes) > 0 && !slices.Contains(rule.UpdateTypes, update.Type) {
		return false
	}
	if len(rule.CallbackDataPrefixes) == 0 {
		return true
	}
	for _, prefix := range rule.CallbackDataPrefixes {
		if strings.HasPrefix(update.CallbackData, prefix) {
			return true
		}
	}
	return false
}
