package resource

import (
	"bufio"
	"errors"
	"strconv"
	"strings"
)

// maxManageSieveLiteral bounds literal allocations so that a client cannot
// trigger large allocations before or after authentication.
const maxManageSieveLiteral = 1 << 20 // 1 MiB

// maxManageSieveCommandToken bounds non-literal command tokens so command lines
// cannot be grown without limit before authentication.
const maxManageSieveCommandToken = 64 << 10 // 64 KiB

var (
	errManageSieveLiteralTooLarge = errors.New("literal too large")
	errManageSieveCommandTooLarge = errors.New("command line too long")
)

// managesieveReader reads RFC 5804 commands, including quoted strings and
// synchronized or non-synchronized literals.
type managesieveReader struct {
	reader    *bufio.Reader
	writeLine func(string) error
}

// ReadCommand reads one complete command and returns its verb and arguments.
func (r *managesieveReader) ReadCommand() (string, []string, error) {
	tokens, err := r.readTokens()
	if err != nil {
		return "", nil, err
	}
	if len(tokens) == 0 {
		return "", nil, errors.New("empty command")
	}
	return tokens[0], tokens[1:], nil
}

func (r *managesieveReader) readTokens() ([]string, error) {
	tokens := []string{}
	for {
		if err := r.skipSpaces(); err != nil {
			return nil, err
		}
		b, err := r.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == '\r' {
			next, err := r.reader.ReadByte()
			if err != nil {
				return nil, err
			}
			if next != '\n' {
				return nil, errors.New("expected LF after CR")
			}
			return tokens, nil
		}
		_ = r.reader.UnreadByte()

		token, err := r.readToken()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
}

func (r *managesieveReader) skipSpaces() error {
	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			return err
		}
		if b != ' ' && b != '\t' {
			return r.reader.UnreadByte()
		}
	}
}

func (r *managesieveReader) readToken() (string, error) {
	b, err := r.reader.ReadByte()
	if err != nil {
		return "", err
	}
	switch b {
	case '"':
		return r.readQuoted()
	case '{':
		return r.readLiteral()
	default:
		var builder strings.Builder
		builder.WriteByte(b)
		for {
			ch, err := r.reader.ReadByte()
			if err != nil {
				return "", err
			}
			if ch == ' ' || ch == '\t' || ch == '\r' {
				_ = r.reader.UnreadByte()
				return builder.String(), nil
			}
			if builder.Len() >= maxManageSieveCommandToken {
				return "", errManageSieveCommandTooLarge
			}
			builder.WriteByte(ch)
		}
	}
}

func (r *managesieveReader) readQuoted() (string, error) {
	var builder strings.Builder
	for {
		ch, err := r.reader.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == '"' {
			return builder.String(), nil
		}
		if ch == '\\' {
			escaped, err := r.reader.ReadByte()
			if err != nil {
				return "", err
			}
			if builder.Len() >= maxManageSieveLiteral {
				return "", errManageSieveLiteralTooLarge
			}
			builder.WriteByte(escaped)
			continue
		}
		if ch == '\r' {
			return "", errors.New("unterminated quoted string")
		}
		if builder.Len() >= maxManageSieveLiteral {
			return "", errManageSieveLiteralTooLarge
		}
		builder.WriteByte(ch)
	}
}

func (r *managesieveReader) readLiteral() (string, error) {
	var numberBuilder strings.Builder
	for {
		ch, err := r.reader.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == '}' {
			break
		}
		if numberBuilder.Len() > 20 {
			return "", errManageSieveCommandTooLarge
		}
		numberBuilder.WriteByte(ch)
	}

	spec := numberBuilder.String()
	nonSynchronized := strings.HasSuffix(spec, "+")
	if nonSynchronized {
		spec = strings.TrimSuffix(spec, "+")
	}
	if len(spec) > 20 {
		return "", errManageSieveCommandTooLarge
	}
	length, err := strconv.Atoi(strings.TrimSpace(spec))
	if err != nil || length < 0 {
		return "", errors.New("invalid literal length")
	}
	if length > maxManageSieveLiteral {
		return "", errManageSieveLiteralTooLarge
	}

	// The literal marker ends the line: consume its trailing CRLF.
	if newline, newlineErr := r.reader.ReadByte(); newlineErr != nil || newline != '\r' {
		if newlineErr == nil {
			_ = r.reader.UnreadByte()
		}
	} else if lineEnd, lineEndErr := r.reader.ReadByte(); lineEndErr != nil || lineEnd != '\n' {
		if lineEndErr == nil {
			_ = r.reader.UnreadByte()
		}
	}

	// Synchronized literals wait for a server continuation request.
	if !nonSynchronized && r.writeLine != nil {
		if err := r.writeLine("OK Ready for literal data"); err != nil {
			return "", err
		}
	}

	buffer := make([]byte, length)
	if _, err := readFull(r.reader, buffer); err != nil {
		return "", err
	}
	return string(buffer), nil
}

func readFull(reader *bufio.Reader, buffer []byte) (int, error) {
	read := 0
	for read < len(buffer) {
		count, err := reader.Read(buffer[read:])
		read += count
		if err != nil {
			return read, err
		}
	}
	return read, nil
}
