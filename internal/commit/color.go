package commit

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiReset = "\x1b[m"
)

func colorOutput(out io.Writer) bool {
	f, ok := out.(*os.File)
	return ok && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd()))
}

func paint(color, s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return color + s + ansiReset
}

// colorStat colors the +/- graph of `git diff --stat` file lines.
func colorStat(stat string, color bool) string {
	if !color {
		return stat
	}
	lines := strings.Split(stat, "\n")
	for i, line := range lines {
		bar := strings.LastIndex(line, " | ")
		if bar < 0 {
			continue
		}
		graph := strings.LastIndexByte(line, ' ') + 1
		rest := line[graph:]
		if graph <= bar+2 || strings.Trim(rest, "+-") != "" {
			continue
		}
		plus := strings.TrimRight(rest, "-")
		lines[i] = line[:graph] + paint(ansiGreen, plus) + paint(ansiRed, rest[len(plus):])
	}
	return strings.Join(lines, "\n")
}
