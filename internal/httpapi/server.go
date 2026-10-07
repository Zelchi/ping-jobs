package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ping-work/internal/config"
	"ping-work/internal/dedupe"
	"ping-work/internal/queue"
)

const (
	maxTitleLength   = 256
	maxContentLength = 4096
	maxLinkLength    = 2048
	maxDateLength    = 1024
)

type Server struct {
	apiToken     string
	destinations config.Destinations
	queue        *queue.Queue
	dedupe       *dedupe.Store
	logger       *slog.Logger
}

type sendRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Link    string `json:"link"`
	Date    string `json:"date"`
}

type sendResponse struct {
	ID           string `json:"id"`
	ExpiresAt    string `json:"expires_at"`
	Destinations int    `json:"destinations"`
}

func NewServer(apiToken string, destinations config.Destinations, messageQueue *queue.Queue, dedupeStore *dedupe.Store, logger *slog.Logger) *Server {
	return &Server{
		apiToken:     apiToken,
		destinations: destinations,
		queue:        messageQueue,
		dedupe:       dedupeStore,
		logger:       logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /messages", s.send)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "token inválido")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request sendRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "corpo JSON inválido")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "envie apenas um objeto JSON")
		return
	}
	request.Title = strings.TrimSpace(request.Title)
	request.Link = strings.TrimSpace(request.Link)
	request.Date = strings.TrimSpace(request.Date)
	if request.Title == "" || strings.TrimSpace(request.Content) == "" || request.Link == "" || request.Date == "" {
		writeError(w, http.StatusBadRequest, "title, content, link e date são obrigatórios")
		return
	}
	if !withinLimit(request.Title, maxTitleLength) {
		writeError(w, http.StatusBadRequest, "title deve ter até 256 caracteres UTF-8")
		return
	}
	if !withinLimit(request.Content, maxContentLength) {
		writeError(w, http.StatusBadRequest, "content deve ter até 4096 caracteres UTF-8")
		return
	}
	if !withinLimit(request.Link, maxLinkLength) || !validLink(request.Link) {
		writeError(w, http.StatusBadRequest, "link deve ser uma URL HTTP ou HTTPS válida")
		return
	}
	if !withinLimit(request.Date, maxDateLength) {
		writeError(w, http.StatusBadRequest, "date deve ter até 1024 caracteres UTF-8")
		return
	}

	channelIDs := make([]string, 0, len(s.destinations))
	for _, channelID := range s.destinations {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Strings(channelIDs)
	var message queue.Message
	duplicate, err := s.dedupe.Accept(r.Context(), request.Title, request.Link, func() error {
		var enqueueErr error
		message, enqueueErr = s.queue.Enqueue(request.Title, request.Content, request.Link, request.Date, channelIDs)
		return enqueueErr
	})
	if duplicate {
		writeError(w, http.StatusConflict, "vaga já recebida nos últimos 30 dias")
		return
	}
	if err != nil {
		if errors.Is(err, queue.ErrFull) {
			writeError(w, http.StatusServiceUnavailable, "fila temporariamente cheia")
			return
		}
		s.logger.Error("não foi possível aceitar ou registrar a vaga", "error", err)
		writeError(w, http.StatusInternalServerError, "não foi possível aceitar a mensagem")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(sendResponse{
		ID:           message.ID,
		ExpiresAt:    message.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Destinations: len(channelIDs),
	})
}

func (s *Server) authorized(r *http.Request) bool {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(value, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiToken)) == 1
}

func withinLimit(value string, limit int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit
}

func validLink(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
