package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// pickPasswordCheck: якщо серед вибраних є Android-панелі — суворі вимоги, інакше базові.
func (a *App) pickPasswordCheck(targets []*Device) func(string) error {
	for _, d := range targets {
		if a.Cfg.ProfileFor(d).Name == "android-v2" {
			return checkStrong
		}
	}
	return checkBasic
}

// ---------- passwd-manager ----------

func cmdPasswdManager(a *App) int {
	targets := a.eligible(nil)
	if !a.planned("Змінити пароль manager", targets) {
		return 0
	}
	newPass, err := askNewPassword("BASIP_NEW_PASSWORD", "Новий пароль manager", a.pickPasswordCheck(targets))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка:", err)
		return 2
	}
	force := a.Opt.Bools["force"]
	a.printHeader("Зміна пароля manager")
	res := a.RunAll(func(d *Device) Result {
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		note := ""
		enabled, known, err := c.ManagerStatus()
		switch {
		case err == nil && known && !enabled && !force:
			return Result{Status: StSkipped, Detail: "обліковий запис manager вимкнено (--force — змінити все одно)"}
		case err == nil && known && !enabled:
			note = " (manager вимкнено, змінено з --force)"
		case err == nil && known:
			note = " (manager увімкнено)"
		default:
			note = " (стан manager визначити не вдалося)"
		}
		// Стан облікового запису не змінюємо: передаємо поточний, якщо його вдалося визначити.
		var keep *bool
		if err == nil && known {
			keep = &enabled
		}
		if err := c.SetPassword("passwd_manager", "", newPass, keep); err != nil {
			return opFail(err)
		}
		return Result{Status: StOK, Detail: "пароль manager змінено" + note}
	})
	return a.Summarize(res)
}

// ---------- passwd-admin ----------

func cmdPasswdAdmin(a *App) int {
	if a.File == nil {
		fmt.Fprintln(os.Stderr, "Помилка: для зміни пароля admin потрібен файл пристроїв (у нього записується новий хеш)")
		return 2
	}
	targets := a.eligible(nil)
	if !a.planned("Змінити пароль admin", targets) {
		return 0
	}
	// Чи є серед вибраних пристрої, прошивка яких вимагає старий пароль.
	needOld := false
	for _, d := range targets {
		if a.NewClient(d).NeedsOld("passwd_admin") {
			needOld = true
			break
		}
	}
	oldPass := ""
	if needOld {
		oldPass = os.Getenv("BASIP_OLD_PASSWORD")
		if oldPass == "" {
			p, err := readSecret("Поточний пароль admin (потрібен частині прошивок): ")
			if err != nil || p == "" {
				fmt.Fprintln(os.Stderr, "Помилка: поточний пароль не введено")
				return 2
			}
			oldPass = p
		}
	}
	newPass, err := askNewPassword("BASIP_NEW_PASSWORD", "Новий пароль admin", a.pickPasswordCheck(targets))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка:", err)
		return 2
	}
	oldHash, newHash := MD5Of(oldPass), MD5Of(newPass)

	a.printHeader("Зміна пароля admin")
	var mu sync.Mutex
	var changed []*Device
	res := a.RunAll(func(d *Device) Result {
		if d.SkipAuth() {
			return Result{Status: StSkipped, Detail: skipOKText}
		}
		c := a.NewClient(d)
		if c.NeedsOld("passwd_admin") && !strings.EqualFold(d.PasswordHash(), oldHash) {
			// Старий пароль підходить лише до пристроїв з відповідним хешем — інші не чіпаємо.
			return Result{Status: StSkipped, Detail: "старий пароль не збігається з хешем у файлі"}
		}
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		if err := c.SetPassword("passwd_admin", oldPass, newPass, nil); err != nil {
			return opFail(err)
		}
		// Перевірка: вхід з новим паролем. Якщо не вдався — перевіряємо, чи діє старий.
		chk := a.NewClient(d)
		if err := chk.Login(adminUser, newHash); err != nil {
			chk2 := a.NewClient(d)
			if err2 := chk2.Login(adminUser, d.PasswordHash()); err2 == nil {
				return Result{Status: StError, Detail: "пристрій відповів успіхом, але пароль не змінився"}
			}
			return Result{Status: StError, Detail: "УВАГА: вхід ні з новим, ні зі старим паролем не вдався — перевірте пристрій вручну"}
		}
		mu.Lock()
		changed = append(changed, d)
		mu.Unlock()
		return Result{Status: StOK, Detail: "пароль admin змінено, вхід з новим паролем перевірено"}
	})
	if len(changed) > 0 {
		var ips []string
		for _, d := range changed {
			if d.InFile {
				a.File.SetSecret(d, newHash)
			} else {
				d.Secret = newHash
				a.File.AppendDevice(d)
			}
			ips = append(ips, d.IP)
		}
		sortIPs(ips)
		bak, err := a.File.Save()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ПОМИЛКА оновлення файлу пристроїв:", err)
			fmt.Fprintln(os.Stderr, "Новий хеш для пристроїв", strings.Join(ips, ", "), "—", newHash)
		} else {
			fmt.Fprintf(a.Out, "\nПароль змінено на %d пристроях. Хеш оновлено у %s (резервна копія: %s)\n",
				len(changed), filepath.Base(a.File.Path), bak)
		}
	}
	return a.Summarize(res)
}

