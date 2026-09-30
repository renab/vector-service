package migrate

import (
	"bytes"
	"fmt"
	"strings"
)

// Released migrations carry outer transaction framing: every released file
// begins with a BEGIN; line and ends with a COMMIT; line. The runner executes
// each migration in its own transaction, so the outer framing is stripped
// from the execution stream only. The checksum is always computed over the
// original embedded bytes; stripping never alters the checksum.
//
// A file is well-formed only if:
//
//  1. its first non-blank, non-comment line is exactly "BEGIN;"
//     (keyword case-insensitive, semicolon required);
//  2. its last non-blank, non-comment line is exactly "COMMIT;"
//     (keyword case-insensitive, semicolon required);
//  3. after removing exactly those two framing lines, the body contains no
//     other top-level transaction control statement (BEGIN, START
//     TRANSACTION, COMMIT, END, ROLLBACK, ABORT) outside $$ ... $$ quoted
//     bodies and single-quoted string literals.
//
// Malformed framing is fatal, not repaired: the file is rejected before any
// migration is applied, and a released file is never rewritten to fix
// framing.
const (
	clauseBegin   = "first non-blank, non-comment line is exactly BEGIN;"
	clauseCommit  = "last non-blank, non-comment line is exactly COMMIT;"
	clauseControl = "body contains a top-level transaction control statement outside the outer framing"
)

// framingError reports a migration file whose outer transaction framing
// violates the runner contract. It names the file and the violated clause.
type framingError struct {
	file   string
	clause string
	detail string
}

func (e *framingError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("migration %s has malformed transaction framing: %s", e.file, e.clause)
	}
	return fmt.Sprintf("migration %s has malformed transaction framing: %s (%s)", e.file, e.clause, e.detail)
}

// framingLine identifies one physical line of a migration file.
type framingLine struct {
	start int // offset of the first byte of the line
	end   int // offset just past the line terminator (or EOF)
}

// lineKind classifies a physical line for framing purposes.
type lineKind int

const (
	lineBlank   lineKind = iota // empty or whitespace-only
	lineComment                 // first non-blank byte is "--"
	lineContent
)

// lineSpan returns the span of the physical line containing offset i.
func lineSpan(raw []byte, i int) framingLine {
	start := i
	for start > 0 && raw[start-1] != '\n' {
		start--
	}
	end := start
	for end < len(raw) && raw[end] != '\n' {
		end++
	}
	if end < len(raw) {
		end++ // include the newline
	}
	return framingLine{start: start, end: end}
}

// allLines enumerates the physical lines of raw.
func allLines(raw []byte) []framingLine {
	var lines []framingLine
	for i := 0; i < len(raw); {
		line := lineSpan(raw, i)
		lines = append(lines, line)
		i = line.end
	}
	return lines
}

// classifyLine reports whether the line is blank, a line comment, or content.
func classifyLine(line []byte) lineKind {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return lineBlank
	}
	if bytes.HasPrefix(trimmed, []byte("--")) {
		return lineComment
	}
	return lineContent
}

// firstContentLine returns the first non-blank, non-comment line, or an
// error if the file has no content lines at all.
func firstContentLine(raw []byte, lines []framingLine) (framingLine, error) {
	for _, line := range lines {
		if classifyLine(raw[line.start:line.end]) == lineContent {
			return line, nil
		}
	}
	return framingLine{}, &framingError{file: "", clause: clauseBegin, detail: "file has no non-blank, non-comment line"}
}

// lastContentLine returns the last non-blank, non-comment line, or an error
// if the file has no content lines at all.
func lastContentLine(raw []byte, lines []framingLine) (framingLine, error) {
	for i := len(lines) - 1; i >= 0; i-- {
		if classifyLine(raw[lines[i].start:lines[i].end]) == lineContent {
			return lines[i], nil
		}
	}
	return framingLine{}, &framingError{file: "", clause: clauseCommit, detail: "file has no non-blank, non-comment line"}
}

// isFramingKeyword reports whether the line is exactly the given transaction
// keyword followed by a semicolon. The keyword is case-insensitive; optional
// whitespace around the keyword and before the semicolon is permitted, but
// nothing else (no transaction modes, no additional statement).
func isFramingKeyword(line []byte, keyword string) bool {
	trimmed := strings.TrimRight(string(bytes.TrimSpace(line)), "\r\n")
	if !strings.HasSuffix(trimmed, ";") {
		return false
	}
	body := strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	return strings.EqualFold(body, keyword)
}

// lineNo is the 1-based line number of a line span.
func lineNo(raw []byte, line framingLine) int {
	return 1 + bytes.Count(raw[:line.start], []byte{'\n'})
}

// stripFraming validates the outer BEGIN/COMMIT framing of raw and returns
// the execution stream: the original bytes minus exactly the two framing
// lines. Every other byte is preserved verbatim.
func stripFraming(file string, raw []byte) ([]byte, error) {
	lines := allLines(raw)

	first, err := firstContentLine(raw, lines)
	if err != nil {
		err.(*framingError).file = file
		return nil, err
	}
	if !isFramingKeyword(raw[first.start:first.end], "BEGIN") {
		return nil, &framingError{
			file:   file,
			clause: clauseBegin,
			detail: fmt.Sprintf("line %d is %q", lineNo(raw, first), quoteLine(raw[first.start:first.end])),
		}
	}

	last, err := lastContentLine(raw, lines)
	if err != nil {
		err.(*framingError).file = file
		return nil, err
	}
	if !isFramingKeyword(raw[last.start:last.end], "COMMIT") {
		return nil, &framingError{
			file:   file,
			clause: clauseCommit,
			detail: fmt.Sprintf("line %d is %q", lineNo(raw, last), quoteLine(raw[last.start:last.end])),
		}
	}

	body := make([]byte, 0, len(raw)-(last.end-first.start))
	body = append(body, raw[:first.start]...)
	body = append(body, raw[first.end:last.start]...)
	body = append(body, raw[last.end:]...)

	if err := scanTopLevelTransactionControl(file, body); err != nil {
		return nil, err
	}
	return body, nil
}

func quoteLine(b []byte) string {
	return fmt.Sprintf("%q", strings.TrimRight(string(b), "\r\n"))
}

// transaction control keywords that may not appear as top-level statements
// in the execution body. "START" is only transactional when followed by
// TRANSACTION or WORK.
var topLevelControl = map[string]bool{
	"BEGIN":    true,
	"COMMIT":   true,
	"END":      true,
	"ROLLBACK": true,
	"ABORT":    true,
}

var startTransaction = map[string]bool{
	"TRANSACTION": true,
	"WORK":        true,
}

// scanTopLevelTransactionControl reports an error if body contains a
// top-level transaction control statement outside $$ ... $$ quoted bodies,
// single-quoted string literals, and comments. Statement boundaries are the
// semicolons that the scanner observes in plain SQL text.
func scanTopLevelTransactionControl(file string, body []byte) error {
	i := 0
	n := len(body)
	var stmt []byte // plain SQL bytes of the current statement (comments,
	// string literals, and dollar-quoted bodies are not accumulated)
	stmtStart := 0 // offset in body where the current statement started

	// flush reports a violation if the completed statement is a top-level
	// transaction control statement, then starts a fresh statement at i.
	flush := func() error {
		if len(stmt) > 0 {
			if kw, second, ok := statementKeyword(stmt); ok && (topLevelControl[kw] || (kw == "START" && startTransaction[second])) {
				line := 1 + bytes.Count(body[:stmtStart], []byte{'\n'})
				return &framingError{
					file:   file,
					clause: clauseControl,
					detail: fmt.Sprintf("statement %q at line %d", string(stmt), line),
				}
			}
		}
		stmt = stmt[:0]
		stmtStart = i
		return nil
	}

	for i < n {
		c := body[i]
		switch {
		case c == '-' && i+1 < n && body[i+1] == '-':
			// Line comment: skipped through end of line; contributes no
			// statement bytes.
			i = skipLineComment(body, i)
		case c == '/' && i+1 < n && body[i+1] == '*':
			// Block comment (nestable): skipped; contributes no statement
			// bytes.
			end := skipBlockComment(body, i)
			if end < 0 {
				return fmt.Errorf("migration %s: unterminated block comment", file)
			}
			i = end
		case c == '\'':
			end, err := skipQuotedString(body, i)
			if err != nil {
				return fmt.Errorf("migration %s: %w", file, err)
			}
			i = end
		case c == '$':
			if q, ok := dollarQuoteStart(body, i); ok {
				end, err := skipDollarQuoted(body, i, q)
				if err != nil {
					return fmt.Errorf("migration %s: %w", file, err)
				}
				i = end
				break
			}
			stmt = append(stmt, c)
			i++
		default:
			stmt = append(stmt, c)
			i++
			if c == ';' {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}

	// A trailing statement without a terminating semicolon is not a complete
	// statement; the framing contract already requires the final content
	// line to be COMMIT;, so there is nothing further to flush.

	return nil
}

// skipLineComment returns the offset just past the end of the line comment
// starting at body[i] (body[i] == '-').
func skipLineComment(body []byte, i int) int {
	for i < len(body) && body[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment returns the offset just past the matching "*/" for the
// block comment starting at body[i] (body[i] == '/'). Block comments nest in
// PostgreSQL.
func skipBlockComment(body []byte, i int) int {
	depth := 0
	for i < len(body) {
		if body[i] == '/' && i+1 < len(body) && body[i+1] == '*' {
			depth++
			i += 2
			continue
		}
		if body[i] == '*' && i+1 < len(body) && body[i+1] == '/' {
			depth--
			i += 2
			if depth == 0 {
				return i
			}
			continue
		}
		i++
	}
	return -1
}

// skipQuotedString returns the offset just past the closing quote of the
// single-quoted string starting at body[i] (an opening single quote).
// Doubled single quotes inside the literal are escape sequences; when the
// string was introduced by E or e, backslash escapes are honored as well.
func skipQuotedString(body []byte, i int) (int, error) {
	// Detect the E'' escape-string form: the quote is introduced by an
	// immediately preceding 'E' or 'e'.
	escape := i > 0 && (body[i-1] == 'E' || body[i-1] == 'e')
	i++
	for i < len(body) {
		c := body[i]
		if c == '\'' {
			if i+1 < len(body) && body[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1, nil
		}
		if escape && c == '\\' && i+1 < len(body) {
			i += 2
			continue
		}
		i++
	}
	return 0, fmt.Errorf("unterminated string literal")
}

// dollarQuoteStart reports whether body[i] starts a dollar quote ("$$" or
// "$tag$"), returning the full delimiter including both "$" characters.
func dollarQuoteStart(body []byte, i int) (string, bool) {
	if body[i] != '$' {
		return "", false
	}
	if i+1 < len(body) && body[i+1] == '$' {
		return "$$", true
	}
	// $tag$: letters/digits after the first $, starting with a letter,
	// closed by $tag$.
	j := i + 1
	for j < len(body) && (body[j] == '_' || isLetterOrDigit(body[j])) {
		j++
	}
	if j > i+1 && j < len(body) && body[j] == '$' {
		tag := string(body[i+1 : j+1])
		if isLetter(body[i+1]) {
			return tag, true
		}
	}
	return "", false
}

func isLetterOrDigit(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// skipDollarQuoted returns the offset just past the closing delimiter of the
// dollar-quoted body starting at body[i] with delimiter q (including its "$"
// characters).
func skipDollarQuoted(body []byte, i int, q string) (int, error) {
	idx := bytes.Index(body[i+len(q):], []byte(q))
	if idx < 0 {
		return 0, fmt.Errorf("unterminated dollar-quoted body %q", q)
	}
	return i + len(q) + idx + len(q), nil
}

// statementKeyword splits the significant bytes of a completed statement
// into its leading keyword and second word (upper-cased).
func statementKeyword(stmt []byte) (string, string, bool) {
	trimmed := bytes.TrimSpace(stmt)
	trimmed = bytes.TrimSuffix(trimmed, []byte(";"))
	if len(trimmed) == 0 {
		return "", "", false
	}
	fields := strings.Fields(string(trimmed))
	if len(fields) == 0 {
		return "", "", false
	}
	second := ""
	if len(fields) > 1 {
		second = fields[1]
	}
	return strings.ToUpper(fields[0]), strings.ToUpper(second), true
}
