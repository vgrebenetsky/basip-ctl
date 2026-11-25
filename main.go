// basip-ctl — утиліта для групового керування пристроями BAS-IP.
// Етап 1: ядро, команди ping, info, auth.
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const appVersion = "0.1.0 (етап 1)"

// Options — розібрані параметри командного рядка.
type Options struct {
	Command  string
	File     string
	IPArg    string
	Password string
	Workers  int
	Timeout  time.Duration
	PingWait time.Duration
	Values   map[string]string
	Bools    map[string]bool
}

// Параметри, що потребують значення.
var valueFlags = map[string]bool{
	"ip": true, "password": true, "workers": true, "timeout": true,
	"report": true, "endpoints": true, "duration": true, "unlock-time": true, "from": true,
}

// Параметри-перемикачі.
var boolFlags = map[string]bool{
	"https": true, "yes": true, "dry-run": true, "json": true, "force": true,
	"wait": true, "save": true, "no-backup": true, "settings": true, "users": true,
}

// Команди, реалізовані на цьому етапі.
var commands = map[string]func(*App) int{
	"ping": cmdPing,
	"info": cmdInfo,
	"auth": cmdAuth,
}

// Команди наступних етапів.
var plannedCommands = map[string]bool{
	"door-status": true, "backup": true, "restore": true, "emergency-on": true,
	"emergency-off": true, "open": true, "reboot": true, "clear-logs": true,
	"passwd-manager": true, "passwd-admin": true,
}

// Команди, для яких --ip обов'язковий (адреси або all).
var requireIP = map[string]bool{
	"open": true, "passwd-manager": true, "passwd-admin": true,
	"restore": true, "reboot": true, "clear-logs": true,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opt, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка:", err)
		fmt.Fprintln(os.Stderr, "Довідка: basip-ctl --help")
		return 2
	}
	switch opt.Command {
	case "", "help":
		printHelp()
		return 0
	case "version":
		fmt.Println("basip-ctl", appVersion)
		return 0
	}
	handler, ok := commands[opt.Command]
	if !ok {
		if plannedCommands[opt.Command] {
			fmt.Fprintf(os.Stderr, "Команда %q ще не реалізована (буде в наступному етапі).\n", opt.Command)
			return 2
		}
		fmt.Fprintf(os.Stderr, "Помилка: невідома команда %q\nДовідка: basip-ctl --help\n", opt.Command)
		return 2
	}

	initLog("basip-ctl.log")
	defer closeLog()
	logf("=== запуск: команда %s, файл %q, --ip %q", opt.Command, opt.File, opt.IPArg)

	app, code := prepare(opt)
	if code != 0 {
		return code
	}
	return handler(app)
}

// parseArgs розбирає аргументи: команда, необов'язковий файл пристроїв і параметри --name значення.
func parseArgs(args []string) (*Options, error) {
	o := &Options{
		Workers:  10,
		Timeout:  5 * time.Second,
		PingWait: time.Second,
		Values:   map[string]string{},
		Bools:    map[string]bool{},
	}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			o.Command = "help"
			return o, nil
		}
		if a == "--version" {
			o.Command = "version"
			return o, nil
		}
		if strings.HasPrefix(a, "--") {
			name := a[2:]
			val := ""
			hasVal := false
			if k := strings.IndexByte(name, '='); k >= 0 {
				val = name[k+1:]
				name = name[:k]
				hasVal = true
			}
			switch {
			case valueFlags[name]:
				if !hasVal {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("параметр --%s потребує значення", name)
					}
					i++
					val = args[i]
				}
				o.Values[name] = val
			case boolFlags[name]:
				if hasVal {
					return nil, fmt.Errorf("параметр --%s не приймає значення", name)
				}
				o.Bools[name] = true
			default:
				return nil, fmt.Errorf("невідомий параметр --%s", name)
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) > 0 {
		o.Command = strings.ToLower(pos[0])
	}
	if len(pos) > 1 {
		o.File = pos[1]
	}
	if len(pos) > 2 {
		return nil, fmt.Errorf("зайві аргументи: %s", strings.Join(pos[2:], " "))
	}
	o.IPArg = strings.TrimSpace(o.Values["ip"])
	o.Password = o.Values["password"]
	if v, ok := o.Values["workers"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return nil, fmt.Errorf("--workers: очікується число від 1 до 200")
		}
		o.Workers = n
	}
	if v, ok := o.Values["timeout"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 600 {
			return nil, fmt.Errorf("--timeout: очікується кількість секунд від 1 до 600")
		}
		o.Timeout = time.Duration(n) * time.Second
	}
	return o, nil
}

// App — спільний стан виконання команди.
type App struct {
	Opt     *Options
	File    *DeviceFile // nil, якщо файл пристроїв не задано
	Devices []*Device
	Cfg     *EndpointConfig
	Pinger  *Pinger
	HTTP    *http.Client
	Scheme  string
	Out     io.Writer // потік для построкового виводу та підсумку
}

// prepare завантажує файл, профілі API і формує перелік пристроїв.
func prepare(opt *Options) (*App, int) {
	app := &App{Opt: opt, Scheme: "http", Out: os.Stdout}
	if opt.Bools["https"] {
		app.Scheme = "https"
	}
	app.HTTP = &http.Client{
		Timeout: opt.Timeout,
		Transport: &http.Transport{
			// Самопідписані сертифікати панелей приймаються.
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConnsPerHost: 2,
		},
	}

	cfg, err := LoadEndpointConfig(opt.Values["endpoints"])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка у файлі профілів API:", err)
		return nil, 2
	}
	app.Cfg = cfg

	if opt.File == "" && opt.IPArg == "" {
		fmt.Fprintln(os.Stderr, "Помилка: вкажіть файл пристроїв (наприклад devices.csv) або параметр --ip")
		return nil, 2
	}
	if requireIP[opt.Command] && opt.IPArg == "" {
		fmt.Fprintf(os.Stderr, "Помилка: для команди %s обов'язковий параметр --ip (адреси або all)\n", opt.Command)
		return nil, 2
	}
	if opt.File != "" {
		df, err := LoadDeviceFile(opt.File)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Помилка читання файлу пристроїв:", err)
			return nil, 2
		}
		for _, w := range df.Warnings {
			fmt.Fprintln(os.Stderr, "Попередження:", w)
		}
		app.File = df
	}

	devs, warns, err := selectDevices(app.File, opt.IPArg, opt.Password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка:", err)
		return nil, 2
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "Попередження:", w)
	}
	if len(devs) == 0 {
		fmt.Fprintln(os.Stderr, "Немає пристроїв для обробки")
		return nil, 2
	}
	app.Devices = devs
	app.Pinger = NewPinger(opt.PingWait)
	return app, 0
}

func printHelp() {
	fmt.Print(`basip-ctl ` + appVersion + ` — групове керування пристроями BAS-IP

Використання:
  basip-ctl <команда> [devices.csv] [параметри]

Команди (етап 1):
  ping            перевірка доступності пристроїв
  auth            перевірка входу (admin) на пристроях
  info            інформація про пристрій (/api/info, без входу)

Команди наступних етапів:
  door-status, backup, restore, emergency-on, emergency-off, open,
  reboot, clear-logs, passwd-manager, passwd-admin

Параметри:
  --ip <адреси>       192.168.10.11 | 192.168.10.11,192.168.10.21 |
                      192.168.10.11-16 | all
  --password <знач.>  пароль або MD5-хеш для пристроїв, яких немає у файлі
  --https             працювати через HTTPS (самопідписані сертифікати приймаються)
  --workers <N>       кількість пристроїв, що обробляються одночасно (типово 10)
  --timeout <сек>     тайм-аут HTTP-запиту (типово 5)
  --report <файл>     зберегти результати у CSV-файл
  --endpoints <файл>  файл профілів API (типово endpoints.json поруч з програмою)
  --json              (info) вивести дані у форматі JSON
  --help              ця довідка

Коди завершення: 0 — усе успішно, 1 — є помилки на частині пристроїв,
2 — помилка параметрів або файлу.
`)
}
