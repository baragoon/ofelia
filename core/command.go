package core

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gobs/args"
)

func parseCommandArgs(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	if shell, flag, rest, ok := splitShellCommand(command); ok {
		return []string{shell, flag, trimMatchingQuotes(rest)}
	}

	return args.GetArgs(command)
}

func splitShellCommand(command string) (shell, flag, rest string, ok bool) {
	shell, remainder, ok := nextCommandToken(command)
	if !ok || !isShellBinary(shell) {
		return "", "", "", false
	}

	flag, remainder, ok = nextCommandToken(remainder)
	if !ok || !isShellCommandFlag(flag) {
		return "", "", "", false
	}

	rest = strings.TrimLeftFunc(remainder, unicode.IsSpace)
	if rest == "" {
		return "", "", "", false
	}

	return shell, flag, rest, true
}

func nextCommandToken(input string) (token, remainder string, ok bool) {
	trimmed := strings.TrimLeftFunc(input, unicode.IsSpace)
	if trimmed == "" {
		return "", "", false
	}

	for index, r := range trimmed {
		if unicode.IsSpace(r) {
			return trimmed[:index], trimmed[index:], true
		}
	}

	return trimmed, "", true
}

func isShellBinary(command string) bool {
	switch filepath.Base(command) {
	case "sh", "ash", "bash", "dash", "zsh":
		return true
	default:
		return false
	}
}

func isShellCommandFlag(flag string) bool {
	if len(flag) < 2 || flag[0] != '-' {
		return false
	}

	hasCommandFlag := false
	for _, r := range flag[1:] {
		if !unicode.IsLetter(r) {
			return false
		}
		if r == 'c' || r == 'C' {
			hasCommandFlag = true
		}
	}

	return hasCommandFlag
}

func trimMatchingQuotes(input string) string {
	if len(input) < 2 {
		return input
	}

	if (input[0] == '"' && input[len(input)-1] == '"') || (input[0] == '\'' && input[len(input)-1] == '\'') {
		return input[1 : len(input)-1]
	}

	return input
}
