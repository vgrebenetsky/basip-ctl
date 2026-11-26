package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// errNoOp — операції немає у профілі або пристрій відповідає 404/405 на всі варіанти шляху.
var errNoOp = errors.New("операція не підтримується пристроєм")

// OpError — помилка виконання операції з кодом HTTP і текстом від пристрою.
type OpError struct {
	Code int
	Msg  string
}

func (e *OpError) Error() string {
	switch e.Code {
	case 401:
		return "пристрій відхилив запит (потрібен вхід)"
	case 403:
		return "недостатньо прав"
	}
	if e.Msg != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("HTTP %d", e.Code)
}

// variants повертає основний і запасні варіанти (метод, шлях) операції.
func (c *Client) variants(ep Endpoint) [][2]string {
	m := ep.Method
	if m == "" {
		m = "GET"
	}
	res := [][2]string{{strings.ToUpper(m), ep.Path}}
	for _, a := range ep.Alt {
		a = strings.TrimSpace(a)
		if k := strings.IndexByte(a, ' '); k > 0 {
			res = append(res, [2]string{strings.ToUpper(a[:k]), strings.TrimSpace(a[k+1:])})
		} else if a != "" {
			res = append(res, [2]string{res[0][0], a})
		}
	}
	return res
}

// Call виконує операцію профілю. vars підставляються у шлях ({lock}).
// Запасні шляхи пробуються лише коли пристрій відповідає 404/405 (тобто дія не виконана).
func (c *Client) Call(op string, vars map[string]string, q url.Values, body []byte, ctype string) ([]byte, http.Header, error) {
	ep, ok := c.op(op)
	if !ok {
		return nil, nil, errNoOp
	}
	for _, v := range c.variants(ep) {
		path := v[1]
		for k, val := range vars {
			path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(val))
		}
		code, data, hdr, err := c.requestH(v[0], c.fullURL(Endpoint{Path: path}), q, body, ctype)
		if err != nil {
			return nil, nil, errors.New(netErrText(err))
		}
		if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
			continue
		}
		if code >= 200 && code < 300 {
			return data, hdr, nil
		}
		msg, _ := apiErrorText(data)
		return data, hdr, &OpError{Code: code, Msg: msg}
	}
	return nil, nil, errNoOp
}

// CallJSON — виклик з JSON-тілом (або без тіла, якщо payload == nil).
func (c *Client) CallJSON(op string, vars map[string]string, q url.Values, payload any) ([]byte, error) {
	var body []byte
	ctype := ""
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body, ctype = b, "application/json"
	}
	data, _, err := c.Call(op, vars, q, body, ctype)
	return data, err
}

// Download завантажує файл (архів). Відповідь у форматі JSON вважається помилкою.
// Повтори:
//   - HTTP 5xx — до двох повторів з паузою 3 і 6 с;
//   - HTTP 400 «... is being processed» (панель ще формує попередній експорт) —
//     очікування з перевіркою кожні 5 с, загалом до 2 хв.
func (c *Client) Download(op string) ([]byte, error) {
	c.long = true
	defer func() { c.long = false }()
	var data []byte
	var hdr http.Header
	var err error
	fails5xx := 0
	deadline := time.Now().Add(2 * time.Minute)
	for {
		data, hdr, err = c.Call(op, nil, nil, nil, "")
		var oe *OpError
		if err == nil || !errors.As(err, &oe) {
			break
		}
		if oe.Code >= 500 && fails5xx < 2 {
			fails5xx++
			logf("%s %s: збій пристрою, повтор %d", c.dev.IP, op, fails5xx)
			time.Sleep(time.Duration(3*fails5xx) * time.Second)
			continue
		}
		if oe.Code == 400 && strings.Contains(strings.ToLower(oe.Msg), "being processed") {
			if time.Now().After(deadline) {
				err = errors.New("пристрій понад 2 хв повідомляє, що експорт ще триває (ймовірно, завис — допоможе перезавантаження)")
				break
			}
			logf("%s %s: експорт ще триває, чекаємо 5 с", c.dev.IP, op)
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}
	ct := strings.ToLower(hdr.Get("Content-Type"))
	if strings.Contains(ct, "json") || len(data) == 0 {
		msg, _ := apiErrorText(data)
		if msg == "" {
			msg = "пристрій не повернув файл"
		}
		return nil, errors.New(msg)
	}
	logf("%s %s: отримано %d байт", c.dev.IP, op, len(data))
	return data, nil
}

// Upload відправляє файл як multipart/form-data.
func (c *Client) Upload(op, filename string, content []byte) error {
	ep, ok := c.op(op)
	if !ok {
		return errNoOp
	}
	field := ep.Field
	if field == "" {
		field = "file"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	c.long = true
	defer func() { c.long = false }()
	_, _, err = c.Call(op, nil, nil, buf.Bytes(), w.FormDataContentType())
	return err
}

// SetPassword змінює пароль через операцію passwd_admin або passwd_manager.
// Формат параметрів (query/json, імена, MD5) задається профілем.
func (c *Client) SetPassword(op, oldPass, newPass string) error {
	ep, ok := c.op(op)
	if !ok {
		return errNoOp
	}
	val := func(s string) string {
		if ep.Hash {
			return MD5Of(s)
		}
		return s
	}
	newName := ep.New
	if newName == "" {
		newName = "new_password"
	}
	if ep.Mode == "query" {
		q := url.Values{newName: {val(newPass)}}
		if ep.Old != "" {
			q.Set(ep.Old, val(oldPass))
		}
		_, _, err := c.Call(op, nil, q, nil, "")
		return err
	}
	payload := map[string]string{newName: val(newPass)}
	if ep.Old != "" {
		payload[ep.Old] = val(oldPass)
	}
	_, err := c.CallJSON(op, nil, nil, payload)
	return err
}

// NeedsOld — чи вимагає операція зміни пароля старий пароль.
func (c *Client) NeedsOld(op string) bool {
	ep, ok := c.op(op)
	return ok && ep.Old != ""
}

// ManagerStatus повертає стан облікового запису manager: true/false або known=false, якщо визначити не вдалося.
func (c *Client) ManagerStatus() (enabled bool, known bool, err error) {
	data, err := c.CallJSON("manager_status", nil, nil, nil)
	if err != nil {
		return false, false, err
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return false, false, nil
	}
	for _, k := range []string{"enabled", "is_enabled", "enable", "status", "active"} {
		switch v := m[k].(type) {
		case bool:
			return v, true, nil
		case string:
			s := strings.ToLower(v)
			if s == "enabled" || s == "on" || s == "true" || s == "active" {
				return true, true, nil
			}
			if s == "disabled" || s == "off" || s == "false" || s == "inactive" {
				return false, true, nil
			}
		}
	}
	if v, ok := m["disabled"].(bool); ok {
		return !v, true, nil
	}
	return false, false, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
