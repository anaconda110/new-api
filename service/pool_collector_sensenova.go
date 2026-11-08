package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// SenseNova Token Plan quota is an account-level console feature: the relay
// API key cannot read it, so collection logs into the console with a second
// credential and polls the token-plan pool-usage endpoint. The console
// throttles by source IP ("操作过于频繁"), so every console request goes
// through one global gate with a generous spacing — a six-account round
// takes a couple of minutes by design and never trips the limiter.
const (
	sensenovaProvider      = "sensenova"
	sensenovaConsoleBase   = "https://platform.sensenova.cn"
	sensenovaIAMBase       = "https://iam.sensecoreapi.cn/iam/authn"
	sensenovaClientID      = "nova"
	sensenovaMinIntervalMs = 20000
	sensenovaJitterMs      = 5000
)

var (
	consoleGateMu      sync.Mutex
	consoleNextAllowed time.Time
)

// throttleConsole spaces console requests out per source IP. Blocks until the
// slot opens, then reserves it.
func throttleConsole() {
	consoleGateMu.Lock()
	defer consoleGateMu.Unlock()
	now := time.Now()
	if consoleNextAllowed.After(now) {
		time.Sleep(consoleNextAllowed.Sub(now))
		now = time.Now()
	}
	jitter := time.Duration(common.GetRandomInt(sensenovaJitterMs)) * time.Millisecond
	consoleNextAllowed = now.Add(time.Duration(sensenovaMinIntervalMs)*time.Millisecond + jitter)
}

// sensenovaConsoleClient returns a client that stops at each redirect so the
// login chain can read Location headers, with cookies kept per flow.
func sensenovaConsoleClient(jar *cookiejar.Jar) *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sensenovaDo(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	throttleConsole()
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, body, err
}

// sensenovaPKCE builds the S256 pair the authorize step needs.
func sensenovaPKCE() (verifier string, challenge string) {
	raw := make([]byte, 48)
	_, _ = rand.Read(raw)
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return verifier, challenge
}

func sensenovaTokenExpiry(accessToken string, expiresIn int64) int64 {
	if expiresIn > 0 {
		return time.Now().Unix() + expiresIn
	}
	// Opaque fallback: 2h, matching the observed token lifetime.
	return time.Now().Unix() + 7200
}

// sensenovaRefresh exchanges a refresh_token for a new token set.
func sensenovaRefresh(refreshToken string) (access string, refresh string, expiresAt int64, err error) {
	throttleConsole()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {sensenovaClientID},
		"refresh_token": {refreshToken},
	}
	req, err := http.NewRequest("POST", sensenovaConsoleBase+"/oauth2/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, body, err := sensenovaDoRaw(client, req)
	if err != nil {
		return "", "", 0, err
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	if jsonErr := json.Unmarshal(body, &data); jsonErr != nil || data.AccessToken == "" {
		return "", "", 0, fmt.Errorf("sensenova token refresh failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 160)])))
	}
	return data.AccessToken, data.RefreshToken, sensenovaTokenExpiry(data.AccessToken, data.ExpiresIn), nil
}

func sensenovaDoRaw(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	throttleConsole()
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, body, err
}

// sensenovaPasswordLogin runs the full OAuth password flow: authorize with
// PKCE, solve the login challenge, follow the redirect chain to the code, and
// exchange it. Mirrors the community plugin's proven sequence.
func sensenovaPasswordLogin(username string, password string) (access string, refresh string, expiresAt int64, err error) {
	jar, _ := cookiejar.New(nil)
	client := sensenovaConsoleClient(jar)

	verifier, challenge := sensenovaPKCE()
	redirectURI := sensenovaConsoleBase
	authorizeURL := fmt.Sprintf("%s/oauth2/auth?%s", sensenovaConsoleBase, url.Values{
		"client_id":             {sensenovaClientID},
		"response_type":         {"code"},
		"scope":                 {"openid offline offline_access"},
		"redirect_uri":          {redirectURI},
		"state":                 {base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode())

	exchange := func(code string) (string, string, int64, error) {
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {sensenovaClientID},
			"code":          {code},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
		}
		req, reqErr := http.NewRequest("POST", sensenovaConsoleBase+"/oauth2/token", strings.NewReader(form.Encode()))
		if reqErr != nil {
			return "", "", 0, reqErr
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, body, doErr := sensenovaDoRaw(client, req)
		if doErr != nil {
			return "", "", 0, doErr
		}
		var data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if jsonErr := json.Unmarshal(body, &data); jsonErr != nil || data.AccessToken == "" {
			return "", "", 0, fmt.Errorf("sensenova code exchange failed (status %d)", resp.StatusCode)
		}
		return data.AccessToken, data.RefreshToken, sensenovaTokenExpiry(data.AccessToken, data.ExpiresIn), nil
	}

	codeOf := func(location string) string {
		if u, parseErr := url.Parse(location); parseErr == nil {
			if code := u.Query().Get("code"); code != "" {
				return code
			}
		}
		return ""
	}

	req, reqErr := http.NewRequest("GET", authorizeURL, nil)
	if reqErr != nil {
		return "", "", 0, reqErr
	}
	req.Header.Set("Accept", "text/html")
	res, _, err := sensenovaDo(client, req)
	if err != nil {
		return "", "", 0, err
	}
	location := res.Header.Get("Location")
	if code := codeOf(location); code != "" {
		return exchange(code)
	}
	matches := strings.SplitN(location, "login_challenge=", 2)
	if len(matches) < 2 || matches[1] == "" {
		return "", "", 0, fmt.Errorf("sensenova login: no challenge in redirect (status %d)", res.StatusCode)
	}
	challengeValue := matches[1]
	if cut := strings.IndexAny(challengeValue, "&"); cut >= 0 {
		challengeValue = challengeValue[:cut]
	}

	checkReq, _ := http.NewRequest("GET", sensenovaIAMBase+"/v1/auth/checkChallenge?challenge="+url.QueryEscape(challengeValue), nil)
	checkReq.Header.Set("Accept", "application/json")
	checkResp, checkBody, err := sensenovaDo(client, checkReq)
	if err != nil {
		return "", "", 0, err
	}
	var check struct {
		IsValid bool `json:"is_valid"`
	}
	_ = json.Unmarshal(checkBody, &check)
	if checkResp.StatusCode != http.StatusOK || !check.IsValid {
		return "", "", 0, fmt.Errorf("sensenova login: challenge expired (status %d)", checkResp.StatusCode)
	}

	loginReq, _ := http.NewRequest("POST", sensenovaIAMBase+"/v1/auth/nova/login",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q,"challenge":%q,"is_encrypt":false}`, username, password, challengeValue)))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Accept", "application/json")
	loginReq.Header.Set("Origin", sensenovaConsoleBase)
	loginReq.Header.Set("Referer", sensenovaConsoleBase+"/")
	loginResp, loginBody, err := sensenovaDo(client, loginReq)
	if err != nil {
		return "", "", 0, err
	}
	var loginData struct {
		Redirect string `json:"redirect"`
		Details  []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"details"`
	}
	_ = json.Unmarshal(loginBody, &loginData)
	if loginResp.StatusCode != http.StatusOK {
		reason := ""
		for _, d := range loginData.Details {
			reason = strings.ToLower(strings.TrimSpace(d.Reason))
			if reason == "" {
				reason = strings.ToLower(strings.TrimSpace(d.Message))
			}
			break
		}
		switch {
		case strings.Contains(reason, "frequen") || strings.Contains(reason, "toomanyattempts"):
			return "", "", 0, errors.New("sensenova login throttled by platform, retry later")
		case strings.Contains(reason, "password") || strings.Contains(reason, "invalidaccount"):
			return "", "", 0, errors.New("sensenova login rejected: invalid account or password")
		case strings.Contains(reason, "locked"):
			return "", "", 0, errors.New("sensenova login rejected: account locked")
		}
		return "", "", 0, fmt.Errorf("sensenova login rejected (status %d): %s", loginResp.StatusCode, reason)
	}
	if loginData.Redirect == "" {
		return "", "", 0, errors.New("sensenova login: no redirect after login")
	}

	next := loginData.Redirect
	for i := 0; i < 10; i++ {
		hopReq, _ := http.NewRequest("GET", next, nil)
		hopReq.Header.Set("Accept", "text/html,application/json")
		hopResp, _, hopErr := sensenovaDo(client, hopReq)
		if hopErr != nil {
			return "", "", 0, hopErr
		}
		loc := hopResp.Header.Get("Location")
		if loc == "" {
			return "", "", 0, fmt.Errorf("sensenova login: redirect chain stuck at %s", next[:min(len(next), 80)])
		}
		abs := loc
		if u, parseErr := url.Parse(loc); parseErr == nil {
			if u.IsAbs() {
				abs = loc
			} else if base, baseErr := url.Parse(next); baseErr == nil {
				abs = base.ResolveReference(u).String()
			}
		}
		if code := codeOf(abs); code != "" {
			return exchange(code)
		}
		if strings.Contains(abs, "login_challenge=") {
			return "", "", 0, errors.New("sensenova login: bounced back to login")
		}
		next = abs
	}
	return "", "", 0, errors.New("sensenova login: redirect chain too long")
}

// sensenovaFetchPoolUsage reads the token-plan pool usage of one account.
func sensenovaFetchPoolUsage(accessToken string) ([]model.PoolMemberWindow, error) {
	req, err := http.NewRequest("GET", sensenovaConsoleBase+"/lite/console/v1/tokenplan/pool-usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, body, err := sensenovaDoRaw(client, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("sensenova console token rejected (401)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sensenova pool-usage status %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 120)])))
	}
	return sensenovaMapPoolUsage(body)
}

// sensenovaMapPoolUsage converts the console response into pool windows.
func sensenovaMapPoolUsage(body []byte) ([]model.PoolMemberWindow, error) {
	var data struct {
		Pools []struct {
			Name     string               `json:"name"`
			PoolType string               `json:"pool_type"`
			Window5h *sensenovaWindowData `json:"window_5h"`
			Window7d *sensenovaWindowData `json:"window_7d"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("sensenova pool-usage decode: %v", err)
	}
	windows := make([]model.PoolMemberWindow, 0, len(data.Pools)*2)
	for _, pool := range data.Pools {
		for _, w := range []struct {
			data  *sensenovaWindowData
			short bool
		}{
			{pool.Window5h, true},
			{pool.Window7d, false},
		} {
			if w.data == nil || w.data.Limit <= 0 {
				continue
			}
			snapshot := model.PoolMemberWindow{
				Remaining:   w.data.Remaining,
				Capacity:    w.data.Limit,
				ShortWindow: w.short,
				Source:      "collector",
			}
			if w.data.ResetAt > 0 {
				snapshot.ExpiryAt = time.Unix(w.data.ResetAt, 0)
			}
			windows = append(windows, snapshot)
		}
	}
	return windows, nil
}

type sensenovaWindowData struct {
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	ResetAt   int64   `json:"reset_at"`
}

// sensenovaConsoleCollector implements QuotaCollector over the console
// credential of a channel. Quota is account-level: one fetch serves the
// channel's keys.
type sensenovaConsoleCollector struct{}

func (sensenovaConsoleCollector) Collect(channel *model.Channel, keyIndex int) ([]model.PoolMemberWindow, error) {
	cred, err := model.GetChannelConsoleCredential(channel.Id)
	if err != nil || cred == nil {
		return nil, fmt.Errorf("channel #%d has no %s console credential", channel.Id, sensenovaProvider)
	}
	if cred.Provider != sensenovaProvider {
		return nil, fmt.Errorf("credential provider mismatch: %s", cred.Provider)
	}

	accessToken := cred.AccessToken
	if accessToken == "" || cred.TokenExpiresAt <= time.Now().Unix()+60 {
		if cred.RefreshToken != "" {
			access, refresh, expiresAt, refreshErr := sensenovaRefresh(cred.RefreshToken)
			if refreshErr != nil {
				if cred.Password == "" {
					return nil, fmt.Errorf("sensenova refresh failed and no password fallback: %v", refreshErr)
				}
				access, refresh, expiresAt, err = sensenovaPasswordLogin(cred.Username, cred.Password)
				if err != nil {
					return nil, err
				}
			}
			accessToken = access
			_ = model.UpdateChannelConsoleCredentialTokens(channel.Id, access, refresh, expiresAt)
		} else if cred.Password != "" {
			access, refresh, expiresAt, loginErr := sensenovaPasswordLogin(cred.Username, cred.Password)
			if loginErr != nil {
				return nil, loginErr
			}
			accessToken = access
			_ = model.UpdateChannelConsoleCredentialTokens(channel.Id, access, refresh, expiresAt)
		} else {
			return nil, errors.New("sensenova credential has neither refresh_token nor password")
		}
	}

	windows, err := sensenovaFetchPoolUsage(accessToken)
	if err != nil {
		// A rejected token is dropped so the next round re-authenticates;
		// everything else (403/5xx/throttle) keeps the cached token.
		if strings.Contains(err.Error(), "(401)") {
			_ = model.UpdateChannelConsoleCredentialTokens(channel.Id, "", cred.RefreshToken, 0)
		}
		return nil, err
	}
	if len(windows) == 0 {
		return nil, errors.New("sensenova pool-usage returned no pools")
	}
	return windows, nil
}

func init() {
	RegisterCredentialCollector(sensenovaProvider, sensenovaConsoleCollector{})
}
