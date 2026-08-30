package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func getKeycloakAdminToken(cfg *platformConfig) (string, error) {
	form := url.Values{
		"username":   {cfg.KeycloakAdminUser},
		"password":   {cfg.KeycloakAdminPass},
		"grant_type": {"password"},
		"client_id":  {"admin-cli"},
	}
	tokenURL := fmt.Sprintf("%s/realms/master/protocol/openid-connect/token", cfg.KeycloakURL)
	resp, err := httpClient.Post(tokenURL, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to call Keycloak token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Keycloak token request failed (HTTP %d): %s", resp.StatusCode, body)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode token response: %w", err)
	}
	return result.AccessToken, nil
}

func keycloakRequest(method, url string, body interface{}, token string) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return httpClient.Do(req)
}

// OIDCCredentials holds everything ESO needs to sync into the K8s Secret.
type OIDCCredentials struct {
	ClientID     string
	ClientSecret string
	IssuerURL    string
	CookieSecret string
}

// keycloakClientListItem represents a Keycloak client with just the ID field.
type keycloakClientListItem struct {
	ID string `json:"id"`
}

// decodeKeycloakClientList decodes the Keycloak Admin API /clients response.
// The endpoint can return either a JSON array or a single object when there is
// exactly one match, so we handle both shapes here.
func decodeKeycloakClientList(data []byte) ([]keycloakClientListItem, error) {
	// Try array first
	var asArray []keycloakClientListItem
	if err := json.Unmarshal(data, &asArray); err == nil {
		return asArray, nil
	}
	// Try single object
	var asObject keycloakClientListItem
	if err := json.Unmarshal(data, &asObject); err != nil {
		return nil, err
	}
	return []keycloakClientListItem{asObject}, nil
}

// registerKeycloakClient ensures the client exists and returns full credentials.
func registerKeycloakClient(ctx context.Context, appName string, cfg *platformConfig) (*OIDCCredentials, error) {
	token, err := getKeycloakAdminToken(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to get admin token: %w", err)
	}

	realmURL := fmt.Sprintf("%s/admin/realms/%s", cfg.KeycloakURL, cfg.KeycloakRealm)

	// Check if client already exists
	listURL := fmt.Sprintf("%s/clients?clientId=%s", realmURL, appName)
	resp, err := keycloakRequest("GET", listURL, nil, token)
	if err != nil {
		return nil, fmt.Errorf("failed to list clients: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read client list response: %w", err)
	}

	clients, err := decodeKeycloakClientList(body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode client list: %w", err)
	}

	var internalID string
	if len(clients) > 0 {
		internalID = clients[0].ID
	} else {
		// Create the client
		clientDef := map[string]interface{}{
			"clientId":            appName,
			"enabled":             true,
			"publicClient":        false,
			"standardFlowEnabled": true,
			"redirectUris":        []string{"*"},
			"webOrigins":          []string{"+"},
			"protocol":            "openid-connect",
			"attributes":          map[string]string{"helmsman.managed": "true"},
		}
		createURL := fmt.Sprintf("%s/clients", realmURL)
		resp, err = keycloakRequest("POST", createURL, clientDef, token)
		if err != nil {
			return nil, fmt.Errorf("failed to create client: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("client creation failed (HTTP %d): %s", resp.StatusCode, body)
		}
		location := resp.Header.Get("Location")
		parts := strings.Split(strings.TrimRight(location, "/"), "/")
		internalID = parts[len(parts)-1]
	}

	// Get client secret from Keycloak
	secretURL := fmt.Sprintf("%s/clients/%s/client-secret", realmURL, internalID)
	resp2, err := keycloakRequest("GET", secretURL, nil, token)
	if err != nil {
		return nil, fmt.Errorf("failed to get client secret: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		return nil, fmt.Errorf("client secret fetch failed (HTTP %d): %s", resp2.StatusCode, body)
	}
	var secretResp struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&secretResp); err != nil {
		return nil, fmt.Errorf("failed to decode client secret: %w", err)
	}

	return &OIDCCredentials{
		ClientID:     appName,
		ClientSecret: secretResp.Value,
		IssuerURL:    fmt.Sprintf("%s/realms/%s", cfg.KeycloakURL, cfg.KeycloakRealm),
	}, nil
}

// writeOIDCCredsToVault writes OIDC credentials to Vault KV v2.
// Preserves any existing cookie-secret to avoid invalidating active sessions.
func writeOIDCCredsToVault(ctx context.Context, appName string, creds *OIDCCredentials, cfg *platformConfig) error {
	vaultPath := fmt.Sprintf("%s/v1/secret/data/apps/%s/oidc", cfg.VaultURL, appName)

	// Check if path already exists to preserve cookie-secret
	existingCookieSecret := ""
	req, _ := http.NewRequest("GET", vaultPath, nil)
	req.Header.Set("X-Vault-Token", cfg.VaultToken)
	resp, err := httpClient.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var existing struct {
			Data struct {
				Data map[string]string `json:"data"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&existing); err == nil {
			existingCookieSecret = existing.Data.Data["cookie-secret"]
		}
	}

	// Generate cookie secret if none exists
	cookieSecret := existingCookieSecret
	if cookieSecret == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("failed to generate cookie secret: %w", err)
		}
		cookieSecret = base64.URLEncoding.EncodeToString(raw)
	}
	creds.CookieSecret = cookieSecret

	// Write to Vault KV v2
	payload := map[string]interface{}{
		"data": map[string]string{
			"client-id":     creds.ClientID,
			"client-secret": creds.ClientSecret,
			"issuer-url":    creds.IssuerURL,
			"cookie-secret": creds.CookieSecret,
		},
	}
	body, _ := json.Marshal(payload)
	writeReq, _ := http.NewRequest("POST", vaultPath, bytes.NewReader(body))
	writeReq.Header.Set("X-Vault-Token", cfg.VaultToken)
	writeReq.Header.Set("Content-Type", "application/json")

	writeResp, err := httpClient.Do(writeReq)
	if err != nil {
		return fmt.Errorf("failed to write to Vault: %w", err)
	}
	defer writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusOK && writeResp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(writeResp.Body)
		return fmt.Errorf("Vault write failed (HTTP %d): %s", writeResp.StatusCode, b)
	}
	return nil
}

// deleteKeycloakClient removes the Keycloak client during finalizer processing.
func deleteKeycloakClient(ctx context.Context, appName string, cfg *platformConfig) error {
	token, err := getKeycloakAdminToken(cfg)
	if err != nil {
		return fmt.Errorf("failed to get admin token: %w", err)
	}
	realmURL := fmt.Sprintf("%s/admin/realms/%s", cfg.KeycloakURL, cfg.KeycloakRealm)
	listURL := fmt.Sprintf("%s/clients?clientId=%s", realmURL, appName)
	resp, err := keycloakRequest("GET", listURL, nil, token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	clients, err := decodeKeycloakClientList(body)
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		return nil
	}
	deleteURL := fmt.Sprintf("%s/clients/%s", realmURL, clients[0].ID)
	resp2, err := keycloakRequest("DELETE", deleteURL, nil, token)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("client deletion failed (HTTP %d): %s", resp2.StatusCode, body)
	}
	return nil
}
