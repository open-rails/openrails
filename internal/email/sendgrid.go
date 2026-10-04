// Package email holds OpenRails' built-in email sender (SendGrid) and the
// adapter that hands the control plane's AuthKit messages to the one sender a
// deployment has.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/config"
)

const sendGridAPI = "https://api.sendgrid.com"

// SendGrid delivers email through SendGrid's v3 Mail Send API.
type SendGrid struct {
	key    string
	from   config.EmailAddress
	base   string
	client *http.Client
}

// NewSendGrid validates cfg and returns its sender.
func NewSendGrid(cfg config.SendGridConfig) (*SendGrid, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, errors.New("sendgrid: api_key is required")
	}
	return &SendGrid{key: key, from: trim(cfg.From), base: sendGridAPI, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Send delivers m, filling an empty From field with the configured one.
func (s *SendGrid) Send(ctx context.Context, m config.Email) error {
	from := trim(m.From)
	if from.Address == "" {
		from.Address = s.from.Address
	}
	if from.Name == "" {
		from.Name = s.from.Name
	}
	if from.Address == "" {
		return errors.New("sendgrid: no from address: set the merchant's profile from_email or sendgrid.from")
	}
	if strings.TrimSpace(m.To) == "" {
		return errors.New("sendgrid: no recipient")
	}
	type address struct {
		Email string `json:"email"`
		Name  string `json:"name,omitempty"`
	}
	type content struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	body := struct {
		Personalizations []struct {
			To []address `json:"to"`
		} `json:"personalizations"`
		From    address   `json:"from"`
		Subject string    `json:"subject"`
		Content []content `json:"content"`
	}{From: address{from.Address, from.Name}, Subject: m.Subject}
	body.Personalizations = append(body.Personalizations, struct {
		To []address `json:"to"`
	}{To: []address{{Email: strings.TrimSpace(m.To)}}})
	if m.Text != "" {
		body.Content = append(body.Content, content{"text/plain", m.Text})
	}
	if m.HTML != "" {
		body.Content = append(body.Content, content{"text/html", m.HTML})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodPost, "/v3/mail/send", raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("sendgrid: mail send answered %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
	return nil
}

// CheckHealth reports whether the key may send mail.
func (s *SendGrid) CheckHealth(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodGet, "/v3/scopes", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sendgrid: scopes answered %d", resp.StatusCode)
	}
	var scopes struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&scopes); err != nil {
		return fmt.Errorf("sendgrid: scopes: %w", err)
	}
	if !slices.Contains(scopes.Scopes, "mail.send") {
		return errors.New("sendgrid: the API key lacks the mail.send scope")
	}
	return nil
}

func (s *SendGrid) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sendgrid: %w", err)
	}
	return resp, nil
}

func trim(a config.EmailAddress) config.EmailAddress {
	return config.EmailAddress{Name: strings.TrimSpace(a.Name), Address: strings.TrimSpace(a.Address)}
}
