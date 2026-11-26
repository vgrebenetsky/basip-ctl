package main

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Device — один пристрій з файлу або з параметра --ip.
type Device struct {
	IP         string
	Type       string // device_type: panel, accesscontroller ...
	Name       string // device_name: модель
	APIVersion string
	LockRaw    string
	Locks      []int // номери замків; 0 — усі замки
	Secret     string // пароль або MD5-хеш як задано
	Emergency  bool
	InFile     bool
	line       int // індекс рядка у файлі
}

// isMD5 — значення є MD5-хешем (рівно 32 шістнадцяткові символи).
func isMD5(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// MD5Of повертає MD5-хеш рядка у нижньому регістрі.
func MD5Of(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// PasswordHash повертає хеш для входу: хеш з файлу як є або MD5 від пароля.
func (d *Device) PasswordHash() string {
	s := strings.TrimSpace(d.Secret)
	if isMD5(s) {
		return s
	}
	return MD5Of(s)
}

// SkipAuth — у полі пароля вказано "OK": пристрій виключено з усіх дій, що потребують входу
// (працюють лише ping та info).
func (d *Device) SkipAuth() bool {
	return strings.EqualFold(strings.TrimSpace(d.Secret), "OK")
}

// HasSecret — для пристрою відомий пароль або хеш.
func (d *Device) HasSecret() bool {
	return strings.TrimSpace(d.Secret) != ""
}

// Model — назва моделі для виводу.
func (d *Device) Model() string {
	if d.Name == "" {
		return "—"
	}
	return d.Name
}

// parseLocks розбирає поле lock: "1", "1,2", "0" (усі). Порожнє значення = 1.
func parseLocks(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []int{1}, nil
	}
	var res []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 8 {
			return nil, fmt.Errorf("некоректний номер замка %q (очікується 0..8)", p)
		}
		res = append(res, n)
	}
	if len(res) == 0 {
		return []int{1}, nil
	}
	return res, nil
}

// DeviceFile — файл devices.csv з збереженням вихідних рядків для акуратного перезапису.
type DeviceFile struct {
	Path     string
	Lines    []string
	Header   []string
	Col      map[string]int // назва колонки -> індекс
	PassCol  string         // "status" або "password"
	Devices  []*Device
	ByIP     map[string]*Device
	Warnings []string
	CRLF     bool
}

// LoadDeviceFile читає файл пристроїв (розділювач «;», перший рядок — заголовок).
func LoadDeviceFile(path string) (*DeviceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	text := string(data)
	df := &DeviceFile{Path: path, Col: map[string]int{}, ByIP: map[string]*Device{}}
	df.CRLF = strings.Contains(text, "\r\n")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	df.Lines = strings.Split(text, "\n")
	// Прибрати порожній хвіст після останнього переводу рядка.
	for len(df.Lines) > 0 && strings.TrimSpace(df.Lines[len(df.Lines)-1]) == "" {
		df.Lines = df.Lines[:len(df.Lines)-1]
	}

	headerIdx := -1
	for i, ln := range df.Lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		headerIdx = i
		break
	}
	if headerIdx < 0 {
		return nil, fmt.Errorf("файл порожній")
	}
	for i, h := range strings.Split(df.Lines[headerIdx], ";") {
		h = strings.ToLower(strings.TrimSpace(h))
		df.Header = append(df.Header, h)
		if h != "" {
			if _, dup := df.Col[h]; !dup {
				df.Col[h] = i
			}
		}
	}
	if _, ok := df.Col["ip"]; !ok {
		return nil, fmt.Errorf("у заголовку немає колонки ip")
	}
	// Колонка пароля історично може називатися status або password.
	if _, ok := df.Col["password"]; ok {
		df.PassCol = "password"
	} else if _, ok := df.Col["status"]; ok {
		df.PassCol = "status"
	} else {
		df.Warnings = append(df.Warnings, "у заголовку немає колонки status/password — пароль треба передавати через --password")
	}

	for i := headerIdx + 1; i < len(df.Lines); i++ {
		t := strings.TrimSpace(df.Lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Split(df.Lines[i], ";")
		get := func(name string) string {
			idx, ok := df.Col[name]
			if !ok || idx >= len(f) {
				return ""
			}
			return strings.TrimSpace(f[idx])
		}
		ip := get("ip")
		if net.ParseIP(ip).To4() == nil {
			df.Warnings = append(df.Warnings, fmt.Sprintf("рядок %d: некоректна IP-адреса %q — пропущено", i+1, ip))
			continue
		}
		if _, dup := df.ByIP[ip]; dup {
			df.Warnings = append(df.Warnings, fmt.Sprintf("рядок %d: адреса %s повторюється — використано перший запис", i+1, ip))
			continue
		}
		d := &Device{
			IP:         ip,
			Type:       get("device_type"),
			Name:       get("device_name"),
			APIVersion: get("api_version"),
			LockRaw:    get("lock"),
			Emergency:  get("emergency") == "1",
			InFile:     true,
			line:       i,
		}
		if df.PassCol != "" {
			d.Secret = get(df.PassCol)
		}
		locks, err := parseLocks(d.LockRaw)
		if err != nil {
			df.Warnings = append(df.Warnings, fmt.Sprintf("рядок %d (%s): %v — використано замок 1", i+1, ip, err))
			locks = []int{1}
		}
		d.Locks = locks
		df.Devices = append(df.Devices, d)
		df.ByIP[ip] = d
	}
	return df, nil
}

// buildLine формує рядок CSV для пристрою у порядку колонок заголовка.
func (df *DeviceFile) buildLine(d *Device) string {
	f := make([]string, len(df.Header))
	for i, h := range df.Header {
		switch h {
		case "ip":
			f[i] = d.IP
		case "device_type":
			f[i] = d.Type
		case "device_name":
			f[i] = d.Name
		case "api_version":
			f[i] = d.APIVersion
		case "lock":
			f[i] = d.LockRaw
		case "status", "password":
			if h == df.PassCol {
				f[i] = d.Secret
			}
		case "emergency":
			if d.Emergency {
				f[i] = "1"
			}
		}
	}
	return strings.Join(f, ";") + ";"
}

// SetSecret змінює пароль/хеш у рядку пристрою, зберігаючи решту полів.
func (df *DeviceFile) SetSecret(d *Device, secret string) {
	d.Secret = secret
	idx, ok := df.Col[df.PassCol]
	if !ok || !d.InFile {
		return
	}
	f := strings.Split(df.Lines[d.line], ";")
	for len(f) <= idx {
		f = append(f, "")
	}
	f[idx] = secret
	df.Lines[d.line] = strings.Join(f, ";")
}

// SetInfo оновлює у рядку пристрою колонки device_type, device_name, api_version,
// зберігаючи решту полів. Повертає true, якщо щось змінилося.
func (df *DeviceFile) SetInfo(d *Device, typ, name, api string) bool {
	if !d.InFile {
		return false
	}
	f := strings.Split(df.Lines[d.line], ";")
	changed := false
	set := func(col, val string, dst *string) {
		idx, ok := df.Col[col]
		if !ok || val == "" {
			return
		}
		for len(f) <= idx {
			f = append(f, "")
		}
		if strings.TrimSpace(f[idx]) != val {
			f[idx] = val
			*dst = val
			changed = true
		}
	}
	set("device_type", typ, &d.Type)
	set("device_name", name, &d.Name)
	set("api_version", api, &d.APIVersion)
	if changed {
		df.Lines[d.line] = strings.Join(f, ";")
	}
	return changed
}

// AppendDevice додає новий пристрій у кінець файлу (у пам'яті).
func (df *DeviceFile) AppendDevice(d *Device) {
	d.InFile = true
	d.line = len(df.Lines)
	df.Lines = append(df.Lines, df.buildLine(d))
	df.Devices = append(df.Devices, d)
	df.ByIP[d.IP] = d
}

// Save записує файл, попередньо створивши резервну копію з ротацією
// (.bak -> .bak1 -> .bak2 ...; свіжа копія завжди .bak). Повертає ім'я копії.
func (df *DeviceFile) Save() (string, error) {
	bak, err := rotateBackup(df.Path)
	if err != nil {
		return "", fmt.Errorf("не вдалося створити резервну копію: %w", err)
	}
	nl := "\n"
	if df.CRLF {
		nl = "\r\n"
	}
	content := strings.Join(df.Lines, nl) + nl
	tmp := df.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return bak, err
	}
	if err := os.Rename(tmp, df.Path); err != nil {
		return bak, err
	}
	logf("файл %s оновлено, резервна копія %s", df.Path, bak)
	return bak, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// rotateBackup зсуває наявні копії і створює свіжу копію <файл>.bak.
func rotateBackup(path string) (string, error) {
	base := path + ".bak"
	if fileExists(base) {
		// Перший вільний номер.
		n := 1
		for fileExists(base + strconv.Itoa(n)) {
			n++
		}
		for i := n; i > 1; i-- {
			if err := os.Rename(base+strconv.Itoa(i-1), base+strconv.Itoa(i)); err != nil {
				return "", err
			}
		}
		if err := os.Rename(base, base+"1"); err != nil {
			return "", err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base, data, 0o600); err != nil {
		return "", err
	}
	return filepath.Base(base), nil
}

// IPSelector — результат розбору параметра --ip.
type IPSelector struct {
	All bool
	IPs []string
}

func ipToU32(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }

func u32ToIP(n uint32) string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return net.IP(b).String()
}

// ParseIPSelector розбирає: адресу, список через кому, діапазон a.b.c.X-Y або a.b.c.X-a.b.c.Y, all.
func ParseIPSelector(s string) (*IPSelector, error) {
	sel := &IPSelector{}
	if strings.EqualFold(strings.TrimSpace(s), "all") {
		sel.All = true
		return sel, nil
	}
	seen := map[string]bool{}
	add := func(ip string) {
		if !seen[ip] {
			seen[ip] = true
			sel.IPs = append(sel.IPs, ip)
		}
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.EqualFold(part, "all") {
			return nil, fmt.Errorf("all не можна поєднувати з іншими адресами")
		}
		if k := strings.IndexByte(part, '-'); k >= 0 {
			left, right := strings.TrimSpace(part[:k]), strings.TrimSpace(part[k+1:])
			start := net.ParseIP(left).To4()
			if start == nil {
				return nil, fmt.Errorf("некоректний діапазон %q", part)
			}
			var end net.IP
			if strings.Contains(right, ".") {
				end = net.ParseIP(right).To4()
			} else {
				n, err := strconv.Atoi(right)
				if err == nil && n >= 0 && n <= 255 {
					end = net.IPv4(start[0], start[1], start[2], byte(n)).To4()
				}
			}
			if end == nil {
				return nil, fmt.Errorf("некоректний діапазон %q", part)
			}
			a, b := ipToU32(start), ipToU32(end)
			if b < a {
				return nil, fmt.Errorf("діапазон %q: кінцева адреса менша за початкову", part)
			}
			if b-a > 4095 {
				return nil, fmt.Errorf("діапазон %q завеликий (більше 4096 адрес)", part)
			}
			for n := a; n <= b; n++ {
				add(u32ToIP(n))
			}
			continue
		}
		ip := net.ParseIP(part).To4()
		if ip == nil {
			return nil, fmt.Errorf("некоректна IP-адреса %q", part)
		}
		add(ip.String())
	}
	if len(sel.IPs) == 0 {
		return nil, fmt.Errorf("параметр --ip не містить жодної адреси")
	}
	return sel, nil
}

// selectDevices формує перелік пристроїв для обробки.
func selectDevices(df *DeviceFile, ipArg, password string) ([]*Device, []string, error) {
	var warns []string
	if ipArg == "" {
		if df == nil {
			return nil, nil, fmt.Errorf("вкажіть файл пристроїв або --ip")
		}
		return withFallbackSecret(df.Devices, password), nil, nil
	}
	sel, err := ParseIPSelector(ipArg)
	if err != nil {
		return nil, nil, fmt.Errorf("--ip: %v", err)
	}
	if sel.All {
		if df == nil {
			return nil, nil, fmt.Errorf("--ip all потребує файлу пристроїв")
		}
		return withFallbackSecret(df.Devices, password), nil, nil
	}
	var res []*Device
	for _, ip := range sel.IPs {
		if df != nil {
			if d, ok := df.ByIP[ip]; ok {
				res = append(res, d)
				continue
			}
			if password == "" {
				warns = append(warns, fmt.Sprintf("адреси %s немає у %s — передайте пароль або хеш через --password", ip, filepath.Base(df.Path)))
			} else {
				warns = append(warns, fmt.Sprintf("адреси %s немає у %s — використано --password; після успішного входу пристрій буде додано до файлу", ip, filepath.Base(df.Path)))
			}
		}
		res = append(res, &Device{IP: ip, Secret: password, Locks: []int{1}, LockRaw: "1"})
	}
	return res, warns, nil
}

// withFallbackSecret підставляє --password тим пристроям, у яких у файлі пароль не вказано.
func withFallbackSecret(devs []*Device, password string) []*Device {
	if password != "" {
		for _, d := range devs {
			if !d.HasSecret() {
				d.Secret = password
			}
		}
	}
	return devs
}

// sortIPs сортує адреси за числовим значенням.
func sortIPs(ips []string) {
	sort.Slice(ips, func(i, j int) bool {
		a, b := net.ParseIP(ips[i]).To4(), net.ParseIP(ips[j]).To4()
		if a == nil || b == nil {
			return ips[i] < ips[j]
		}
		return ipToU32(a) < ipToU32(b)
	})
}
