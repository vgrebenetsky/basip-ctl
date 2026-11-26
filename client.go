package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
)

// Client — сеанс роботи з одним пристроєм.
type Client struct {
	app   *App
	dev   *Device
	prof  *Profile
	token string
	long  bool // довгий тайм-аут (завантаження файлів)
}

func (a *App) NewClient(d *Device) *Client {
	return &Client{app: a, dev: d, prof: a.Cfg.ProfileFor(d)}
}

// Refresh повторно вибирає профіль (після уточнення типу пристрою через /api/info).
func (c *Client) Refresh() { c.prof = c.app.Cfg.ProfileFor(c.dev) }

func (c *Client) op(name string) (Endpoint, bool) {
	ep, ok := c.prof.Ops[name]
	return ep, ok
}

func (c *Client) fullURL(ep Endpoint) string {
	p := ep.Path
	if !strings.HasPrefix(p, "/api/") {
		p = c.prof.Prefix + p
	}
	return c.app.Scheme + "://" + c.dev.IP + p
}

// request виконує HTTP-запит. У журнал потрапляє лише метод, шлях і код відповіді, без параметрів.
func (c *Client) request(method, rawURL string, q url.Values, body []byte, ctype string) (int, []byte, error) {
	code, data, _, err := c.requestH(method, rawURL, q, body, ctype)
	return code, data, err
}

// requestH — те саме, що request, але повертає ще й заголовки відповіді.
func (c *Client) requestH(method, rawURL string, q url.Values, body []byte, ctype string) (int, []byte, http.Header, error) {
	logURL := rawURL
	if len(q) > 0 {
		rawURL += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json, */*")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	hc := c.app.HTTP
	if c.long && c.app.HTTPLong != nil {
		hc = c.app.HTTPLong
	}
	resp, err := hc.Do(req)
	if err != nil {
		logf("%s %s %s → помилка: %s", c.dev.IP, method, logURL, netErrText(err))
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	logf("%s %s %s → %d", c.dev.IP, method, logURL, resp.StatusCode)
	return resp.StatusCode, data, resp.Header, err
}

// netErrText — зрозумілий опис мережевої помилки.
func netErrText(err error) string {
	// Прибираємо з помилки URL (у ньому можуть бути параметри запиту).
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "пристрій розірвав з'єднання (перевантажений або обмежує запити)"
	}
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "тайм-аут з'єднання"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "з'єднання відхилено (перевірте HTTP/HTTPS)"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "вузол недосяжний"
	}
	var re tls.RecordHeaderError
	if errors.As(err, &re) {
		return "помилка TLS (можливо, пристрій працює по HTTP)"
	}
	s := err.Error()
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	return s
}

// apiErrorText витягує текст помилки з відповіді: {"error":"..."} або {"error":{"code","message"}}.
func apiErrorText(data []byte) (string, int) {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		s := strings.TrimSpace(string(data))
		if len(s) > 120 {
			s = s[:120]
		}
		return s, 0
	}
	secs := 0
	if raw, ok := m["seconds"]; ok {
		_ = json.Unmarshal(raw, &secs)
	}
	if raw, ok := m["error"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, secs
		}
		var o struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &o) == nil {
			if o.Message != "" {
				return o.Message, secs
			}
			return o.Code, secs
		}
	}
	return "", secs
}

// LoginError — результат невдалого входу з категорією для статистики.
type LoginError struct {
	Kind    Status
	Msg     string
	Seconds int
}

func (e *LoginError) Error() string { return e.Msg }

// Login виконує вхід. Повторна спроба робиться лише якщо пристрій не прийняв сам формат запиту
// (404/405 або «відсутні параметри»), але ніколи після відмови через невірний пароль.
func (c *Client) Login(user, hash string) error {
	ep, ok := c.op("login")
	if !ok {
		return &LoginError{Kind: StUnsupported, Msg: "у профілі " + c.prof.Name + " немає операції login"}
	}
	first := ep.Mode
	if first != "json" {
		first = "query"
	}
	second := "json"
	if first == "json" {
		second = "query"
	}
	for i, mode := range []string{first, second} {
		u := c.fullURL(ep)
		var code int
		var data []byte
		var err error
		if mode == "json" {
			method := "POST"
			if i == 0 && ep.Method != "" {
				method = ep.Method
			}
			body, _ := json.Marshal(map[string]string{"username": user, "password": hash})
			code, data, err = c.request(method, u, nil, body, "application/json")
		} else {
			method := "GET"
			if i == 0 && ep.Method != "" {
				method = ep.Method
			}
			code, data, err = c.request(method, u, url.Values{"username": {user}, "password": {hash}}, nil, "")
		}
		if err != nil {
			return &LoginError{Kind: StError, Msg: netErrText(err)}
		}
		msg, secs := apiErrorText(data)
		low := strings.ToLower(msg)
		switch {
		case code == http.StatusOK:
			var r struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(data, &r) != nil || r.Token == "" {
				return &LoginError{Kind: StError, Msg: "пристрій не повернув токен"}
			}
			c.token = r.Token
			return nil
		case code == http.StatusTooManyRequests:
			return &LoginError{Kind: StRateLimited, Seconds: secs, Msg: "забагато спроб входу"}
		case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
			if i == 0 {
				continue
			}
			return &LoginError{Kind: StUnsupported, Msg: fmt.Sprintf("пристрій не приймає запит входу (HTTP %d)", code)}
		case strings.Contains(low, "disabled"):
			return &LoginError{Kind: StAuthFail, Msg: "обліковий запис вимкнено"}
		case code == 400 && i == 0 && (strings.Contains(low, "missed") || strings.Contains(low, "json") || strings.Contains(low, "validation")):
			// Пристрій не розпізнав формат запиту — пробуємо інший формат.
			continue
		case code == 400 || code == 401 || code == 403:
			return &LoginError{Kind: StAuthFail, Msg: "невірний логін або пароль"}
		default:
			return &LoginError{Kind: StError, Msg: strings.TrimSpace(fmt.Sprintf("HTTP %d %s", code, msg))}
		}
	}
	return &LoginError{Kind: StError, Msg: "пристрій не прийняв жоден формат запиту входу"}
}

// DevInfo — відповідь /api/info.
type DevInfo struct {
	FirmwareVersion string         `json:"firmware_version"`
	APIVersion      string         `json:"api_version"`
	DeviceName      string         `json:"device_name"`
	DeviceType      string         `json:"device_type"`
	Serial          string         `json:"device_serial_number"`
	Raw             map[string]any `json:"-"`
}

// Info запитує /api/info (без входу).
func (c *Client) Info() (*DevInfo, error) {
	ep, ok := c.op("info")
	if !ok {
		ep = Endpoint{Method: "GET", Path: "/api/info"}
	}
	method := ep.Method
	if method == "" {
		method = "GET"
	}
	code, data, err := c.request(method, c.fullURL(ep), nil, nil, "")
	if err != nil {
		return nil, errors.New(netErrText(err))
	}
	if code != http.StatusOK {
		msg, _ := apiErrorText(data)
		if code == 401 {
			return nil, errors.New("пристрій вимагає входу для /api/info")
		}
		return nil, fmt.Errorf("HTTP %d %s", code, msg)
	}
	var info DevInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, errors.New("некоректна відповідь /api/info")
	}
	_ = json.Unmarshal(data, &info.Raw)
	return &info, nil
}
