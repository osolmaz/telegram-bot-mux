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
	object, err := decodeUpdateObject(raw)
	if err != nil {
		return Update{}, err
	}
	id, err := parseUpdateID(object)
	if err != nil {
		return Update{}, err
	}
	updateType, payload, err := updatePayload(object)
	if err != nil {
		return Update{}, err
	}
	callbackData, err := parseCallbackData(updateType, payload)
	if err != nil {
		return Update{}, err
	}
	return Update{ID: id, Type: updateType, CallbackData: callbackData, Raw: slices.Clone(raw)}, nil
}

func decodeUpdateObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("decode update: %w", err)
	}
	if len(object) < 2 {
		return nil, errors.New("update must contain update_id and one update payload")
	}
	return object, nil
}

func parseUpdateID(object map[string]json.RawMessage) (int64, error) {
	idRaw, exists := object["update_id"]
	if !exists {
		return 0, errors.New("update_id is required")
	}
	var id json.Number
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return 0, errors.New("update_id must be an integer")
	}
	parsed, err := id.Int64()
	if err != nil || parsed < 0 {
		return 0, errors.New("update_id must be a nonnegative integer")
	}
	return parsed, nil
}

func updatePayload(object map[string]json.RawMessage) (string, json.RawMessage, error) {
	keys := make([]string, 0, len(object)-1)
	for key := range object {
		if key != "update_id" {
			keys = append(keys, key)
		}
	}
	if len(keys) != 1 {
		return "", nil, errors.New("update must contain exactly one update payload")
	}
	return keys[0], object[keys[0]], nil
}

func parseCallbackData(updateType string, payload json.RawMessage) (string, error) {
	if updateType != "callback_query" {
		return "", nil
	}
	var callback struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(payload, &callback); err != nil {
		return "", fmt.Errorf("decode callback_query: %w", err)
	}
	return callback.Data, nil
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
