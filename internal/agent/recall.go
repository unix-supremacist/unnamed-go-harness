package agent

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// recallText is the full text behind an archived result: the blob when the
// original output was trimmed, else the result itself.
func recallText(e archived) string {
	if e.Blob != "" {
		if b, err := os.ReadFile(e.Blob); err == nil {
			return string(b)
		}
	}
	return e.Text
}

func parseAlias(id string) (int, bool) {
	id = strings.TrimSpace(strings.ToLower(id))
	id = strings.TrimPrefix(id, "c")
	n, err := strconv.Atoi(id)
	return n, err == nil && n > 0
}

// recall serves the recall tool: up to recallCap bytes of one archived
// result, paged by line or filtered by pattern. The reply is an ordinary new
// tool result; the placeholder that sent the model here stays as it was.
func (a *Agent) recall(id string, startLine int, grep string) (string, error) {
	ord, ok := parseAlias(id)
	if !ok {
		return "", fmt.Errorf("recall: %q is not a result id (use the c<N> shown in a placeholder)", id)
	}
	e, ok := a.arch.get(ord)
	if !ok {
		return "", fmt.Errorf("recall: no result %s in this conversation (results so far: c1–c%d)", aliasOf(ord), a.arch.count())
	}
	text := recallText(e)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	head := fmt.Sprintf("[%s: %s · %s]", e.alias(), callLabel(e), kib(len(text)))
	var b strings.Builder
	b.WriteString(head + "\n")
	if strings.TrimSpace(grep) != "" {
		rx, err := regexp.Compile("(?i)" + grep)
		if err != nil {
			rx = regexp.MustCompile("(?i)" + regexp.QuoteMeta(grep))
		}
		matched, shown := 0, 0
		for i, l := range lines {
			if !rx.MatchString(l) {
				continue
			}
			matched++
			row := fmt.Sprintf("%d: %s\n", i+1, clipBytes(l, 2000))
			if b.Len()+len(row) > recallCap {
				continue
			}
			b.WriteString(row)
			shown++
		}
		fmt.Fprintf(&b, "[%d of %d lines match %q", shown, matched, grep)
		if shown < matched {
			b.WriteString("; narrow the pattern to see the rest")
		}
		b.WriteString("]")
		return b.String(), nil
	}
	if startLine < 1 {
		startLine = 1
	}
	if startLine > len(lines) {
		return "", fmt.Errorf("recall: %s has %d lines; start_line %d is past the end", e.alias(), len(lines), startLine)
	}
	i := startLine - 1
	for ; i < len(lines); i++ {
		row := lines[i] + "\n"
		if b.Len()+len(row) > recallCap {
			if i == startLine-1 { // one enormous line: show what fits
				b.WriteString(clipBytes(lines[i], recallCap-b.Len()-200) + "\n")
				fmt.Fprintf(&b, "[line %d is %s long; only its first part is shown]\n", i+1, kib(len(lines[i])))
				i++
			}
			break
		}
		b.WriteString(row)
	}
	if i < len(lines) {
		fmt.Fprintf(&b, "[lines %d–%d of %d · recall(%q, %d) for more]", startLine, i, len(lines), e.alias(), i+1)
	} else {
		fmt.Fprintf(&b, "[lines %d–%d of %d · end]", startLine, len(lines), len(lines))
	}
	return b.String(), nil
}
