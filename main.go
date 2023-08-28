package keycloakopenid

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	KeycloakURL   string `json:"url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	KeycloakRealm string `json:"keycloak_realm"`
}

type keycloakAuth struct {
	next   http.Handler
	config *Config
}

type KeycloakTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type state struct {
	RedirectURL string `json:"redirect_url"`
}

func CreateConfig() *Config {
	return &Config{}
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.KeycloakURL == "" || config.ClientID == "" {
		return nil, errors.New("invalid configuration")
	}

	return &keycloakAuth{
		next:   next,
		config: config,
	}, nil
}

func (k *keycloakAuth) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	cookie, err := req.Cookie("Authorization")
	if err == nil && strings.HasPrefix(cookie.Value, "Bearer ") {
		token := strings.TrimPrefix(cookie.Value, "Bearer ")
		fmt.Printf("token = %+v\n", token)

		ok, err := k.verifyToken(token)
		fmt.Printf("ok = %+v\n", ok)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		if !ok {
			qry := req.URL.Query()
			qry.Del("code")
			qry.Del("state")
			qry.Del("session_state")
			req.URL.RawQuery = qry.Encode()
			req.RequestURI = req.URL.RequestURI()

			expiration := time.Now().Add(-24 * time.Hour)
			newCookie := &http.Cookie{
				Name:    "Authorization",
				Value:   "",
				Path:    "/",
				Expires: expiration,
				MaxAge:  -1,
			}
			http.SetCookie(rw, newCookie)

			k.redirectToKeycloak(rw, req)
			return
		}

		k.next.ServeHTTP(rw, req)
	} else {
		authCode := req.URL.Query().Get("code")
		if authCode == "" {
			fmt.Printf("code is missing, redirect to keycloak\n")
			k.redirectToKeycloak(rw, req)
			return
		}

		stateBase64 := req.URL.Query().Get("state")
		if stateBase64 == "" {
			fmt.Printf("state is missing, redirect to keycloak\n")
			k.redirectToKeycloak(rw, req)
			return
		}

		fmt.Printf("exchange auth code called\n")
		token, err := k.exchangeAuthCode(req, authCode, stateBase64)
		fmt.Printf("exchange auth code finished %+v\n", token)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		http.SetCookie(rw, &http.Cookie{
			Name:     "Authorization",
			Value:    "Bearer " + token,
			Secure:   true,
			HttpOnly: true,
			Path:     "/",
			SameSite: http.SameSiteStrictMode,
		})

		qry := req.URL.Query()
		qry.Del("code")
		qry.Del("state")
		qry.Del("session_state")
		req.URL.RawQuery = qry.Encode()
		req.RequestURI = req.URL.RequestURI()

		scheme := req.Header.Get("X-Forwarded-Proto")
		host := req.Header.Get("X-Forwarded-Host")
		originalURL := fmt.Sprintf("%s://%s%s", scheme, host, req.RequestURI)

		http.Redirect(rw, req, originalURL, http.StatusFound)
	}
}

func (k *keycloakAuth) exchangeAuthCode(req *http.Request, authCode string, stateBase64 string) (string, error) {
	stateBytes, _ := base64.StdEncoding.DecodeString(stateBase64)
	var state state
	json.Unmarshal(stateBytes, &state)

	resp, err := http.PostForm("http://"+k.config.KeycloakURL+"/realms/"+k.config.KeycloakRealm+"/protocol/openid-connect/token",
		url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {k.config.ClientID},
			"client_secret": {k.config.ClientSecret},
			"code":          {authCode},
			"redirect_uri":  {state.RedirectURL},
		})

	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", errors.New("received bad response from Keycloak: " + string(body))
	}

	var tokenResponse KeycloakTokenResponse
	err = json.NewDecoder(resp.Body).Decode(&tokenResponse)
	if err != nil {
		return "", err
	}

	return tokenResponse.AccessToken, nil
}

func (k *keycloakAuth) redirectToKeycloak(rw http.ResponseWriter, req *http.Request) {
	scheme := req.Header.Get("X-Forwarded-Proto")
	host := req.Header.Get("X-Forwarded-Host")
	originalURL := fmt.Sprintf("%s://%s%s", scheme, host, req.RequestURI)

	state := state{
		RedirectURL: originalURL,
	}

	stateBytes, _ := json.Marshal(state)
	stateBase64 := base64.StdEncoding.EncodeToString(stateBytes)

	redirectURL := url.URL{
		Scheme: "http",
		Host:   k.config.KeycloakURL,
		Path:   "/realms/" + k.config.KeycloakRealm + "/protocol/openid-connect/auth",
		RawQuery: url.Values{
			"response_type": {"code"},
			"client_id":     {k.config.ClientID},
			"redirect_uri":  {originalURL},
			"state":         {stateBase64},
		}.Encode(),
	}

	http.Redirect(rw, req, redirectURL.String(), http.StatusFound)
}

func (k *keycloakAuth) verifyToken(token string) (bool, error) {
	client := &http.Client{}

	data := url.Values{
		"token": {token},
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+k.config.KeycloakURL+"/realms/"+k.config.KeycloakRealm+"/protocol/openid-connect/token/introspect", strings.NewReader(data.Encode()))
	if err != nil {
		return false, err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(k.config.ClientID, k.config.ClientSecret)

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, nil
	}

	var introspectResponse map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&introspectResponse)
	if err != nil {
		return false, err
	}

	return introspectResponse["active"].(bool), nil
}
