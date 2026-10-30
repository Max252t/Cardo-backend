package main

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

	"github.com/google/uuid"
)

const (
	maxRetries     = 8
	defaultBackoff = time.Second
	avatarColor    = "#336699"
	memberRole     = "editor"
)

type apiClient struct {
	base string
	http *http.Client
}

func newAPIClient(base string) *apiClient {
	return &apiClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

type account struct {
	ID       uuid.UUID
	Email    string
	Token    string
	Refresh  string
	Password string
}

type sessionResponse struct {
	User struct {
		ID uuid.UUID `json:"id"`
	} `json:"user"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type boardResponse struct {
	ID uuid.UUID `json:"id"`
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
			return time.Duration(secs)*time.Second + 50*time.Millisecond
		}
	}
	return defaultBackoff * time.Duration(attempt+1)
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusBadGateway
}

func (a *apiClient) post(ctx context.Context, path, token string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("POST %s: %w", path, err)
		}
		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if retryable(resp.StatusCode) && attempt < maxRetries {
			select {
			case <-time.After(retryDelay(resp, attempt)):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

func (a *apiClient) register(ctx context.Context, email, name, password string) (account, error) {
	var s sessionResponse
	err := a.post(ctx, "/api/auth/register", "", map[string]string{
		"email":        email,
		"password":     password,
		"display_name": name,
		"avatar_color": avatarColor,
	}, &s)
	if err != nil {
		return account{}, err
	}
	return account{ID: s.User.ID, Email: email, Token: s.AccessToken, Refresh: s.RefreshToken, Password: password}, nil
}

func (a *apiClient) createBoard(ctx context.Context, token, title string) (uuid.UUID, error) {
	var b boardResponse
	if err := a.post(ctx, "/api/boards", token, map[string]string{"title": title}, &b); err != nil {
		return uuid.Nil, err
	}
	return b.ID, nil
}

func (a *apiClient) addMember(ctx context.Context, token string, boardID uuid.UUID, email string) error {
	return a.post(ctx, "/api/boards/"+boardID.String()+"/members", token,
		map[string]string{"email": email, "role": memberRole}, nil)
}

func (a *apiClient) wsURL(boardID uuid.UUID) string {
	u := a.base
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/ws/boards/" + boardID.String()
}

func (a *apiClient) refresh(ctx context.Context, refreshToken string) (string, string, error) {
	var s sessionResponse
	if err := a.post(ctx, "/api/auth/refresh", "", map[string]string{"refresh_token": refreshToken}, &s); err != nil {
		return "", "", err
	}
	return s.AccessToken, s.RefreshToken, nil
}
