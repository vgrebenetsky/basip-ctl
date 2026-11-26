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
	// Enabled — ім'я поля стану облікового запису (manager), яке передається разом з новим паролем.
	Enabled string `json:"enabled,omitempty"`
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
		// Підтверджено документацією: відновлення — POST multipart на той самий шлях.
		"restore_settings": {Method: "POST", Path: "/system/settings/backup/all", Field: "file"},
		// Шлях перезавантаження в документації не знайдено — перебираються варіанти.
		"reboot": {Method: "GET", Path: "/system/reboot/run",
			Alt: []string{"POST /system/reboot/run", "GET /system/reboot", "POST /system/reboot"}},
		// Налагоджувальний журнал: завантаження (для --save) та очищення. Перевірено на панелі .61.
		"log_get":        {Method: "GET", Path: "/system/debug/log"},
		"debuglog_clear": {Method: "DELETE", Path: "/system/debug/log"},
		// Системний журнал (syslog): підтверджено документацією AA-12FB — GET /syslog/clear.
		"log_clear": {Method: "GET", Path: "/syslog/clear", Alt: []string{"DELETE /system/debug/log"}},
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
					// Підтверджено документацією: GET/POST /system/settings/tables.
					"backup_users":  {Method: "GET", Path: "/system/settings/tables"},
					"restore_users": {Method: "POST", Path: "/system/settings/tables", Field: "file"},
					// Документація AA-12FB: POST /security/password/admin, JSON {old_password, new_password}.
					"passwd_admin": {Method: "POST", Path: "/security/password/admin", Mode: "json", Old: "old_password", New: "new_password",
						Alt: []string{"POST /security/password/web/admin"}},
					// Документація AA-12FB: POST /security/password/manager, JSON {is_enabled, new_password}.
					"passwd_manager": {Method: "POST", Path: "/security/password/manager", Mode: "json", New: "new_password", Enabled: "is_enabled"},
					// Документація AA-12FB: GET /security/password/manager — стан облікового запису manager.
					"manager_status": {Method: "GET", Path: "/security/password/manager"},
				}),
			},
			// Camdroid-пристрої, API 1.x (контролер CR-02BD). Вхід перевірено на парку.
			"controller-v1": {
				Prefix: "/api/v1",
				Ops: withOps(commonOps(), map[string]Endpoint{
					"login": {Method: "GET", Path: "/login", Mode: "query"},
					// Документація: GET /system/settings/export/tables. На прошивці 3.13.2 → 404, тому є запасні варіанти.
					"backup_users": {Method: "GET", Path: "/system/settings/export/tables",
						Alt: []string{"GET /system/settings/tables", "GET /system/settings/backup/tables"}},
					"restore_users": {Method: "POST", Path: "/system/settings/export/tables", Field: "file",
						Alt: []string{"POST /system/settings/import/tables", "POST /system/settings/tables"}},
					// Документація: POST з параметрами oldPassword/newPassword. На прошивці 3.13.2 → 404.
					"passwd_admin": {Method: "POST", Path: "/security/password/admin", Mode: "query", Old: "oldPassword", New: "newPassword",
						Alt: []string{"POST /security/password/web/admin", "PATCH /security/password/admin", "PUT /security/password/admin"}},
					"passwd_manager": {Method: "POST", Path: "/security/password/manager", Mode: "query", New: "newPassword",
						Alt: []string{"POST /security/password/web/manager", "PATCH /security/password/manager", "PUT /security/password/manager"}},
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

// Операції, яких немає у пристроїв певного профілю (за документацією BAS-IP).
var absentOps = map[string][]string{
	// У Camdroid-пристроїв (CR-02BD) немає системного (налагоджувального) журналу.
	"controller-v1": {"log_get", "log_clear", "debuglog_clear"},
}

// LoadEndpointConfig завантажує вбудовані профілі та накладає зміни з endpoints.json.
// Пошук файлу: явно заданий шлях, потім поруч з програмою, потім у поточному каталозі.
func LoadEndpointConfig(explicit string) (*EndpointConfig, error) {
	cfg := defaultConfig()
	for name, p := range cfg.Profiles {
		p.Name = name
		for _, op := range absentOps[name] {
			delete(p.Ops, op)
		}
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

// opTitles — назви операцій для команди api-map.
var opTitles = []struct{ key, title string }{
	{"login", "вхід"},
	{"info", "інформація"},
	{"door_status", "стан дверей"},
	{"open", "відкрити замок"},
	{"emergency_on", "аварійний режим: увімк."},
	{"emergency_off", "аварійний режим: вимк."},
	{"reboot", "перезавантаження"},
	{"backup_settings", "копія налаштувань"},
	{"restore_settings", "відновлення налаштувань"},
	{"backup_users", "копія даних користувачів"},
	{"restore_users", "відновлення даних корист."},
	{"log_get", "налагоджувальний журнал: отримати"},
	{"log_clear", "системний журнал (syslog): очистити"},
	{"debuglog_clear", "налагоджувальний журнал: очистити"},
	{"manager_status", "стан облікового запису manager"},
	{"passwd_admin", "пароль admin"},
	{"passwd_manager", "пароль manager"},
}

// cmdAPIMap друкує, які запити програма надсилає пристроям кожного профілю
// (основний варіант і запасні, що пробуються при 404/405).
func cmdAPIMap(opt *Options) int {
	cfg, err := LoadEndpointConfig(opt.Values["endpoints"])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка:", err)
		return 2
	}
	names := []string{"android-v2", "controller-v1", "legacy-v0"}
	for n := range cfg.Profiles {
		known := false
		for _, k := range names {
			known = known || k == n
		}
		if !known {
			names = append(names, n)
		}
	}
	for _, n := range names {
		p := cfg.Profiles[n]
		if p == nil {
			continue
		}
		var who []string
		for _, r := range cfg.Rules {
			if r.Profile == n && r.DeviceType != "" {
				w := r.DeviceType
				if r.APIMajor != "" {
					w += " API " + r.APIMajor + ".x"
				}
				who = append(who, w)
			}
		}
		fmt.Printf("\n=== Профіль %s (%s), префікс %s\n", n, strings.Join(who, ", "), p.Prefix)
		for _, t := range opTitles {
			ep, ok := p.Ops[t.key]
			if !ok {
				fmt.Printf("  %-32s —  немає\n", t.title)
				continue
			}
			m := ep.Method
			if m == "" {
				m = "GET"
			}
			fmt.Printf("  %-32s %s %s\n", t.title, m, ep.Path)
			for _, a := range ep.Alt {
				fmt.Printf("  %-32s   запасний: %s\n", "", a)
			}
		}
	}
	return 0
}
