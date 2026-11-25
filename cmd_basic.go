package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const adminUser = "admin"

func (a *App) printHeader(title string) {
	fmt.Fprintf(a.Out, "%s: пристроїв %d, перевірка доступності — %s\n\n", title, len(a.Devices), a.Pinger.ModeText())
}

// cmdPing — перевірка доступності.
func cmdPing(a *App) int {
	a.printHeader("Перевірка доступності")
	res := a.RunAll(func(d *Device) Result {
		if a.Pinger.Alive(d.IP) {
			return Result{Status: StOK, Detail: "доступний"}
		}
		return Result{Status: StUnreachable}
	})
	return a.Summarize(res)
}

// infoMismatch порівнює дані пристрою з записом у файлі.
func infoMismatch(d *Device, info *DevInfo) []string {
	var diff []string
	if d.Name != "" && info.DeviceName != "" && !strings.EqualFold(d.Name, info.DeviceName) {
		diff = append(diff, fmt.Sprintf("модель %s≠%s", d.Name, info.DeviceName))
	}
	if d.APIVersion != "" && info.APIVersion != "" && d.APIVersion != info.APIVersion {
		diff = append(diff, fmt.Sprintf("API %s≠%s", d.APIVersion, info.APIVersion))
	}
	if d.Type != "" && info.DeviceType != "" && !strings.EqualFold(d.Type, info.DeviceType) {
		diff = append(diff, fmt.Sprintf("тип %s≠%s", d.Type, info.DeviceType))
	}
	return diff
}

// cmdInfo — інформація про пристрій без входу.
func cmdInfo(a *App) int {
	asJSON := a.Opt.Bools["json"]
	if asJSON {
		// JSON іде у stdout, построковий вивід і підсумок — у stderr.
		a.Out = os.Stderr
	}
	a.printHeader("Інформація про пристрої")
	res := a.RunAll(func(d *Device) Result {
		if !a.Pinger.Alive(d.IP) {
			return Result{Status: StUnreachable}
		}
		info, err := a.NewClient(d).Info()
		if err != nil {
			return Result{Status: StError, Detail: err.Error()}
		}
		if d.Name == "" {
			d.Name = info.DeviceName
		}
		detail := fmt.Sprintf("прошивка %s | API %s | тип %s | s/n %s",
			dash(info.FirmwareVersion), dash(info.APIVersion), dash(info.DeviceType), dash(info.Serial))
		if d.InFile {
			if diff := infoMismatch(d, info); len(diff) > 0 {
				return Result{Status: StMismatch, Detail: strings.Join(diff, ", ") + " | " + detail, Data: info}
			}
		}
		return Result{Status: StOK, Detail: detail, Data: info}
	})
	code := a.Summarize(res)
	if asJSON {
		var out []map[string]any
		for _, r := range res {
			item := map[string]any{"ip": r.Dev.IP, "result": statusSingle[r.Status]}
			if info, ok := r.Data.(*DevInfo); ok && info != nil {
				item["info"] = info.Raw
			} else if r.Detail != "" {
				item["error"] = r.Detail
			}
			out = append(out, item)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
	}
	return code
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// connect — спільний крок для команд з входом: ping, уточнення типу пристрою, вхід admin.
// Повертає клієнт з токеном або результат-помилку.
func (a *App) connect(d *Device) (*Client, *Result) {
	if !a.Pinger.Alive(d.IP) {
		return nil, &Result{Status: StUnreachable}
	}
	if !d.HasSecret() {
		return nil, &Result{Status: StNoCreds, Detail: "передайте пароль або хеш через --password"}
	}
	c := a.NewClient(d)
	if !d.InFile || d.Type == "" || d.APIVersion == "" {
		// Пристрій не з файлу — тип і версію API визначаємо через /api/info.
		if info, err := c.Info(); err == nil {
			if d.Type == "" {
				d.Type = info.DeviceType
			}
			if d.Name == "" {
				d.Name = info.DeviceName
			}
			if d.APIVersion == "" {
				d.APIVersion = info.APIVersion
			}
			c.Refresh()
		}
	}
	if err := c.Login(adminUser, d.PasswordHash()); err != nil {
		le, ok := err.(*LoginError)
		if !ok {
			return nil, &Result{Status: StError, Detail: err.Error()}
		}
		detail := le.Msg
		if le.Kind == StRateLimited && le.Seconds > 0 {
			detail = fmt.Sprintf("%s, повторити через %d с", le.Msg, le.Seconds)
		}
		if le.Kind == StAuthFail && le.Msg == "невірний логін або пароль" {
			detail = "" // назва статусу вже все пояснює
		}
		return nil, &Result{Status: le.Kind, Detail: detail}
	}
	return c, nil
}

// cmdAuth — перевірка входу. Нові пристрої (з --password) після успішного входу додаються у файл.
func cmdAuth(a *App) int {
	a.printHeader("Перевірка входу (admin)")
	res := a.RunAll(func(d *Device) Result {
		_, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		return Result{Status: StOK, Detail: "вхід виконано"}
	})
	a.appendNewDevices(res)
	return a.Summarize(res)
}

// appendNewDevices дописує у файл пристрої, яких там не було і на які вдалося увійти.
func (a *App) appendNewDevices(res []Result) {
	if a.File == nil {
		return
	}
	var added []string
	for _, r := range res {
		d := r.Dev
		if r.Status != StOK || d.InFile {
			continue
		}
		d.Secret = d.PasswordHash() // у файл записується хеш, не відкритий пароль
		if d.LockRaw == "" {
			d.LockRaw = "1"
		}
		a.File.AppendDevice(d)
		added = append(added, d.IP)
	}
	if len(added) == 0 {
		return
	}
	bak, err := a.File.Save()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка оновлення файлу пристроїв:", err)
		return
	}
	fmt.Fprintf(a.Out, "\nДо %s додано пристроїв: %d (%s). Резервна копія: %s\n",
		filepath.Base(a.File.Path), len(added), strings.Join(added, ", "), bak)
}
