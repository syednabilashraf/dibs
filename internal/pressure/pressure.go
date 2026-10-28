package pressure

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Level int

const (
	Unknown Level = iota
	Normal
	Warning
	Critical
)

func (l Level) String() string {
	switch l {
	case Normal:
		return "normal"
	case Warning:
		return "warning"
	case Critical:
		return "critical"
	}
	return "unknown"
}

var Current = read

func read(ctx context.Context) Level {
	switch runtime.GOOS {
	case "darwin":
		return darwin(ctx)
	case "linux":
		if level := psi("/proc/pressure/memory"); level != Unknown {
			return level
		}
		return meminfo("/proc/meminfo")
	}
	return Unknown
}

func darwin(ctx context.Context) Level {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output()
	if err != nil {
		return Unknown
	}
	switch strings.TrimSpace(string(out)) {
	case "1":
		return Normal
	case "2":
		return Warning
	case "4":
		return Critical
	}
	return Unknown
}

func psi(path string) Level {
	file, err := os.Open(path)
	if err != nil {
		return Unknown
	}
	defer file.Close()
	values := map[string]float64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		for _, field := range fields[1:] {
			if avg, ok := strings.CutPrefix(field, "avg10="); ok {
				values[fields[0]], _ = strconv.ParseFloat(avg, 64)
			}
		}
	}
	switch {
	case len(values) == 0:
		return Unknown
	case values["full"] >= 10:
		return Critical
	case values["some"] >= 20:
		return Warning
	}
	return Normal
}

func meminfo(path string) Level {
	file, err := os.Open(path)
	if err != nil {
		return Unknown
	}
	defer file.Close()
	values := map[string]float64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			values[strings.TrimSuffix(fields[0], ":")], _ = strconv.ParseFloat(fields[1], 64)
		}
	}
	total, available := values["MemTotal"], values["MemAvailable"]
	switch {
	case total == 0:
		return Unknown
	case available/total < 0.05:
		return Critical
	case available/total < 0.10:
		return Warning
	}
	return Normal
}
