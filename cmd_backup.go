package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Загальний архів basip-ctl: <префікс>_<дата_час>.zip, усередині тека на кожен пристрій
// <ip>_<модель>/ з файлами settings.zip, users.zip та manifest.json.

type manifest struct {
	IP       string `json:"ip"`
	Model    string `json:"model"`
	Type     string `json:"device_type"`
	API      string `json:"api_version"`
	Firmware string `json:"firmware_version,omitempty"`
	Serial   string `json:"serial,omitempty"`
	Created  string `json:"created"`
}

type archEntry struct {
	name string
	data []byte
}

type Archive struct {
	prefix  string
	mu      sync.Mutex
	entries []archEntry
	folders map[string]bool
}

func newArchive(prefix string) *Archive {
	return &Archive{prefix: prefix, folders: map[string]bool{}}
}

func folderFor(d *Device) string {
	model := strings.NewReplacer("/", "-", "\\", "-", " ", "_").Replace(d.Name)
	if model == "" {
		model = "device"
	}
	return d.IP + "_" + model
}

// Add додає файл у теку пристрою.
func (ar *Archive) Add(d *Device, name string, data []byte) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	f := folderFor(d)
	ar.folders[f] = true
	ar.entries = append(ar.entries, archEntry{f + "/" + name, data})
}

// AddManifest записує опис пристрою в його теку.
func (ar *Archive) AddManifest(d *Device, info *DevInfo) {
	m := manifest{IP: d.IP, Model: d.Name, Type: d.Type, API: d.APIVersion, Created: time.Now().Format(time.RFC3339)}
	if info != nil {
		m.Firmware, m.Serial = info.FirmwareVersion, info.Serial
		if info.DeviceName != "" {
			m.Model = info.DeviceName
		}
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	ar.Add(d, "manifest.json", data)
}

func (ar *Archive) Empty() bool { return len(ar.entries) == 0 }

// Write зберігає архів у поточному каталозі й повертає ім'я файлу.
func (ar *Archive) Write(suffix string) (string, error) {
	name := fmt.Sprintf("%s_%s%s.zip", ar.prefix, time.Now().Format("20060102_150405"), suffix)
	sort.Slice(ar.entries, func(i, j int) bool { return ar.entries[i].name < ar.entries[j].name })
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ar.entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Now()}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return "", err
		}
		if _, err := w.Write(e.data); err != nil {
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := os.WriteFile(name, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return name, nil
}

func (a *App) writeArchive(ar *Archive, title string) {
	if ar.Empty() {
		return
	}
	name, err := ar.Write("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка запису архіву:", err)
		return
	}
	fmt.Fprintf(a.Out, "\n%s: %s (пристроїв: %d)\n", title, name, len(ar.folders))
	logf("%s: %s", title, name)
}

// backupParts — які частини зберігати/відновлювати: --settings, --users або обидві.
func (a *App) backupParts() (settings, users bool) {
	s, u := a.Opt.Bools["settings"], a.Opt.Bools["users"]
	if !s && !u {
		return true, true
	}
	return s, u
}

// backupResult — що вдалося зберегти з одного пристрою.
type backupResult struct {
	done    []string // збережені частини
	notes   []string // частини, які пристрій не підтримує
	failed  []string // частини, що завершилися помилкою
	noneSup bool     // жодна з частин не підтримується
	reboot  bool     // експорт завис на пристрої — допоможе перезавантаження
}

func (r backupResult) text() string {
	t := "збережено: " + strings.Join(r.done, " + ")
	if len(r.done) == 0 {
		t = "нічого не збережено"
	}
	for _, n := range r.notes {
		t += "; " + n + " — не підтримується"
	}
	for _, f := range r.failed {
		t += "; " + f
	}
	return t
}

// backupDevice знімає копію одного пристрою в архів. Частини (налаштування, дані користувачів)
// незалежні: якщо одна не підтримується або дала збій, інша все одно потрапляє в архів.
func backupDevice(c *Client, d *Device, ar *Archive, settings, users bool) backupResult {
	info, _ := c.Info()
	if info != nil && d.Name == "" {
		d.Name = info.DeviceName
	}
	var r backupResult
	type part struct {
		want  bool
		op    string
		file  string
		title string
	}
	parts := []part{
		{settings, "backup_settings", "settings.zip", "налаштування"},
		{users, "backup_users", "users.zip", "дані користувачів"},
	}
	asked := 0
	for i, p := range parts {
		if !p.want {
			continue
		}
		asked++
		if i > 0 && len(r.done) > 0 {
			// Коротка пауза між експортами: панелі гірше відпрацьовують два запити поспіль.
			time.Sleep(2 * time.Second)
		}
		data, err := c.Download(p.op)
		switch {
		case err == nil:
			ar.Add(d, p.file, data)
			r.done = append(r.done, p.title)
		case errors.Is(err, errNoOp):
			r.notes = append(r.notes, p.title)
		default:
			r.failed = append(r.failed, p.title+": "+err.Error())
			var oe *OpError
			if errors.Is(err, errExportLocked) || (errors.As(err, &oe) && oe.Code >= 500) {
				r.reboot = true
			}
		}
	}
	r.noneSup = asked > 0 && len(r.notes) == asked
	if len(r.done) > 0 {
		ar.AddManifest(d, info)
	}
	return r
}

func cmdBackup(a *App) int {
	settings, users := a.backupParts()
	a.printHeader("Резервне копіювання")
	ar := newArchive("backup")
	var stuckMu sync.Mutex
	var stuck []string
	res := a.RunAll(func(d *Device) Result {
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		part := newArchive("")
		r := backupDevice(c, d, part, settings, users)
		if r.reboot {
			stuckMu.Lock()
			stuck = append(stuck, d.IP)
			stuckMu.Unlock()
		}
		if len(r.done) > 0 {
			ar.mu.Lock()
			ar.entries = append(ar.entries, part.entries...)
			for f := range part.folders {
				ar.folders[f] = true
			}
			ar.mu.Unlock()
		}
		switch {
		case r.noneSup:
			return Result{Status: StUnsupported, Detail: "резервне копіювання недоступне на цьому пристрої"}
		case len(r.failed) > 0:
			// Частину збережено, але є збій — рахуємо як помилку, щоб не пропустити.
			return Result{Status: StError, Detail: r.text()}
		default:
			return Result{Status: StOK, Detail: r.text()}
		}
	})
	a.writeArchive(ar, "Резервну копію збережено")
	if len(stuck) > 0 {
		sortIPs(stuck)
		fmt.Fprintf(a.Out, "\nЕкспорт даних користувачів завис на %d пристр.: %s\n", len(stuck), strings.Join(stuck, ", "))
		fmt.Fprintf(a.Out, "Перезавантажте їх і повторіть копію:\n  basip-ctl reboot --ip %s --wait\n  basip-ctl backup --ip %s\n",
			strings.Join(stuck, ","), strings.Join(stuck, ","))
		logf("експорт завис: %s", strings.Join(stuck, ","))
	}
	return a.Summarize(res)
}

// ---------- restore ----------

type restoreSet struct {
	man      manifest
	settings []byte
	users    []byte
}

// loadRestoreArchive читає загальний архів і групує вміст за IP.
func loadRestoreArchive(file string) (map[string]*restoreSet, error) {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil, fmt.Errorf("не вдалося відкрити %s: %v", file, err)
	}
	defer zr.Close()
	sets := map[string]*restoreSet{}
	byFolder := map[string]*restoreSet{}
	for _, f := range zr.File {
		dir, base := path.Split(f.Name)
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" || strings.Contains(dir, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		s := byFolder[dir]
		if s == nil {
			s = &restoreSet{}
			byFolder[dir] = s
		}
		switch base {
		case "manifest.json":
			_ = json.Unmarshal(data, &s.man)
		case "settings.zip":
			s.settings = data
		case "users.zip":
			s.users = data
		}
	}
	for dir, s := range byFolder {
		ip := s.man.IP
		if ip == "" {
			ip = strings.SplitN(dir, "_", 2)[0]
		}
		if s.man.IP == "" || (s.settings == nil && s.users == nil) {
			continue
		}
		sets[ip] = s
	}
	if len(sets) == 0 {
		return nil, errors.New("це не загальний архів basip-ctl (немає тек пристроїв з manifest.json). " +
			"Відновлення з окремого файлу на кілька пристроїв заборонено")
	}
	return sets, nil
}

func cmdRestore(a *App) int {
	from := a.Opt.Values["from"]
	if from == "" {
		fmt.Fprintln(os.Stderr, "Помилка: вкажіть архів резервної копії: --from backup_<дата>.zip")
		return 2
	}
	sets, err := loadRestoreArchive(from)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Помилка:", err)
		return 2
	}
	wantS, wantU := a.Opt.Bools["settings"], a.Opt.Bools["users"]
	both := !wantS && !wantU
	noBackup := a.Opt.Bools["no-backup"]

	var targets []*Device
	for _, d := range a.eligible(nil) {
		if sets[d.IP] != nil {
			targets = append(targets, d)
		}
	}
	q := "Відновити з " + from
	if !noBackup {
		q += " (перед цим буде зроблено свіжу резервну копію)"
	}
	if !a.planned(q, targets) {
		return 0
	}
	a.printHeader("Відновлення з резервної копії")
	pre := newArchive("backup")
	res := a.RunAll(func(d *Device) Result {
		s := sets[d.IP]
		if d.SkipAuth() {
			return Result{Status: StSkipped, Detail: skipOKText}
		}
		if s == nil {
			return Result{Status: StSkipped, Detail: "немає копії в архіві"}
		}
		doS := s.settings != nil && (both || wantS)
		doU := s.users != nil && (both || wantU)
		if !doS && !doU {
			return Result{Status: StSkipped, Detail: "в архіві немає потрібних частин"}
		}
		c, fail := a.connect(d)
		if fail != nil {
			return *fail
		}
		info, err := c.Info()
		if err != nil {
			return Result{Status: StError, Detail: err.Error()}
		}
		if s.man.Model != "" && info.DeviceName != "" && !strings.EqualFold(s.man.Model, info.DeviceName) {
			return Result{Status: StMismatch, Detail: fmt.Sprintf("інша модель: у копії %s, на пристрої %s", s.man.Model, info.DeviceName)}
		}
		if !noBackup {
			part := newArchive("")
			if r := backupDevice(c, d, part, doS, doU); len(r.failed) > 0 || len(r.done) == 0 {
				return Result{Status: StError, Detail: "не вдалося зробити попередню копію, відновлення скасовано: " + r.text()}
			}
			pre.mu.Lock()
			pre.entries = append(pre.entries, part.entries...)
			for f := range part.folders {
				pre.folders[f] = true
			}
			pre.mu.Unlock()
		}
		var done []string
		// Після імпорту пристрій перезавантажується — чекаємо й входимо знову перед наступним кроком.
		if doU {
			if err := c.Upload("restore_users", "users.zip", s.users); err != nil {
				return opFail(fmt.Errorf("дані користувачів: %w", err))
			}
			done = append(done, "дані користувачів")
			if doS {
				if !a.waitBack(d, 5*time.Minute) {
					return Result{Status: StError, Detail: "дані користувачів відновлено, але пристрій не повернувся в мережу; налаштування не відновлено"}
				}
				if c, fail = a.connect(d); fail != nil {
					fail.Detail = "після відновлення даних користувачів: " + fail.Detail
					return *fail
				}
			}
		}
		if doS {
			if err := c.Upload("restore_settings", "settings.zip", s.settings); err != nil {
				return opFail(fmt.Errorf("налаштування: %w", err))
			}
			done = append(done, "налаштування")
		}
		return Result{Status: StOK, Detail: "відновлено: " + strings.Join(done, " + ") + " (пристрій перезавантажується)"}
	})
	if !pre.Empty() {
		if name, err := pre.Write("_before-restore"); err == nil {
			fmt.Fprintf(a.Out, "\nКопію перед відновленням збережено: %s\n", name)
		} else {
			fmt.Fprintln(os.Stderr, "Помилка запису архіву:", err)
		}
	}
	return a.Summarize(res)
}
