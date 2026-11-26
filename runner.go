package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Status — підсумковий стан обробки пристрою.
type Status int

const (
	StOK Status = iota
	StUnreachable
	StNoCreds
	StAuthFail
	StRateLimited
	StMismatch
	StUnsupported
	StSkipped
	StError
)

// Порядок і назви для підсумку (множина) та для рядка пристрою (однина).
var statusOrder = []Status{StOK, StUnreachable, StNoCreds, StAuthFail, StRateLimited, StMismatch, StUnsupported, StSkipped, StError}

var statusPlural = map[Status]string{
	StOK:          "успішно",
	StUnreachable: "недоступні",
	StNoCreds:     "немає облікових даних",
	StAuthFail:    "помилка входу",
	StRateLimited: "тимчасово заблоковано",
	StMismatch:    "розбіжність з файлом",
	StUnsupported: "не підтримується",
	StSkipped:     "пропущено",
	StError:       "помилка",
}

var statusSingle = map[Status]string{
	StOK:          "успішно",
	StUnreachable: "недоступний",
	StNoCreds:     "немає облікових даних",
	StAuthFail:    "помилка входу",
	StRateLimited: "тимчасово заблоковано",
	StMismatch:    "розбіжність з файлом",
	StUnsupported: "не підтримується",
	StSkipped:     "пропущено",
	StError:       "помилка",
}

func statusMark(s Status) string {
	switch s {
	case StOK:
		return "✔"
	case StSkipped:
		return "–"
	case StMismatch:
		return "⚠"
	}
	return "✖"
}

// Result — результат обробки одного пристрою.
type Result struct {
	Dev    *Device
	Status Status
	Detail string
	Data   any
}

var printMu sync.Mutex

func (a *App) printResult(r Result) {
	text := statusSingle[r.Status]
	if r.Detail != "" {
		if r.Status == StOK {
			text = r.Detail
		} else {
			text += ": " + r.Detail
		}
	}
	printMu.Lock()
	defer printMu.Unlock()
	fmt.Fprintf(a.Out, "%-15s  %-10s  %s %s\n", r.Dev.IP, r.Dev.Model(), statusMark(r.Status), text)
}

// RunAll обробляє пристрої паралельно (не більше --workers одночасно) і друкує рядок на кожен.
func (a *App) RunAll(fn func(*Device) Result) []Result {
	results := make([]Result, len(a.Devices))
	sem := make(chan struct{}, a.Opt.Workers)
	var wg sync.WaitGroup
	for i, d := range a.Devices {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d *Device) {
			defer wg.Done()
			defer func() { <-sem }()
			r := fn(d)
			r.Dev = d
			results[i] = r
			logf("%s", strings.TrimSpace(fmt.Sprintf("%s: %s %s", d.IP, statusSingle[r.Status], r.Detail)))
			a.printResult(r)
		}(i, d)
	}
	wg.Wait()
	return results
}

// Summarize друкує підсумок і переліки проблемних пристроїв; повертає код завершення.
func (a *App) Summarize(results []Result) int {
	counts := map[Status]int{}
	lists := map[Status][]string{}
	for _, r := range results {
		counts[r.Status]++
		lists[r.Status] = append(lists[r.Status], r.Dev.IP)
	}
	parts := []string{fmt.Sprintf("всього %d", len(results))}
	for _, s := range statusOrder {
		if counts[s] > 0 || s == StOK {
			parts = append(parts, fmt.Sprintf("%s %d", statusPlural[s], counts[s]))
		}
	}
	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, "Підсумок: "+strings.Join(parts, " | "))
	code := 0
	for _, s := range statusOrder {
		if s == StOK || len(lists[s]) == 0 {
			continue
		}
		sortIPs(lists[s])
		title := capitalize(statusPlural[s])
		fmt.Fprintf(a.Out, "%s: %s\n", title, strings.Join(lists[s], ", "))
		if s != StSkipped {
			code = 1
		}
	}
	logf("підсумок: %s", strings.Join(parts, " | "))
	if path := a.Opt.Values["report"]; path != "" {
		if err := writeReport(path, results); err != nil {
			fmt.Fprintln(os.Stderr, "Помилка запису звіту:", err)
		} else {
			fmt.Fprintln(a.Out, "Звіт збережено:", path)
		}
	}
	return code
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// writeReport зберігає результати у CSV (UTF-8 з BOM, розділювач «;» — для Excel).
func writeReport(path string, results []Result) error {
	var b strings.Builder
	b.WriteString("\uFEFFip;модель;результат;деталі\r\n")
	for _, r := range results {
		detail := strings.ReplaceAll(r.Detail, ";", ",")
		fmt.Fprintf(&b, "%s;%s;%s;%s\r\n", r.Dev.IP, r.Dev.Name, statusSingle[r.Status], detail)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
