package supabase

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client wraps minimal Supabase REST interactions used by the API server.
type Client struct {
	baseURL       string
	serviceKey    string
	encryptionKey []byte
	httpClient    *http.Client
}

// NewClientFromEnv builds a Supabase client using environment variables.
func NewClientFromEnv() (*Client, error) {
	url := strings.TrimSuffix(os.Getenv("SUPABASE_URL"), "/")
	serviceKey := os.Getenv("SUPABASE_SERVICE_KEY")
	encryptionKey := os.Getenv("ENCRYPTION_KEY")

	if url == "" || serviceKey == "" || encryptionKey == "" {
		return nil, errors.New("missing SUPABASE_URL, SUPABASE_SERVICE_KEY or ENCRYPTION_KEY")
	}

	// Derive a fixed-length key for AES-256 using SHA-256.
	keyHash := sha256.Sum256([]byte(encryptionKey))

	return &Client{
		baseURL:    url,
		serviceKey: serviceKey,
		// Retain a copy to avoid accidental modification from outside.
		encryptionKey: append([]byte(nil), keyHash[:]...),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// UpsertUserAPIKey encrypts the raw key and upserts it into Supabase.
func (c *Client) UpsertUserAPIKey(ctx context.Context, tgID, provider, rawKey string) error {
	if c == nil {
		return errors.New("supabase client is nil")
	}
	if strings.TrimSpace(tgID) == "" || strings.TrimSpace(provider) == "" || strings.TrimSpace(rawKey) == "" {
		return errors.New("tg_id, provider and raw key must be provided")
	}

	encrypted, err := c.encrypt(rawKey)
	if err != nil {
		return fmt.Errorf("encrypt key: %w", err)
	}

	payload := []map[string]string{{
		"tg_id":         strings.TrimSpace(tgID),
		"provider":      strings.TrimSpace(provider),
		"encrypted_key": encrypted,
	}}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal supabase payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/rest/v1/user_api_keys?on_conflict=tg_id,provider", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build supabase request: %w", err)
	}

	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=representation")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call supabase: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase upsert failed: status=%d body=%s", resp.StatusCode, string(responseBody))
	}

	return nil
}

func (c *Client) encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(c.encryptionKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	// Prepend nonce to ciphertext so it can be used during decryption.
	ciphertext := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return hex.EncodeToString(ciphertext), nil
}
