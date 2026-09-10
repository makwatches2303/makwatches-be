package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleOAuth handles Google OAuth authentication.
//
// CSRF state is not tracked here -- it used to be an in-process map, which
// does not survive across server instances (or Lambda cold starts). The
// caller (AuthHandler in internal/handlers/auth_handler.go) stores state in
// Redis instead, alongside its other shared state.
type GoogleOAuth struct {
	config *oauth2.Config
}

// GoogleUserInfo represents user information from Google
type GoogleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	Locale        string `json:"locale"`
}

// NewGoogleOAuth creates a new GoogleOAuth instance
func NewGoogleOAuth(clientID, clientSecret, redirectURL string) *GoogleOAuth {
	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes: []string{
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		},
		Endpoint: google.Endpoint,
	}

	return &GoogleOAuth{
		config: config,
	}
}

// GetAuthURL returns the Google OAuth authorization URL
func (g *GoogleOAuth) GetAuthURL(state string) string {
	return g.config.AuthCodeURL(state, oauth2.AccessTypeOffline)
}

// Exchange exchanges authorization code for access token
func (g *GoogleOAuth) Exchange(code string) (*oauth2.Token, error) {
	token, err := g.config.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}
	return token, nil
}

// GetUserInfo retrieves user information from Google
func (g *GoogleOAuth) GetUserInfo(token *oauth2.Token) (map[string]interface{}, error) {
	client := g.config.Client(context.Background(), token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get user info: status %d, body: %s", resp.StatusCode, string(body))
	}

	var userInfo map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	return userInfo, nil
}
