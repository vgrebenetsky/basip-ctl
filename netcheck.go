package main

import (
	"context"
	"errors"
	"math"
	"net"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Pinger перевіряє доступність: ICMP через системну утиліту ping,
// а якщо її немає або бракує прав — перевірка TCP-портів 80/443.
type Pinger struct {
	Mode    string // "icmp" або "tcp"
	Timeout time.Duration
	args    []string
}

func NewPinger(timeout time.Duration) *Pinger {
	p := &Pinger{Mode: "tcp", Timeout: timeout}
	if _, err := exec.LookPath("ping"); err != nil {
		logf("утиліту ping не знайдено — перевірка через TCP 80/443")
		return p
	}
	secs := strconv.Itoa(int(math.Max(1, math.Ceil(timeout.Seconds()))))
	// Спочатку варіант з коротким інтервалом, потім стандартний.
	candidates := [][]string{
		{"-n", "-c", "2", "-i", "0.2", "-W", secs},
		{"-n", "-c", "2", "-W", secs},
	}
	for _, c := range candidates {
		args := append(append([]string{}, c...), "127.0.0.1")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := exec.CommandContext(ctx, "ping", args...).Run()
		cancel()
		if err == nil {
			p.Mode = "icmp"
			p.args = c
			return p
		}
	}
	logf("ICMP недоступний (немає прав) — перевірка через TCP 80/443")
	return p
}

// ModeText — опис способу перевірки для виводу.
func (p *Pinger) ModeText() string {
	if p.Mode == "icmp" {
		return "ICMP (ping)"
	}
	return "TCP 80/443 (немає прав на ICMP)"
}

// Alive повертає true, якщо пристрій відповідає.
func (p *Pinger) Alive(ip string) bool {
	if p.Mode == "icmp" {
		args := append(append([]string{}, p.args...), ip)
		ctx, cancel := context.WithTimeout(context.Background(), p.Timeout*2+5*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "ping", args...).Run() == nil
	}
	return tcpAlive(ip, p.Timeout*2)
}

// tcpAlive: успішне з'єднання або відмова у з'єднанні (RST) означають, що вузол у мережі.
func tcpAlive(ip string, timeout time.Duration) bool {
	for _, port := range []string{"80", "443"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, port), timeout)
		if err == nil {
			conn.Close()
			return true
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return true
		}
	}
	return false
}
