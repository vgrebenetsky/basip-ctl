// basip-ctl — утиліта для групового керування пристроями BAS-IP.
// Етап 1: ядро, команди ping, info, auth.
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const appVersion = "0.2.7 (етап 2)"

// defaultDevicesFile — файл пристроїв, що береться з поточного каталогу, якщо ім'я не вказано.
const defaultDevicesFile = "devices.csv"

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
	"wait": true, "save": true, "update": true, "no-file": true, "no-backup": true, "settings": true, "users": true,
}

// Команди, реалізовані на цьому етапі.
var commands = map[string]func(*App) int{
	"ping":           cmdPing,
	"info":           cmdInfo,
	"auth":           cmdAuth,
	"door-status":    cmdDoorStatus,
	"backup":         cmdBackup,
	"restore":        cmdRestore,
	"emergency-on":   cmdEmergencyOn,
	"emergency-off":  cmdEmergencyOff,
	"open":           cmdOpen,
	"reboot":         cmdReboot,
	"clear-logs":     cmdClearLogs,
	"passwd-manager": cmdPasswdManager,
	"passwd-admin":   cmdPasswdAdmin,
}

// Команди наступних етапів.
var plannedCommands = map[string]bool{}

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
	case "api-map":
		return cmdAPIMap(opt)
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
	logf("=== запуск v%s: команда %s, файл %q, --ip %q", appVersion, opt.Command, opt.File, opt.IPArg)

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
		if a == "-h" || a == "--help" || a == "/?" || a == "-?" || a == "/h" {
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
	HTTPLong *http.Client // для завантаження/відновлення архівів
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

	longT := 3 * opt.Timeout
	if longT < 120*time.Second {
		longT = 120 * time.Second
	}
	app.HTTPLong = &http.Client{
		Timeout: longT,
		Transport: &http.Transport{
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

	// Файл пристроїв за замовчуванням — devices.csv у поточному каталозі.
	if opt.Bools["no-file"] {
		if opt.File != "" {
			fmt.Fprintln(os.Stderr, "Помилка: --no-file не можна поєднувати з іменем файлу пристроїв")
			return nil, 2
		}
		if opt.IPArg == "" {
			fmt.Fprintln(os.Stderr, "Помилка: з --no-file обов'язковий параметр --ip (адреси)")
			return nil, 2
		}
	} else if opt.File == "" {
		if fileExists(defaultDevicesFile) {
			opt.File = defaultDevicesFile
		} else if opt.IPArg == "" {
			wd, _ := os.Getwd()
			fmt.Fprintf(os.Stderr, "Помилка: файл %s не знайдено в %s. Вкажіть ім'я файлу або --ip\n", defaultDevicesFile, wd)
			return nil, 2
		}
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
		if abs, err := filepath.Abs(opt.File); err == nil {
			fmt.Fprintln(os.Stderr, "Файл пристроїв:", abs)
		}
	} else {
		fmt.Fprintln(os.Stderr, "Файл пристроїв: не використовується")
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
  basip-ctl <команда> [файл] [параметри]

  Якщо файл не вказано, використовується devices.csv з поточного каталогу.

Команди:
  ping            перевірка доступності
  info            інформація про пристрій (без входу); --update — оновити файл
  auth            перевірка входу admin
  door-status     стан датчика дверей
  backup          резервна копія (налаштування + дані користувачів) в один архів
  emergency-on    аварійний режим: відкрити замки (лише emergency=1), --duration сек (типово 12 год)
  emergency-off   вимкнути аварійний режим (лише emergency=1)
  open            відкрити замки з колонки lock            [--ip обов'язковий]
  restore         відновлення з архіву backup --from файл   [--ip обов'язковий]
  reboot          перезавантаження, --wait — дочекатися     [--ip обов'язковий]
  clear-logs      очистити системні журнали, --save — зберегти перед очищенням [--ip обов'язковий]
  passwd-manager  змінити пароль manager, --force — навіть якщо вимкнено [--ip обов'язковий]
  passwd-admin    змінити пароль admin, новий хеш пишеться у файл     [--ip обов'язковий]
  api-map         показати, які запити API надсилаються пристроям кожного типу

Параметри:
  --ip <адреси>       192.168.10.11 | 192.168.10.11,192.168.10.21 | 192.168.10.11-16 | all
  --password <знач.>  пароль або MD5-хеш для пристроїв, яких немає у файлі
  --no-file           не використовувати файл пристроїв (потрібні --ip і --password)
  --https             працювати через HTTPS (самопідписані сертифікати приймаються)
  --settings/--users  (backup, restore) лише налаштування / лише дані користувачів
  --from <архів>      (restore) загальний архів, створений командою backup
  --no-backup         (restore) не робити копію перед відновленням
  --duration <сек>    (emergency-on) тривалість, 1..604800, типово 43200
  --unlock-time <сек> (open) час відкриття; без параметра — як налаштовано на пристрої
  --yes               не питати підтвердження
  --dry-run           показати, що буде зроблено, без змін
  --workers <N>       кількість пристроїв, що обробляються одночасно (типово 10)
  --timeout <сек>     тайм-аут HTTP-запиту (типово 5)
  --report <файл>     зберегти результати у CSV
  --endpoints <файл>  файл профілів API (типово endpoints.json поруч з програмою)
  --json              (info) вивести дані у форматі JSON

Паролі для passwd-* вводяться з клавіатури без відображення або беруться зі змінних
BASIP_NEW_PASSWORD та BASIP_OLD_PASSWORD.

Коди завершення: 0 — усе успішно, 1 — є помилки на частині пристроїв,
2 — помилка параметрів або файлу.
`)
}
