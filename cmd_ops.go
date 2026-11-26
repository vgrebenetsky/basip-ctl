package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// opFail перетворює помилку операції на результат.
func opFail(err error) Result {
	if errors.Is(err, errNoOp) {
		return Result{Status: StUnsupported, Detail: "операція недоступна на цьому пристрої"}
	}
	return Result{Status: StError, Detail: err.Error()}
}

// planned друкує перелік пристроїв, з якими буде виконано дію (для підтвердження та --dry-run).
// Повертає false, якщо виконувати не треба.
func (a *App) planned(action string, targets []*Device) bool {
	if len(targets) == 0 {
		fmt.Fprintln(a.Out, "Немає пристроїв, до яких можна застосувати дію (перевірте позначку OK та колонку emergency у файлі).")
		logf("%s: немає пристроїв, до яких можна застосувати дію", action)
		return false
	}
	var ips []string
	for _, d := range targets {
		ips = append(ips, d.IP)
	}
	sortIPs(ips)
	list := strings.Join(ips, ", ")
	if len(ips) > 20 {
		list = strings.Join(ips[:20], ", ") + fmt.Sprintf(" … (ще %d)", len(ips)-20)
	}
	question := fmt.Sprintf("%s: пристроїв %d (%s)", action, len(targets), list)
	if a.Opt.Bools["dry-run"] {
		fmt.Fprintln(a.Out, "[перевірка без змін] "+question)
		return false
	}
	return a.confirm(question)
}

// eligible — пристрої, до яких застосовується дія (без позначки OK та з урахуванням фільтра).
func (a *App) eligible(filter func(*Device) bool) []*Device {
	var res []*Device
	for _, d := range a.Devices {
		if d.SkipAuth() {
			continue
		}
		if filter != nil && !filter(d) {
			continue
		}
		res = append(res, d)
	}
	return res
}

func locksText(locks []int) string {
	var s []string
	for _, l := range locks {
		if l == 0 {
			s = append(s, "усі")
		} else {
			s = append(s, strconv.Itoa(l))
		}
	}
	return strings.Join(s, ",")
}

// ---------- door-status ----------

type doorState struct {
	Status  string `json:"status"`
	Open    int    `json:"opentime"`
	TooLong bool   `json:"is_timeout_exceed"`
	Mode    string `json:"mode"`
}

func cmdDoorStatus(a *App) int {
	a.printHeader("Стан дверей")
	var mu sync.Mutex
	counts := map[string]int{}
	res := a.RunAll(func(d *Device) Result {
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		data, err := c.CallJSON("door_status", nil, nil, nil)
		if err != nil {
			var oe *OpError
			if errors.As(err, &oe) && strings.Contains(strings.ToLower(oe.Msg), "not activated") {
				mu.Lock()
				counts["датчик не активовано"]++
				mu.Unlock()
				return Result{Status: StSkipped, Detail: "датчик дверей не активовано"}
			}
			return opFail(err)
		}
		logf("%s door-status: %s", d.IP, strings.TrimSpace(string(data)))
		var st doorState
		if json.Unmarshal(data, &st) != nil {
			return Result{Status: StError, Detail: "некоректна відповідь пристрою"}
		}
		key, text := "", ""
		switch {
		case strings.EqualFold(st.Status, "closed"):
			key, text = "зачинено", "зачинено"
		case !strings.EqualFold(st.Status, "open"):
			key, text = "невідомий стан", "стан пристрою: "+st.Status
		case st.TooLong:
			key, text = "відчинено задовго", fmt.Sprintf("відчинено задовго (%d с)", st.Open)
		default:
			key, text = "відчинено", fmt.Sprintf("відчинено %d с", st.Open)
		}
		if st.Mode != "" && st.Mode != "door_sensor" {
			text += " (режим " + st.Mode + ")"
		}
		mu.Lock()
		counts[key]++
		mu.Unlock()
		return Result{Status: StOK, Detail: text}
	})
	var parts []string
	for _, k := range []string{"зачинено", "відчинено", "відчинено задовго", "невідомий стан", "датчик не активовано"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintln(a.Out, "\nДвері: "+strings.Join(parts, " | "))
	}
	return a.Summarize(res)
}

// ---------- open ----------

func cmdOpen(a *App) int {
	q := url.Values{}
	if v, ok := a.Opt.Values["unlock-time"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 604800 {
			fmt.Fprintln(os.Stderr, "Помилка: --unlock-time — секунди від 0 до 604800")
			return 2
		}
		q.Set("unlockTime", strconv.Itoa(n))
	}
	if !a.planned("Відкрити замки", a.eligible(nil)) {
		return 0
	}
	a.printHeader("Відкриття замків")
	res := a.RunAll(func(d *Device) Result {
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		for _, l := range d.Locks {
			if _, _, err := c.Call("open", map[string]string{"lock": strconv.Itoa(l)}, q, nil, ""); err != nil {
				r := opFail(err)
				r.Detail = fmt.Sprintf("замок %s: %s", locksText([]int{l}), r.Detail)
				return r
			}
		}
		return Result{Status: StOK, Detail: "відкрито замки: " + locksText(d.Locks)}
	})
	return a.Summarize(res)
}

// ---------- emergency ----------

func emergencyAllowed(d *Device) bool { return d.Emergency }

func cmdEmergencyOn(a *App) int {
	dur := 43200
	if v, ok := a.Opt.Values["duration"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 604800 {
			fmt.Fprintln(os.Stderr, "Помилка: --duration — секунди від 1 до 604800")
			return 2
		}
		dur = n
	}
	if dur > 86400 {
		fmt.Fprintln(os.Stderr, "Попередження: частина прошивок приймає не більше 86400 с (24 год).")
	}
	if !a.planned(fmt.Sprintf("Увімкнути аварійний режим на %s", humanDur(dur)), a.eligible(emergencyAllowed)) {
		return 0
	}
	a.printHeader("Увімкнення аварійного режиму")
	res := a.RunAll(func(d *Device) Result {
		if d.SkipAuth() {
			return Result{Status: StSkipped, Detail: skipOKText}
		}
		if !d.Emergency {
			return Result{Status: StSkipped, Detail: "аварійний режим не дозволено у файлі"}
		}
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		type lockItem struct {
			Lock int `json:"lock_number"`
			Time int `json:"unlock_time"`
		}
		var locks []lockItem
		for _, l := range d.Locks {
			locks = append(locks, lockItem{l, dur})
		}
		if _, err := c.CallJSON("emergency_on", nil, nil, map[string]any{"locks": locks}); err != nil {
			return opFail(err)
		}
		return Result{Status: StOK, Detail: fmt.Sprintf("аварійний режим увімкнено, замки %s, %s", locksText(d.Locks), humanDur(dur))}
	})
	return a.Summarize(res)
}

func cmdEmergencyOff(a *App) int {
	if !a.planned("Вимкнути аварійний режим", a.eligible(emergencyAllowed)) {
		return 0
	}
	a.printHeader("Вимкнення аварійного режиму")
	res := a.RunAll(func(d *Device) Result {
		if d.SkipAuth() {
			return Result{Status: StSkipped, Detail: skipOKText}
		}
		if !d.Emergency {
			return Result{Status: StSkipped, Detail: "аварійний режим не дозволено у файлі"}
		}
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		if _, err := c.CallJSON("emergency_off", nil, nil, map[string]any{"locks": d.Locks}); err != nil {
			return opFail(err)
		}
		return Result{Status: StOK, Detail: "аварійний режим вимкнено, замки " + locksText(d.Locks)}
	})
	return a.Summarize(res)
}

func humanDur(sec int) string {
	switch {
	case sec%3600 == 0:
		return fmt.Sprintf("%d год", sec/3600)
	case sec%60 == 0:
		return fmt.Sprintf("%d хв", sec/60)
	}
	return fmt.Sprintf("%d с", sec)
}

// ---------- reboot ----------

// waitBack чекає, поки пристрій після перезавантаження знову відповість на /api/info.
func (a *App) waitBack(d *Device, limit time.Duration) bool {
	time.Sleep(15 * time.Second)
	deadline := time.Now().Add(limit)
	c := a.NewClient(d)
	for time.Now().Before(deadline) {
		if _, err := c.Info(); err == nil {
			return true
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

func cmdReboot(a *App) int {
	if !a.planned("Перезавантажити", a.eligible(nil)) {
		return 0
	}
	wait := a.Opt.Bools["wait"]
	a.printHeader("Перезавантаження")
	res := a.RunAll(func(d *Device) Result {
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		if _, _, err := c.Call("reboot", nil, nil, nil, ""); err != nil {
			// Пристрій може обірвати з'єднання, вже почавши перезавантаження.
			if !strings.Contains(err.Error(), "розірвав") {
				return opFail(err)
			}
		}
		if !wait {
			return Result{Status: StOK, Detail: "команду перезавантаження надіслано"}
		}
		if a.waitBack(d, 5*time.Minute) {
			return Result{Status: StOK, Detail: "перезавантажено, пристрій знову в мережі"}
		}
		return Result{Status: StError, Detail: "пристрій не повернувся в мережу за 5 хв"}
	})
	return a.Summarize(res)
}

// ---------- clear-logs ----------

func cmdClearLogs(a *App) int {
	save := a.Opt.Bools["save"]
	if !a.planned("Очистити системні журнали", a.eligible(nil)) {
		return 0
	}
	a.printHeader("Очищення системних журналів")
	arch := newArchive("logs")
	res := a.RunAll(func(d *Device) Result {
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		if !c.Has("log_clear") {
			return Result{Status: StUnsupported, Detail: "системного журналу на цьому типі пристроїв немає"}
		}
		if save {
			data, err := c.Download("log_get")
			if err != nil {
				r := opFail(err)
				r.Detail = "журнал не збережено, очищення скасовано: " + r.Detail
				return r
			}
			arch.Add(d, "system-log.zip", data)
		}
		if _, _, err := c.Call("log_clear", nil, nil, nil, ""); err != nil {
			return opFail(err)
		}
		detail := "syslog очищено"
		// Налагоджувальний журнал очищається додатково; його збій не скасовує результат.
		if c.Has("debuglog_clear") {
			if _, _, err := c.Call("debuglog_clear", nil, nil, nil, ""); err != nil {
				detail += "; налагоджувальний журнал не очищено: " + opFail(err).Detail
			} else {
				detail += ", налагоджувальний журнал очищено"
			}
		}
		if save {
			detail = "налагоджувальний журнал збережено; " + detail
		}
		return Result{Status: StOK, Detail: detail}
	})
	if save {
		a.writeArchive(arch, "Журнали збережено")
	}
	return a.Summarize(res)
}
