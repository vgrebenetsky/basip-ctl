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

// errExportLocked — панель повідомляє, що експорт «ще триває», хоча насправді він завис
// після збою (HTTP 500). Знімається лише перезавантаженням пристрою.
var errExportLocked = errors.New("експорт заблоковано попереднім збоєм — потрібне перезавантаження пристрою")

// Download завантажує файл (архів). Відповідь у форматі JSON вважається помилкою.
// Повторів після HTTP 5xx немає: на панелях збій залишає експорт «у процесі» і повтор
// лише отримує HTTP 400. На HTTP 400 «... is being processed» чекаємо до 1 хв
// (раптом експорт справді виконується), далі — errExportLocked.
func (c *Client) Download(op string) ([]byte, error) {
	c.long = true
	// Як у документації BAS-IP: експорт запитується з Accept: application/zip.
	// Ймовірна причина збою на панелях з API 2.21.0: на Accept: application/json вони відповідають HTTP 500 і блокують експорт.
	c.accept = "application/zip"
	defer func() { c.long = false; c.accept = "" }()
	var data []byte
	var hdr http.Header
	var err error
	deadline := time.Now().Add(time.Minute)
	for {
		data, hdr, err = c.Call(op, nil, nil, nil, "")
		var oe *OpError
		if err == nil || !errors.As(err, &oe) {
			break
		}
		if oe.Code == 400 && strings.Contains(strings.ToLower(oe.Msg), "being processed") {
			if time.Now().After(deadline) {
				err = errExportLocked
				break
			}
			logf("%s %s: пристрій повідомляє, що експорт триває, чекаємо 10 с", c.dev.IP, op)
			time.Sleep(10 * time.Second)
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

// Has — чи є операція в профілі пристрою.
func (c *Client) Has(op string) bool {
	_, ok := c.op(op)
	return ok
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
// enabled — стан облікового запису для поля Enabled (nil — поле не передається).
func (c *Client) SetPassword(op, oldPass, newPass string, enabled *bool) error {
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
		// Те саме дублюється в JSON-тілі (old_password/new_password): запасні шляхи
		// на кшталт /security/password/web/admin очікують саме тіло, а не параметри.
		jb := map[string]any{"new_password": val(newPass)}
		if ep.Old != "" {
			jb["old_password"] = val(oldPass)
		}
		_, err := c.CallJSON(op, nil, q, jb)
		return err
	}
	payload := map[string]any{newName: val(newPass)}
	if ep.Old != "" {
		payload[ep.Old] = val(oldPass)
	}
	if ep.Enabled != "" && enabled != nil {
		payload[ep.Enabled] = *enabled
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
	logf("%s: стан manager — відповідь: %s", c.dev.IP, strings.TrimSpace(string(data)))
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
