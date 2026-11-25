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
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Mode   string `json:"mode,omitempty"` // для login: "json" (тіло запиту) або "query" (параметри URL)
}

// Profile — набір викликів для сімейства пристроїв.
type Profile struct {
	Name   string              `json:"-"`
	Prefix string              `json:"prefix"`
	Ops    map[string]Endpoint `json:"ops"`
}

// ProfileRule — правило вибору профілю за типом пристрою та старшою цифрою версії API.
// Порожнє значення поля означає «будь-яке».
type ProfileRule struct {
	DeviceType string `json:"device_type"`
	APIMajor   string `json:"api_major"`
	Profile    string `json:"profile"`
}

type EndpointConfig struct {
	Profiles map[string]*Profile `json:"profiles"`
	Rules    []ProfileRule       `json:"rules"`
}

// defaultConfig — вбудовані профілі. Етап 1: вхід та інформація про пристрій.
// Шляхи решти операцій додаються в наступних етапах після перевірки на тестових пристроях.
func defaultConfig() *EndpointConfig {
	return &EndpointConfig{
		Profiles: map[string]*Profile{
			// Багатоабонентські Android-панелі, API 2.x (AA-12FB).
			"android-v2": {
				Prefix: "/api/v1",
				Ops: map[string]Endpoint{
					"login": {Method: "POST", Path: "/login", Mode: "json"},
					"info":  {Method: "GET", Path: "/api/info"},
				},
			},
			// Контролери доступу, API 1.x (CR-02BD).
			"controller-v1": {
				Prefix: "/api/v1",
				Ops: map[string]Endpoint{
					"login": {Method: "GET", Path: "/login", Mode: "query"},
					"info":  {Method: "GET", Path: "/api/info"},
				},
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
	// Правила з файлу мають пріоритет над вбудованими.
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
