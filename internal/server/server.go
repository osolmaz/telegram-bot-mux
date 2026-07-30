package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/store"
	"github.com/osolmaz/telegram-bot-mux/internal/telegram"
)

const maxControlBodyBytes = 64 << 10

var methodPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

type Server struct {
	store       *store.Store
	telegram    *telegram.Client
	clientToken map[string]string
	logger      *slog.Logger
}

type getUpdatesRequest struct {
	Offset         int64    `json:"offset"`
	Limit          int      `json:"limit"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

func New(updateStore *store.Store, telegramClient *telegram.Client, clientTokens map[string]string, logger *slog.Logger) *Server {
	tokens := make(map[string]string, len(clientTokens))
	for id, token := range clientTokens {
		tokens[id] = token
	}
	return &Server{store: updateStore, telegram: telegramClient, clientToken: tokens, logger: logger}
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" || request.URL.Path == "/readyz" {
		writeJSON(response, http.StatusOK, `{"ok":true}`)
		return
	}
	clientID, token, kind, suffix, ok := parseClientPath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}
	if !s.authenticate(clientID, token) {
		writeTelegramError(response, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch kind {
	case "bot":
		s.handleBot(response, request, clientID, suffix)
	case "file":
		s.handleFile(response, request, suffix)
	default:
		http.NotFound(response, request)
	}
}

func parseClientPath(value string) (clientID, token, kind, suffix string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
	if len(parts) < 4 || parts[0] != "client" {
		return "", "", "", "", false
	}
	clientID = parts[1]
	switch {
	case strings.HasPrefix(parts[2], "bot") && len(parts) == 4:
		return clientID, strings.TrimPrefix(parts[2], "bot"), "bot", parts[3], true
	case parts[2] == "file" && len(parts) >= 5 && strings.HasPrefix(parts[3], "bot"):
		return clientID, strings.TrimPrefix(parts[3], "bot"), "file", strings.Join(parts[4:], "/"), true
	default:
		return "", "", "", "", false
	}
}

func (s *Server) authenticate(clientID, token string) bool {
	expected, exists := s.clientToken[clientID]
	if !exists || len(expected) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(token)) == 1
}

func (s *Server) handleBot(response http.ResponseWriter, request *http.Request, clientID, method string) {
	if !methodPattern.MatchString(method) {
		writeTelegramError(response, http.StatusBadRequest, "invalid method")
		return
	}
	switch strings.ToLower(method) {
	case "getupdates":
		s.handleGetUpdates(response, request, clientID)
	case "setwebhook":
		writeTelegramError(response, http.StatusConflict, "webhook mode is unavailable while the multiplexer owns polling")
	case "deletewebhook":
		writeJSON(response, http.StatusOK, `{"ok":true,"result":true}`)
	case "getwebhookinfo":
		s.handleWebhookInfo(response, request.Context(), clientID)
	default:
		s.proxyBot(response, request, clientID, method)
	}
}

func (s *Server) handleGetUpdates(response http.ResponseWriter, request *http.Request, clientID string) {
	params, err := parseGetUpdatesRequest(response, request)
	if err != nil {
		writeTelegramError(response, http.StatusBadRequest, err.Error())
		return
	}
	if params.Limit < 1 || params.Limit > 100 {
		params.Limit = 100
	}
	if params.Timeout < 0 || params.Timeout > 50 {
		writeTelegramError(response, http.StatusBadRequest, "timeout must be between 0 and 50")
		return
	}
	deadline := time.NewTimer(time.Duration(params.Timeout) * time.Second)
	defer deadline.Stop()
	for {
		changes := s.store.Changes()
		updates, queryErr := s.store.GetUpdates(request.Context(), clientID, params.Offset, params.Limit)
		if queryErr != nil {
			s.logger.Error("getUpdates failed", "client", clientID, "error", queryErr)
			writeTelegramError(response, http.StatusInternalServerError, "internal error")
			return
		}
		if len(updates) > 0 {
			writeUpdates(response, updates)
			return
		}
		if params.Timeout == 0 {
			writeUpdates(response, nil)
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-deadline.C:
			writeUpdates(response, nil)
			return
		case <-changes:
		}
	}
}

func parseGetUpdatesRequest(response http.ResponseWriter, request *http.Request) (getUpdatesRequest, error) {
	request.Body = http.MaxBytesReader(response, request.Body, maxControlBodyBytes)
	contentType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if contentType == "application/json" {
		var params getUpdatesRequest
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&params); err != nil && !errors.Is(err, io.EOF) {
			return getUpdatesRequest{}, errors.New("invalid getUpdates JSON")
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return getUpdatesRequest{}, errors.New("getUpdates body must contain one JSON value")
		}
		return params, nil
	}
	if err := request.ParseForm(); err != nil {
		return getUpdatesRequest{}, errors.New("invalid getUpdates form")
	}
	return getUpdatesRequest{
		Offset:  parseInt64(request.Form.Get("offset")),
		Limit:   parseInt(request.Form.Get("limit")),
		Timeout: parseInt(request.Form.Get("timeout")),
	}, nil
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func parseInt(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func writeUpdates(response http.ResponseWriter, updates []store.Delivery) {
	var body bytes.Buffer
	body.WriteString(`{"ok":true,"result":[`)
	for index, update := range updates {
		if index > 0 {
			body.WriteByte(',')
		}
		body.Write(update.Payload)
	}
	body.WriteString(`]}`)
	writeJSON(response, http.StatusOK, body.String())
}

func (s *Server) handleWebhookInfo(response http.ResponseWriter, ctx context.Context, clientID string) {
	pending, err := s.store.PendingCount(ctx, clientID)
	if err != nil {
		writeTelegramError(response, http.StatusInternalServerError, "internal error")
		return
	}
	payload := fmt.Sprintf(`{"ok":true,"result":{"url":"","has_custom_certificate":false,"pending_update_count":%d}}`, pending)
	writeJSON(response, http.StatusOK, payload)
}

func (s *Server) proxyBot(response http.ResponseWriter, request *http.Request, clientID, method string) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		writeTelegramError(response, http.StatusMethodNotAllowed, "method must use GET or POST")
		return
	}
	upstream, err := s.telegram.ForwardBot(request.Context(), request.Method, method, request.URL.RawQuery, request.Header, request.Body, request.ContentLength)
	if err != nil {
		s.logger.Warn("Telegram Bot API proxy failed", "client", clientID, "method", method, "error", err)
		writeTelegramError(response, http.StatusBadGateway, "Telegram API unavailable")
		return
	}
	defer upstream.Body.Close()
	copyHeaders(response.Header(), upstream.Header)
	response.WriteHeader(upstream.StatusCode)
	if _, err := io.Copy(response, upstream.Body); err != nil {
		s.logger.Warn("Telegram Bot API response stream failed", "client", clientID, "method", method, "error", err)
	}
}

func (s *Server) handleFile(response http.ResponseWriter, request *http.Request, filePath string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeTelegramError(response, http.StatusMethodNotAllowed, "file request must use GET or HEAD")
		return
	}
	upstream, err := s.telegram.ForwardFile(request.Context(), request.Method, filePath, request.URL.RawQuery, request.Header)
	if err != nil {
		s.logger.Warn("Telegram file proxy failed", "error", err)
		writeTelegramError(response, http.StatusBadGateway, "Telegram file API unavailable")
		return
	}
	defer upstream.Body.Close()
	copyHeaders(response.Header(), upstream.Header)
	response.WriteHeader(upstream.StatusCode)
	if request.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(response, upstream.Body); err != nil {
		s.logger.Warn("Telegram file response stream failed", "error", err)
	}
}

func copyHeaders(target, source http.Header) {
	for key, values := range source {
		if isHopByHop(key) {
			continue
		}
		target[key] = append([]string(nil), values...)
	}
}

func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func writeJSON(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body)
}

func writeTelegramError(response http.ResponseWriter, status int, description string) {
	encoded, _ := json.Marshal(struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}{OK: false, ErrorCode: status, Description: description})
	writeJSON(response, status, string(encoded))
}

func ClientAPIBase(listen, clientID string) string {
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return (&url.URL{Scheme: "http", Host: host, Path: "/client/" + clientID}).String()
}
