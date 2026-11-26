package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Endpoint — опис одного виклику API.
// Шлях, що починається з "/api/", використовується як є; інакше до нього додається Prefix профілю.
// У шляху можна використовувати {lock} — номер замка.
type Endpoint struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Mode   string   `json:"mode,omitempty"`  // login/паролі: "json" або "query"
	Alt    []string `json:"alt,omitempty"`   // запасні варіанти "МЕТОД /шлях", пробуються лише при 404/405
	Field  string   `json:"field,omitempty"` // ім'я поля файлу для multipart-завантаження
	Old    string   `json:"old,omitempty"`   // ім'я параметра старого пароля (порожньо — не потрібен)
	New    string   `json:"new,omitempty"`   // ім'я параметра нового пароля
	Hash   bool     `json:"hash,omitempty"`  // передавати MD5 замість відкритого пароля
}

// Profile — набір викликів для сімейства пристроїв.
type Profile struct {
	Name   string              `json:"-"`
	Prefix string              `json:"prefix"`
	Ops    map[string]Endpoint `json:"ops"`
}

// ProfileRule — правило вибору профілю за типом пристрою та старшою цифрою версії API.
type ProfileRule struct {
	DeviceType string `json:"device_type"`
	APIMajor   string `json:"api_major"`
	Profile    string `json:"profile"`
}

type EndpointConfig struct {
	Profiles map[string]*Profile `json:"profiles"`
	Rules    []ProfileRule       `json:"rules"`
}

// commonOps — виклики, однакові для Android-панелей та Camdroid-пристроїв (за документацією BAS-IP).
func commonOps() map[string]Endpoint {
	return map[string]Endpoint{
		"info":          {Method: "GET", Path: "/api/info"},
		"door_status":   {Method: "GET", Path: "/access/door/status"},
		"open":          {Method: "GET", Path: "/access/general/lock/open/remote/accepted/{lock}"},
		"emergency_on":  {Method: "POST", Path: "/access/general/lock/open/emergency"},
		"emergency_off": {Method: "POST", Path: "/access/general/lock/close/emergency"},
		"backup_settings": {Method: "GET", Path: "/system/settings/backup/all"},
		"restore_settings": {Method: "POST", Path: "/system/settings/backup/all", Field: "file"},
		// Шлях перезавантаження в документації не знайдено — перебираються варіанти.
		"reboot": {Method: "GET", Path: "/system/reboot/run",
			Alt: []string{"POST /system/reboot/run", "GET /system/reboot", "POST /system/reboot"}},
		"log_get":   {Method: "GET", Path: "/system/debug/log"},
		"log_clear": {Method: "DELETE", Path: "/system/debug/log"},
		"manager_status": {Method: "GET", Path: "/security/manager",
			Alt: []string{"GET /security/manager/status", "GET /security/password/manager"}},
	}
}

func withOps(base map[string]Endpoint, extra map[string]Endpoint) map[string]Endpoint {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// defaultConfig — вбудовані профілі.
func defaultConfig() *EndpointConfig {
	return &EndpointConfig{
		Profiles: map[string]*Profile{
			// Багатоабонентські Android-панелі, API 2.x (AA-12FB). Вхід перевірено на парку.
			"android-v2": {
				Prefix: "/api/v1",
				Ops: withOps(commonOps(), map[string]Endpoint{
					"login":          {Method: "POST", Path: "/login", Mode: "json"},
					"backup_users":   {Method: "GET", Path: "/system/settings/tables"},
					"restore_users":  {Method: "POST", Path: "/system/settings/tables", Field: "file"},
					"passwd_admin":   {Method: "POST", Path: "/security/password/admin", Mode: "json", Old: "old_password", New: "new_password"},
					"passwd_manager": {Method: "POST", Path: "/security/password/manager", Mode: "json", New: "new_password"},
				}),
			},
			// Camdroid-пристрої, API 1.x (контролер CR-02BD). Вхід перевірено на парку.
			"controller-v1": {
				Prefix: "/api/v1",
				Ops: withOps(commonOps(), map[string]Endpoint{
					"login":          {Method: "GET", Path: "/login", Mode: "query"},
					"backup_users":   {Method: "GET", Path: "/system/settings/export/tables"},
					"restore_users":  {Method: "POST", Path: "/system/settings/import/tables", Field: "file", Alt: []string{"POST /system/settings/export/tables"}},
					"passwd_admin":   {Method: "POST", Path: "/security/password/admin", Mode: "query", Old: "oldPassword", New: "newPassword"},
					"passwd_manager": {Method: "POST", Path: "/security/password/manager", Mode: "query", New: "newPassword"},
				}),
			},
			// Старі панелі з API /v0.
			"legacy-v0": {
				Prefix: "/api/v0",
				Ops: map[string]Endpoint{
					"login": {Method: "GET", Path: "/login", Mode: "query"},
					"info":  {Method: "GET", Path: "/api/info"},
				},
			},
		},
		Rules: []ProfileRule{
			{DeviceType: "panel", APIMajor: "2", Profile: "android-v2"},
			{DeviceType: "panel", APIMajor: "1", Profile: "legacy-v0"},
			{DeviceType: "panel", APIMajor: "0", Profile: "legacy-v0"},
			{DeviceType: "accesscontroller", Profile: "controller-v1"},
			{Profile: "controller-v1"},
		},
	}
}

// LoadEndpointConfig завантажує вбудовані профілі та накладає зміни з endpoints.json.
// Пошук файлу: явно заданий шлях, потім поруч з програмою, потім у поточному каталозі.
func LoadEndpointConfig(explicit string) (*EndpointConfig, error) {
	cfg := defaultConfig()
	for name, p := range cfg.Profiles {
		p.Name = name
	}
	path := explicit
	if path == "" {
		var candidates []string
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), "endpoints.json"))
		}
		candidates = append(candidates, "endpoints.json")
		for _, c := range candidates {
			if fileExists(c) {
				path = c
				break
			}
		}
	}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var user EndpointConfig
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	for name, up := range user.Profiles {
		if up == nil {
			continue
		}
		p, ok := cfg.Profiles[name]
		if !ok {
			p = &Profile{Name: name, Ops: map[string]Endpoint{}}
			cfg.Profiles[name] = p
		}
		if up.Prefix != "" {
			p.Prefix = up.Prefix
		}
		for op, ep := range up.Ops {
			p.Ops[op] = ep
		}
	}
	for _, r := range user.Rules {
		if _, ok := cfg.Profiles[r.Profile]; !ok {
			return nil, fmt.Errorf("%s: правило посилається на невідомий профіль %q", path, r.Profile)
		}
	}
	cfg.Rules = append(append([]ProfileRule{}, user.Rules...), cfg.Rules...)
	logf("профілі API доповнено з файлу %s", path)
	return cfg, nil
}

func apiMajor(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(v), "v"))
	if k := strings.IndexByte(v, '.'); k >= 0 {
		return v[:k]
	}
	return v
}

// ProfileFor вибирає профіль для пристрою.
func (c *EndpointConfig) ProfileFor(d *Device) *Profile {
	major := apiMajor(d.APIVersion)
	for _, r := range c.Rules {
		if r.DeviceType != "" && !strings.EqualFold(r.DeviceType, d.Type) {
			continue
		}
		if r.APIMajor != "" && r.APIMajor != major {
			continue
		}
		if p, ok := c.Profiles[r.Profile]; ok {
			return p
		}
	}
	return c.Profiles["controller-v1"]
}
