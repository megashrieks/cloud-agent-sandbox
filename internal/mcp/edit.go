package mcp

import (
	"errors"
	"fmt"
	"strings"
)

func renderNumberedLines(content string, startLine, endLine int) string {
	lines := splitDisplayLines(content)
	if startLine < 1 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > endLine || startLine > len(lines) {
		return ""
	}

	var b strings.Builder
	for i := startLine; i <= endLine; i++ {
		if i > startLine {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d: %s", i, lines[i-1])
	}
	return b.String()
}

func splitDisplayLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func replaceUnique(content, oldStr, newStr string) (string, error) {
	if oldStr == "" {
		return "", errors.New("old_str must not be empty")
	}
	matches := strings.Count(content, oldStr)
	switch matches {
	case 0:
		return "", errors.New("old_str not found")
	case 1:
		return strings.Replace(content, oldStr, newStr, 1), nil
	default:
		return "", fmt.Errorf("old_str is not unique (%d matches)", matches)
	}
}

func insertAfterLine(content string, line int, text string) (string, error) {
	if line < 0 {
		return "", errors.New("line must be >= 0")
	}
	if line == 0 {
		return text + content, nil
	}

	seen := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			seen++
			if seen == line {
				return content[:i+1] + text + content[i+1:], nil
			}
		}
	}
	if content != "" && line == seen+1 {
		return content + text, nil
	}
	return "", fmt.Errorf("line %d out of range", line)
}
