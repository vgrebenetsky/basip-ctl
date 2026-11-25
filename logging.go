package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Журнал роботи. Паролі, хеші та токени у журнал не записуються.
var (
	logMu   sync.Mutex
	logFile *os.File
)

func initLog(path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Попередження: не вдалося відкрити журнал", path, "—", err)
		return
	}
	logFile = f
}

func closeLog() {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

func logf(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile == nil {
		return
	}
	fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}
