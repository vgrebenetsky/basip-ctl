package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var stdin = bufio.NewReader(os.Stdin)

func readLine() (string, error) {
	s, err := stdin.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// confirm запитує підтвердження. --yes пропускає запит.
func (a *App) confirm(question string) bool {
	if a.Opt.Bools["yes"] {
		return true
	}
	fmt.Fprintf(os.Stderr, "%s\nПродовжити? Введіть «так» або «yes»: ", question)
	ans, err := readLine()
	if err != nil {
		fmt.Fprintln(os.Stderr)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "так", "т", "yes", "y":
		return true
	}
	fmt.Fprintln(os.Stderr, "Скасовано.")
	return false
}

// readSecret читає пароль без відображення на екрані (через stty).
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	hidden := off.Run() == nil
	s, err := readLine()
	if hidden {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
	}
	fmt.Fprintln(os.Stderr)
	return s, err
}

// askNewPassword: пароль зі змінної оточення або двічі з клавіатури.
func askNewPassword(envName, title string, check func(string) error) (string, error) {
	if v := os.Getenv(envName); v != "" {
		if err := check(v); err != nil {
			return "", fmt.Errorf("пароль зі змінної %s: %v", envName, err)
		}
		return v, nil
	}
	for i := 0; i < 3; i++ {
		p1, err := readSecret(title + ": ")
		if err != nil {
			return "", errors.New("не вдалося прочитати пароль")
		}
		if err := check(p1); err != nil {
			fmt.Fprintln(os.Stderr, "Пароль не підходить:", err)
			continue
		}
		p2, err := readSecret("Повторіть пароль: ")
		if err != nil {
			return "", errors.New("не вдалося прочитати пароль")
		}
		if p1 != p2 {
			fmt.Fprintln(os.Stderr, "Паролі не збігаються, спробуйте ще раз.")
			continue
		}
		return p1, nil
	}
	return "", errors.New("пароль не задано")
}

const androidSpecials = "!;*()_-+.,$&\"№%:|@^"

// checkStrong — вимоги Android-панелей: 8–32 символи, мала й велика літера, цифра, спецсимвол з набору.
func checkStrong(p string) error {
	r := []rune(p)
	if len(r) < 8 || len(r) > 32 {
		return errors.New("довжина має бути від 8 до 32 символів")
	}
	var lo, up, dg, sp bool
	for _, c := range r {
		switch {
		case c >= 'a' && c <= 'z':
			lo = true
		case c >= 'A' && c <= 'Z':
			up = true
		case c >= '0' && c <= '9':
			dg = true
		case strings.ContainsRune(androidSpecials, c):
			sp = true
		default:
			return fmt.Errorf("недопустимий символ %q (дозволені латинські літери, цифри та %s)", c, androidSpecials)
		}
	}
	if !lo || !up || !dg || !sp {
		return errors.New("потрібні мала й велика латинська літера, цифра та спецсимвол " + androidSpecials)
	}
	return nil
}

// checkBasic — вимоги Camdroid-пристроїв: 1–32 друкованих ASCII-символи.
func checkBasic(p string) error {
	if len(p) == 0 || len(p) > 32 {
		return errors.New("довжина має бути від 1 до 32 символів")
	}
	for _, c := range p {
		if c < 0x20 || c > 0x7E {
			return errors.New("дозволені лише латинські літери, цифри та розділові знаки ASCII")
		}
	}
	return nil
}
