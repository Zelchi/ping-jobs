package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const apiBaseURL = "https://discord.com/api/v10"

type Client struct {
	token      string
	httpClient *http.Client
}

type APIError struct {
	StatusCode int
	Message    string
	RetryAfter time.Duration
}

type embed struct {
	Title       string       `json:"title"`
	Description string       `json:"description"`
	URL         string       `json:"url"`
	Fields      []embedField `json:"fields"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Discord retornou HTTP %d: %s", e.StatusCode, e.Message)
}

func NewClient(token string) *Client {
	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (c *Client) SendWork(ctx context.Context, channelID, title, content, link, date string) error {
	body, err := json.Marshal(struct {
		Embeds []embed `json:"embeds"`
	}{Embeds: []embed{{
		Title:       title,
		Description: content,
		URL:         link,
		Fields: []embedField{{
			Name:   "Data",
			Value:  date,
			Inline: true,
		}},
	}}})
	if err != nil {
		return fmt.Errorf("codificar mensagem: %w", err)
	}

	url := apiBaseURL + "/channels/" + channelID + "/messages"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("criar requisição para Discord: %w", err)
	}
	request.Header.Set("Authorization", "Bot "+c.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("enviar mensagem ao Discord: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}

	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	var details struct {
		Message    string  `json:"message"`
		RetryAfter float64 `json:"retry_after"`
	}
	_ = json.Unmarshal(responseBody, &details)
	if details.Message == "" {
		details.Message = strings.TrimSpace(string(responseBody))
	}
	if details.Message == "" {
		details.Message = http.StatusText(response.StatusCode)
	}

	retryAfter := time.Duration(details.RetryAfter * float64(time.Second))
	if header := response.Header.Get("Retry-After"); header != "" {
		if seconds, parseErr := strconv.ParseFloat(header, 64); parseErr == nil {
			headerDelay := time.Duration(seconds * float64(time.Second))
			if headerDelay > retryAfter {
				retryAfter = headerDelay
			}
		}
	}
	return &APIError{
		StatusCode: response.StatusCode,
		Message:    details.Message,
		RetryAfter: retryAfter,
	}
}
